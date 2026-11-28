package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

func (s *Server) gitDeployKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method != "GET" && (!s.requireAccess(w, r, "workload", id, 3) || !s.requirePersistentResource(w, r, id)) {
		return
	}
	if r.Method == "GET" {
		if !s.requireAccess(w, r, "workload", id, 1) {
			return
		}
		s.gitDeployKeyPublic(w, r)
		return
	}

	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var configuration []byte
	var active, managed bool
	if err = tx.QueryRow(r.Context(), `SELECT configuration,EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running')),(EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1) OR EXISTS(SELECT 1 FROM git_sources WHERE workload_id=$1)) FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&configuration, &active, &managed); err != nil {
		http.Error(w, "workload unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before changing the deploy key", 409)
		return
	}
	if r.Method == "DELETE" {
		if _, err = tx.Exec(r.Context(), `DELETE FROM git_deploy_keys WHERE workload_id=$1`, id); err != nil {
			http.Error(w, "could not remove deploy key", 503)
			return
		}
	} else {
		if managed {
			http.Error(w, "disconnect the managed Git provider before configuring an SSH deploy key", 409)
			return
		}
		var in struct {
			Repository string `json:"repository"`
			HostKey    string `json:"host_key"`
		}
		if !decode(w, r, &in) {
			return
		}
		var config workloadInput
		plain, err := unseal(s.Key, configuration)
		if err != nil || json.Unmarshal(plain, &config) != nil || config.Source == nil {
			http.Error(w, "save a Git source before creating a deploy key", 409)
			return
		}
		if !strings.HasPrefix(in.Repository, "ssh://") {
			http.Error(w, "an ssh:// repository URL is required", 400)
			return
		}
		sameRepository := config.Source.Repository == in.Repository
		config.Source.Repository, config.Source.SSHHostKey, config.Source.Token = in.Repository, in.HostKey, ""
		if err = validateWorkload(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		public, seed, err := keyPair()
		if err != nil {
			http.Error(w, "key generation failed", 500)
			return
		}
		encrypted, err := seal(s.Key, seed)
		if err != nil {
			http.Error(w, "could not protect deploy key", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO git_deploy_keys(workload_id,repository,host_key,public_key,private_key) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workload_id) DO UPDATE SET repository=EXCLUDED.repository,host_key=EXCLUDED.host_key,public_key=EXCLUDED.public_key,private_key=EXCLUDED.private_key`, id, in.Repository, in.HostKey, public, encrypted); err != nil {
			http.Error(w, "could not save deploy key", 503)
			return
		}
		saved, err := s.sealPreview(config)
		if err != nil {
			http.Error(w, "could not protect Git configuration", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2,webhook_secret=CASE WHEN $3 THEN webhook_secret ELSE NULL END WHERE id=$1`, id, saved, sameRepository); err != nil {
			http.Error(w, "could not save Git configuration", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL AND NOT $2`, id, sameRepository); err != nil {
			http.Error(w, "could not clear queued source triggers", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save deploy key", 503)
		return
	}
	if r.Method == "DELETE" {
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	// Read back only the public identity; private key material never leaves deployment preparation.
	s.gitDeployKeyPublic(w, r)
}

func (s *Server) gitDeployKeyPublic(w http.ResponseWriter, r *http.Request) {
	var public, repository, host string
	if err := s.DB.QueryRow(r.Context(), `SELECT public_key,repository,host_key FROM git_deploy_keys WHERE workload_id=$1`, r.PathValue("id")).Scan(&public, &repository, &host); err != nil {
		if errors.Is(err, pgx.ErrNoRows) && r.Method == "GET" {
			respond(w, 200, nil)
			return
		}
		http.Error(w, "deploy key unavailable", 503)
		return
	}
	status := 201
	if r.Method == "GET" {
		status = 200
	}
	respond(w, status, map[string]string{"public_key": public, "repository": repository, "host_key": host})
}

func (s *Server) prepareGitDeployKey(ctx context.Context, request *worker.Request) error {
	if request.Source.SSHPrivateKey != "" {
		return nil
	}
	var encrypted []byte
	var host string
	if err := s.DB.QueryRow(ctx, `SELECT private_key,host_key FROM git_deploy_keys WHERE workload_id=$1 AND repository=$2`, request.Workload, request.Source.Repository).Scan(&encrypted, &host); err != nil {
		return errors.New("SSH deploy key is unavailable for this repository")
	}
	seed, err := unseal(s.Key, encrypted)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("SSH deploy key could not be decrypted")
	}
	if host != request.Source.SSHHostKey {
		return errors.New("SSH host key changed; save the source with the intended pin")
	}
	block, err := ssh.MarshalPrivateKey(ed25519.NewKeyFromSeed(seed), "")
	if err != nil {
		return err
	}
	request.Source.SSHPrivateKey = string(pem.EncodeToMemory(block))
	raw, err := s.sealPreview(request)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE operations SET payload=$2 WHERE id=$1`, request.ID, raw)
	return err
}
