package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type identityConfiguration struct {
	Old     string `json:"old"`
	New     string `json:"new"`
	OldSeed []byte `json:"old_seed"`
	NewSeed []byte `json:"new_seed"`
}

func (s *Server) nodeIdentity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		var rotation, phase, message string
		var updated *time.Time
		err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(i.id,''),COALESCE(i.phase,'idle'),COALESCE(i.last_error,''),i.updated_at FROM nodes n LEFT JOIN node_identity_rotations i ON i.node_id=n.id WHERE n.id=$1`, id).Scan(&rotation, &phase, &message, &updated)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "SSH identity status unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"id": rotation, "state": phase, "error": message, "updated_at": updated})
		return
	}
	var in struct {
		Action  string `json:"action"`
		Confirm bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm || in.Action != "rotate" && in.Action != "retry" && in.Action != "cancel" {
		http.Error(w, "confirm rotate, retry or cancel", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var public, mode string
	var capable, busy bool
	var encrypted []byte
	err = tx.QueryRow(r.Context(), `SELECT public_key,private_key,mode,host_key IS NOT NULL AND COALESCE((capabilities->>'ssh_identity')::boolean,false),EXISTS(SELECT 1 FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.node_id=n.id AND o.state IN ('queued','running')) FROM nodes n WHERE id=$1 FOR UPDATE`, id).Scan(&public, &encrypted, &mode, &capable, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node identity unavailable", 503)
		return
	}
	if mode != "ssh" || !capable || busy {
		http.Error(w, "verify an upgraded SSH worker and wait for active operations", 409)
		return
	}
	var rotation, phase string
	err = tx.QueryRow(r.Context(), `SELECT id,phase FROM node_identity_rotations WHERE node_id=$1 FOR UPDATE`, id).Scan(&rotation, &phase)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "identity rotation unavailable", 503)
		return
	}
	if in.Action != "rotate" {
		if phase == "" || phase == "succeeded" || phase == "cancelled" || in.Action == "cancel" && phase == "switched" {
			http.Error(w, "this rotation cannot accept that action; a switched identity must finish cleanup", 409)
			return
		}
		if in.Action == "cancel" {
			phase = "aborting"
		}
		_, err = tx.Exec(r.Context(), `UPDATE node_identity_rotations SET phase=$2,last_error='',next_attempt_at=now(),updated_at=now() WHERE node_id=$1`, id, phase)
	} else {
		if phase != "" && phase != "succeeded" && phase != "cancelled" {
			http.Error(w, "finish or cancel the current identity rotation first", 409)
			return
		}
		newPublic, newSeed, keyErr := keyPair()
		if keyErr != nil {
			http.Error(w, "key generation unavailable", 503)
			return
		}
		oldSeed, keyErr := unseal(s.Key, encrypted)
		if keyErr != nil {
			http.Error(w, "current identity unavailable", 503)
			return
		}
		config := identityConfiguration{Old: public, New: newPublic, OldSeed: oldSeed, NewSeed: newSeed}
		data, keyErr := json.Marshal(config)
		if keyErr == nil {
			data, keyErr = seal(s.Key, data)
		}
		if keyErr != nil {
			http.Error(w, "key protection unavailable", 503)
			return
		}
		rotation, phase = resourceID(), "queued"
		_, err = tx.Exec(r.Context(), `INSERT INTO node_identity_rotations(node_id,id,phase,configuration) VALUES($1,$2,'queued',$3) ON CONFLICT(node_id) DO UPDATE SET id=$2,phase='queued',configuration=$3,last_error='',next_attempt_at=now(),updated_at=now()`, id, rotation, data)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save identity intent", 503)
		return
	}
	respond(w, 202, map[string]string{"id": rotation, "state": phase})
}

func (s *Server) processIdentity(ctx context.Context, node string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var id, phase, current string
	var encrypted []byte
	err = tx.QueryRow(ctx, `SELECT i.id,i.phase,i.configuration,n.public_key FROM node_identity_rotations i JOIN nodes n ON n.id=i.node_id WHERE i.node_id=$1 AND i.phase NOT IN ('succeeded','cancelled') AND i.next_attempt_at<=now() FOR UPDATE OF i,n SKIP LOCKED`, node).Scan(&id, &phase, &encrypted, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	pending := func(message string) error {
		if _, err := tx.Exec(ctx, `UPDATE node_identity_rotations SET last_error=$2,next_attempt_at=now()+interval '1 minute',updated_at=now() WHERE node_id=$1`, node, message); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var config identityConfiguration
	data, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(data, &config) != nil {
		return pending("Pending SSH identity could not be decrypted; restore the panel encryption key before retrying.")
	}
	if phase != "switched" && current != config.Old || phase == "switched" && current != config.New {
		return pending("The node identity changed outside this pending rotation; reconcile its saved identity before retrying.")
	}
	request := worker.IdentityRequest{ID: id, Old: config.Old, New: config.New, Action: "stage"}
	next, message := phase, ""
	switch phase {
	case "queued", "aborting":
		if phase == "aborting" {
			request.Action = "abort"
		}
		response, callErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "identity", Identity: &request})
		if callErr != nil || response.Identity == nil || response.Identity.ID != id || phase == "queued" && response.Identity.State != "staged" || phase == "aborting" && response.Identity.State != "aborted" {
			message = "Could not confirm the worker key change. The old identity remains saved; check connectivity and retry or cancel."
		} else if phase == "aborting" {
			next = "cancelled"
		} else {
			next = "staged"
		}
	case "staged", "switched":
		client, callErr := s.openNodeSSH(ctx, node, config.NewSeed)
		if callErr != nil && phase == "switched" {
			oldClient, oldErr := s.openNodeSSH(ctx, node, config.OldSeed)
			if oldErr == nil {
				response, repairErr := callSSHClient(oldClient, worker.RPCRequest{Method: "identity", Identity: &request})
				oldClient.Close()
				if repairErr == nil && response.Identity != nil && response.Identity.ID == id {
					client, callErr = s.openNodeSSH(ctx, node, config.NewSeed)
				}
			}
		}
		if callErr != nil {
			message = "The new SSH identity could not authenticate. Check SSH configuration and retry; no saved private key is returned."
		} else {
			defer client.Close()
			probe, probeErr := callSSHClient(client, worker.RPCRequest{Method: "probe"})
			if probeErr != nil || probe.Probe == nil || probe.Probe.Protocol != 1 {
				message = "The new SSH connection did not confirm the restricted worker RPC. Check the worker and retry."
			} else if phase == "staged" {
				key, keyErr := seal(s.Key, config.NewSeed)
				if keyErr != nil {
					return keyErr
				}
				if _, err = tx.Exec(ctx, `UPDATE nodes SET public_key=$2,private_key=$3 WHERE id=$1`, node, config.New, key); err != nil {
					return err
				}
				next = "switched"
			} else {
				request.Action = "commit"
				response, commitErr := callSSHClient(client, worker.RPCRequest{Method: "identity", Identity: &request})
				if commitErr != nil || response.Identity == nil || response.Identity.ID != id || response.Identity.State != "committed" {
					message = "The new identity is saved and verified; removal of the old key still needs confirmation. Retry cleanup."
				} else {
					next = "succeeded"
				}
			}
		}
	default:
		return errors.New("invalid SSH identity phase")
	}
	terminal := next == "succeeded" || next == "cancelled"
	_, err = tx.Exec(ctx, `UPDATE node_identity_rotations SET phase=$2,last_error=$3,updated_at=now(),next_attempt_at=now()+CASE WHEN $3='' THEN interval '0 seconds' ELSE interval '1 minute' END,configuration=CASE WHEN $4 THEN NULL ELSE configuration END WHERE node_id=$1`, node, next, message, terminal)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) reconcileIdentities(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT node_id FROM node_identity_rotations WHERE phase NOT IN ('succeeded','cancelled') AND next_attempt_at<=now() ORDER BY next_attempt_at LIMIT 4`)
	if err != nil {
		return err
	}
	var nodes []string
	for rows.Next() {
		var node string
		if err = rows.Scan(&node); err != nil {
			break
		}
		nodes = append(nodes, node)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var group sync.WaitGroup
	failures := make(chan error, len(nodes))
	for _, node := range nodes {
		group.Go(func() {
			if err := s.processIdentity(ctx, node); err != nil {
				failures <- err
			}
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}
