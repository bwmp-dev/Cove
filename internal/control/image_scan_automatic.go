package control

import (
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) runAutomaticImageScans(ctx context.Context) {
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()
	for {
		if err := s.reconcileAutomaticScans(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Automatic vulnerability scan reconciliation is unavailable; it will retry without holding deployment slots.")
		}
		if err := s.scheduleAutomaticScans(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Automatic vulnerability scan scheduling is unavailable; it will retry without holding deployment slots.")
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

const automaticScanEligible = `w.kind IN ('service','job') AND NOT w.manually_stopped AND n.state='ready'
 AND COALESCE((n.capabilities->>'automatic_image_scans')::boolean,false)
 AND COALESCE((n.capabilities->>'image_scanning')::boolean,false)
 AND EXISTS(SELECT 1 FROM image_scanning_settings c WHERE c.team_id=w.team_id AND c.node_id=w.node_id AND c.enabled AND c.automatic)
 AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused)
 AND NOT EXISTS(SELECT 1 FROM environment_suspensions p WHERE p.workload_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running'))`

type automaticScanCandidate struct {
	id, node, kind string
	configuration  []byte
}

func (s *Server) scheduleAutomaticScans(ctx context.Context) error {
	if _, err := s.DB.Exec(ctx, `INSERT INTO image_scan_automation_state(workload_id)
 SELECT w.id FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE `+automaticScanEligible+`
 AND EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','rollback') AND o.state='succeeded') FOR KEY SHARE OF w ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `UPDATE image_scan_automation_state a SET next_check_at=now()+interval '1 minute'
 WHERE a.workload_id IN (SELECT w.id FROM workloads w JOIN nodes n ON n.id=w.node_id JOIN image_scan_automation_state a ON a.workload_id=w.id
 WHERE `+automaticScanEligible+` AND a.next_check_at<=now()
 AND NOT EXISTS(SELECT 1 FROM automatic_image_scans p WHERE p.node_id=w.node_id)
 AND NOT EXISTS(SELECT 1 FROM operations o JOIN workloads m ON m.id=o.workload_id WHERE m.node_id=w.node_id AND o.action='image_scan' AND o.state IN ('queued','running'))
 ORDER BY a.next_check_at,w.id LIMIT GREATEST(0,4-(SELECT count(*) FROM automatic_image_scans)) FOR UPDATE OF a SKIP LOCKED)
 RETURNING a.workload_id,(SELECT node_id FROM workloads WHERE id=a.workload_id),(SELECT kind FROM workloads WHERE id=a.workload_id),(SELECT configuration FROM workloads WHERE id=a.workload_id)`)
	if err != nil {
		return err
	}
	candidates := []automaticScanCandidate{}
	for rows.Next() {
		var candidate automaticScanCandidate
		if err = rows.Scan(&candidate.id, &candidate.node, &candidate.kind, &candidate.configuration); err != nil {
			break
		}
		candidates = append(candidates, candidate)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err = s.checkAutomaticScan(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func scanImages(report *worker.ImageScan) map[string][]string {
	images := map[string][]string{}
	if report == nil {
		return images
	}
	seen := map[string]bool{}
	for _, image := range report.Images {
		if image.Identity == "" {
			return map[string][]string{}
		}
		if !seen[image.Service+"/"+image.Identity] {
			images[image.Service] = append(images[image.Service], image.Identity)
			seen[image.Service+"/"+image.Identity] = true
		}
	}
	for service := range images {
		sort.Strings(images[service])
	}
	return images
}

func (s *Server) automaticScanMessage(ctx context.Context, id, message string) error {
	_, err := s.DB.Exec(ctx, `UPDATE image_scan_automation_state SET last_error=$2 WHERE workload_id=$1`, id, message)
	return err
}

func (s *Server) checkAutomaticScan(ctx context.Context, candidate automaticScanCandidate) error {
	request := worker.Request{ID: resourceID(), Workload: candidate.id, Action: "image_scan", Kind: candidate.kind}
	data, err := unseal(s.Key, candidate.configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		return s.automaticScanMessage(ctx, candidate.id, "Resource configuration is unavailable; no automatic scan was queued.")
	}
	request.JobService = config.JobService
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	database, callErr := s.nodeRPC(call, candidate.node, worker.RPCRequest{Method: "image_scan_database", RefreshScanDatabase: true})
	cancel()
	if callErr != nil || database.Error != "" || database.ScanDatabase == nil || !database.ScanDatabase.Fresh || !database.ScanDatabase.Database.Fresh(time.Now().UTC()) {
		return s.automaticScanMessage(ctx, candidate.id, "Vulnerability database coverage is unknown. A bounded refresh may be pending or cooling down; no scan was queued.")
	}
	call, cancel = context.WithTimeout(ctx, 15*time.Second)
	observation, callErr := s.nodeRPC(call, candidate.node, worker.RPCRequest{Method: "image_scan_observe", Request: &request})
	cancel()
	if callErr != nil || observation.Error != "" || observation.ScanObservation == nil || !observation.ScanObservation.Eligible {
		return s.automaticScanMessage(ctx, candidate.id, "The resource is stopped, paused, sleeping, busy or its retained images are unavailable; no scan was queued.")
	}
	request.AutomaticScan = &worker.AutomaticScan{Origin: "scheduled", Database: database.ScanDatabase.Database.Digest, Images: observation.ScanObservation.Images}
	if worker.ValidateRequest(request) != nil {
		return s.automaticScanMessage(ctx, candidate.id, "Complete retained image identities are unavailable; no scan was queued.")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var eligible bool
	var node, lastKey string
	if err = tx.QueryRow(ctx, `SELECT `+automaticScanEligible+`,w.node_id,a.attempt_key FROM workloads w JOIN nodes n ON n.id=w.node_id JOIN image_scan_automation_state a ON a.workload_id=w.id WHERE w.id=$1 FOR UPDATE OF w,a`, candidate.id).Scan(&eligible, &node, &lastKey); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if !eligible || node != candidate.node {
		return nil
	}
	var deployment, state string
	err = tx.QueryRow(ctx, `SELECT id,state FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback') AND ($2='job' OR state='succeeded') ORDER BY created_at DESC,id DESC LIMIT 1`, candidate.id, candidate.kind).Scan(&deployment, &state)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "succeeded" {
		return nil
	}
	if err != nil {
		return err
	}
	var previous []byte
	err = tx.QueryRow(ctx, `SELECT result FROM operations WHERE workload_id=$1 AND action='image_scan' AND state='succeeded' ORDER BY created_at DESC,id DESC LIMIT 1`, candidate.id).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var prior worker.Operation
	if len(previous) <= 2*1024*1024 && len(previous) > 0 {
		if err := json.Unmarshal(previous, &prior); err != nil {
			prior = worker.Operation{}
		}
	}
	report := prior.ImageScan
	if report != nil && report.Database.Fresh(time.Now().UTC()) && report.Database.Digest == request.AutomaticScan.Database && !report.CheckedAt.IsZero() && !report.CheckedAt.After(time.Now().UTC()) && reflect.DeepEqual(scanImages(report), request.AutomaticScan.Images) {
		_, err = tx.Exec(ctx, `UPDATE image_scan_automation_state SET last_error='' WHERE workload_id=$1`, candidate.id)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if report == nil || !reflect.DeepEqual(scanImages(report), request.AutomaticScan.Images) {
		request.AutomaticScan.Origin = "post_deploy"
	}
	identity, _ := json.Marshal(struct {
		Images            map[string][]string
		Database, Scanner string
	}{request.AutomaticScan.Images, request.AutomaticScan.Database, worker.ScannerImage})
	digest := sha256.Sum256(identity)
	key := hex.EncodeToString(digest[:])
	if lastKey == key {
		return nil
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO automatic_image_scans(node_id,id,workload_id,deployment_id,payload,attempt_key) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, candidate.node, request.ID, candidate.id, deployment, payload, key)
	if err != nil || inserted.RowsAffected() == 0 {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE image_scan_automation_state SET last_error='' WHERE workload_id=$1`, candidate.id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) reconcileAutomaticScans(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT node_id,id,workload_id,deployment_id,payload,attempt_key,created_at,cancel_requested FROM automatic_image_scans ORDER BY created_at LIMIT 4`)
	if err != nil {
		return err
	}
	type lease struct {
		node, id, workload, deployment, key string
		payload                             []byte
		created                             time.Time
		cancel                              bool
	}
	leases := []lease{}
	for rows.Next() {
		var item lease
		if err = rows.Scan(&item.node, &item.id, &item.workload, &item.deployment, &item.payload, &item.key, &item.created, &item.cancel); err != nil {
			break
		}
		leases = append(leases, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range leases {
		plain, err := unseal(s.Key, item.payload)
		var request worker.Request
		if err != nil || json.Unmarshal(plain, &request) != nil || request.AutomaticScan == nil || request.ID != item.id || request.Workload != item.workload || worker.ValidateRequest(request) != nil {
			return errors.New("automatic scan lease input is invalid")
		}
		var eligible bool
		err = s.DB.QueryRow(ctx, `SELECT `+automaticScanEligible+` AND w.node_id=$2 AND (SELECT id FROM operations WHERE workload_id=w.id AND action IN ('deploy','rollback') AND (w.kind='job' OR state='succeeded') ORDER BY created_at DESC,id DESC LIMIT 1)=$3 FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, item.workload, item.node, item.deployment).Scan(&eligible)
		if errors.Is(err, pgx.ErrNoRows) {
			eligible, err = false, nil
		}
		if err != nil {
			return err
		}
		method := "submit_automatic_image_scan"
		if item.cancel || !eligible {
			method = "cancel"
		}
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		response, callErr := s.nodeRPC(call, item.node, worker.RPCRequest{Method: method, Request: &request})
		cancel()
		if callErr != nil || response.Error != "" || response.Operation == nil {
			continue
		}
		op := response.Operation
		if op.ID != item.id || op.Workload != item.workload || op.Action != "image_scan" || op.State == "queued" || op.State == "running" {
			continue
		}
		if op.State != "succeeded" && op.State != "failed" && op.State != "interrupted" && op.State != "cancelled" {
			continue
		}
		result, err := json.Marshal(op)
		if err != nil || len(result) > 2*1024*1024 {
			return errors.New("automatic scan result exceeds its bound")
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		if present, lockErr := lockObservationScope(ctx, tx, item.node, item.workload); lockErr != nil || !present {
			_ = tx.Rollback(ctx)
			if lockErr != nil {
				return lockErr
			}
			continue
		}
		var liveLease string
		leaseErr := tx.QueryRow(ctx, `SELECT id FROM automatic_image_scans WHERE id=$1 AND workload_id=$2 FOR UPDATE`, item.id, item.workload).Scan(&liveLease)
		if leaseErr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(leaseErr, pgx.ErrNoRows) {
				continue
			}
			return leaseErr
		}
		_, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,state,payload,result,created_at,execution_node_id) VALUES($1,$2,'image_scan',$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, item.id, item.workload, op.State, item.payload, result, item.created, item.node)
		if err == nil {
			key := item.key
			if op.State == "cancelled" || op.State == "interrupted" {
				key = ""
			}
			message := "The automatic scan did not provide complete coverage; inspect the result. It will not repeat for the same image/database identity."
			if op.State == "cancelled" || op.State == "interrupted" {
				message = "The automatic scan yielded or was interrupted. A later eligible check may retry without holding a deployment slot."
			} else if op.State == "succeeded" && op.ImageScan != nil && op.ImageScan.Complete(time.Now().UTC()) {
				message = ""
			}
			_, err = tx.Exec(ctx, `UPDATE image_scan_automation_state SET attempt_key=$2,last_error=$3 WHERE workload_id=$1`, item.workload, key, message)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM automatic_image_scans WHERE id=$1`, item.id)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) retireRemoteScans(ctx context.Context, tx pgx.Tx, workload, runtime string) error {
	rows, err := tx.Query(ctx, `SELECT execution_node_id,payload FROM operations WHERE workload_id=$1 AND action='image_scan' AND execution_node_id IS NOT NULL AND execution_node_id<>$2`, workload, runtime)
	if err != nil {
		return err
	}
	type saved struct {
		node    string
		payload []byte
	}
	requests := []saved{}
	for rows.Next() {
		var item saved
		if err = rows.Scan(&item.node, &item.payload); err != nil {
			break
		}
		requests = append(requests, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range requests {
		var request worker.Request
		if err := s.unsealJSON(item.payload, &request); err != nil {
			return err
		}
		response, err := s.nodeRPC(ctx, item.node, worker.RPCRequest{Method: "retire_image_scan", Request: &request})
		if err != nil {
			return err
		}
		if response.Error != "" {
			return errors.New("recorded automatic scan cleanup is pending")
		}
	}
	return nil
}
