package control

import (
	"context"
	"cove/internal/buildinfo"
	"cove/internal/worker"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

func (s *Server) observePorts(ctx context.Context, id string, containers []worker.Container) error {
	exposed := false
	for _, c := range containers {
		if c.ExposedPorts || strings.Contains(c.Ports, "0.0.0.0:") || strings.Contains(c.Ports, "[::]:") || strings.Contains(c.Ports, ":::") {
			exposed = true
		}
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO workload_security_observations(workload_id,exposed,attempted_at) VALUES($1,$2,now()) ON CONFLICT(workload_id) DO UPDATE SET exposed=$2,observed_at=now(),attempted_at=now()`, id, exposed)
	return err
}

func (s *Server) refreshPortExposure(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT w.id,w.node_id FROM workloads w LEFT JOIN workload_security_observations o ON o.workload_id=w.id WHERE COALESCE(o.attempted_at,o.observed_at) IS NULL OR COALESCE(o.attempted_at,o.observed_at)<now()-interval '5 minutes' ORDER BY COALESCE(o.attempted_at,o.observed_at) NULLS FIRST LIMIT 8`)
	if err != nil {
		return err
	}
	type item struct{ id, node string }
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.node); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	jobs := make(chan item, len(items))
	for _, i := range items {
		jobs <- i
	}
	close(jobs)
	failures := make(chan error, len(items))
	var group sync.WaitGroup
	for workerIndex := 0; workerIndex < 4; workerIndex++ {
		group.Go(func() {
			for i := range jobs {
				response, e := s.nodeRPC(scanCtx, i.node, worker.RPCRequest{Method: "containers", ID: i.id})
				if e != nil {
					_, e = s.DB.Exec(ctx, `INSERT INTO workload_security_observations(workload_id,exposed,observed_at,attempted_at) VALUES($1,false,NULL,now()) ON CONFLICT(workload_id) DO UPDATE SET attempted_at=now()`, i.id)
				} else {
					e = s.observePorts(ctx, i.id, response.Containers)
				}
				if e != nil {
					failures <- e
				}
			}
		})
	}
	group.Wait()
	close(failures)
	for e := range failures {
		return e
	}

	return nil
}

func (s *Server) reportUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare(tokenHash(bearer(r)), tokenHash(s.AdminToken)) != 1 {
		http.Error(w, "host administrator token required", 403)
		return
	}
	var in struct {
		Version      string    `json:"version"`
		Channel      string    `json:"channel"`
		Revision     string    `json:"revision"`
		Schema       string    `json:"schema_hash"`
		RevisionTime time.Time `json:"revision_time"`
		Root         string    `json:"source_root"`
		Generation   int64     `json:"source_generation"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Channel != "beta" && in.Channel != "release" {
		http.Error(w, "channel required", 400)
		return
	}
	if (in.Channel == "beta") != strings.HasPrefix(in.Version, "beta-") {
		http.Error(w, "version does not match channel", 400)
		return
	}
	if !regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?|beta-[a-f0-9]{40})$`).MatchString(in.Version) {
		http.Error(w, "verified release version required", 400)
		return
	}
	if in.Root != "" || in.Generation != 0 {
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(in.Root) || in.Generation < 1 {
			http.Error(w, "valid signed source lineage required", 400)
			return
		}
	}
	if in.Schema != "" || in.Revision != "" || !in.RevisionTime.IsZero() {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(in.Schema) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(in.Revision) || in.RevisionTime.IsZero() {
			http.Error(w, "complete verified image metadata required", 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "update observation unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), "UPDATE control_settings SET latest_panel_version=$1,panel_update_checked_at=now(),panel_update_channel=$2 WHERE id=1", in.Version, in.Channel)
	if err == nil && in.Schema != "" {
		_, err = tx.Exec(r.Context(), `INSERT INTO panel_channel_observations(channel,version,revision,schema_hash,revision_time,source_root,source_generation) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(channel) DO UPDATE SET version=$2,revision=$3,schema_hash=$4,revision_time=$5,source_root=$6,source_generation=$7,checked_at=now()`, in.Channel, in.Version, in.Revision, in.Schema, in.RevisionTime, in.Root, in.Generation)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "update observation unavailable", 503)
		return
	}

	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) observedPosture(ctx context.Context, add func(string, bool, string, string)) error {
	var backup, updates, ports bool
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(panel_backup_at>now()-interval '48 hours',false),COALESCE(panel_update_checked_at>now()-interval '2 hours' AND latest_panel_version=$1 AND panel_update_channel=update_channel,false),NOT EXISTS(SELECT 1 FROM workloads w LEFT JOIN workload_security_observations o ON o.workload_id=w.id WHERE o.exposed OR o.observed_at IS NULL OR o.observed_at<now()-interval '15 minutes') FROM control_settings WHERE id=1`, buildinfo.Version).Scan(&backup, &updates, &ports); err != nil {
		return err
	}

	add("panel_backup", backup, "Encrypted panel backup repository was verified by the host backup script within 48 hours.", "Run and schedule scripts/backup-panel.py; retain off-host copies and restore-test them. The receipt proves a successful repository check, not off-host durability.")
	add("updates", updates, "The host updater verified the selected channel within two hours and its version matches this panel.", "Run the host updater with --latest and check its timer. A missing or stale successful registry check needs attention.")
	add("port_exposure", ports, "Every workload was inspected within 15 minutes and no all-interface Docker port publication was found.", "Review workload runtime/preflight and bind exposed ports to a deliberate address. Inspect shared ingress HTTP, HTTP/3, TCP and UDP listeners separately on each node and review Host firewall; listener changes never open firewall rules. Resolve unreachable workers so the bounded fleet scanner can inspect them.")
	rows, err := s.DB.Query(ctx, "SELECT capabilities,last_checked_at>now()-interval '15 minutes' FROM nodes WHERE state<>'pending'")
	if err != nil {
		return err
	}
	defer rows.Close()
	current := true
	supported := true
	for rows.Next() {
		var raw []byte
		var recent *bool
		if err = rows.Scan(&raw, &recent); err != nil {
			return err
		}
		var p worker.Probe
		if json.Unmarshal(raw, &p) != nil || recent == nil || !*recent {
			current = false
			continue
		}
		if p.Arch != "amd64" && p.Arch != "arm64" {
			current = false
			continue
		}
		data, e := os.ReadFile(filepath.Join(s.BinaryDir, "cove-worker-linux-"+p.Arch))
		if e != nil {
			supported = false
			continue
		}
		if p.BinarySHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
			current = false
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if supported {
		add("worker_updates", current, "Recently probed workers match the binaries bundled with this panel.", "Upgrade workers using their update timers, then verify them. Old workers without a binary checksum do not pass.")
	}
	return nil
}
