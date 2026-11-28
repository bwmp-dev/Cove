package control

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) composeBundle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	if !s.requireAccess(w, r, "workload", id, level) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.requirePersistentResource(w, r, id) {
		return
	}
	var in struct {
		Bundle *worker.ComposeBundle `json:"bundle"`
	}
	if r.Method == "PUT" && !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Compose configuration unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var supported bool
	if err = tx.QueryRow(r.Context(), `SELECT w.configuration,COALESCE((n.capabilities->>'compose_bundles')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&encrypted, &supported); err != nil {
		http.Error(w, "Compose configuration unavailable", 503)
		return
	}
	var config workloadInput
	if err = s.unsealJSON(encrypted, &config); err != nil {
		http.Error(w, "Compose configuration unavailable", 503)
		return
	}
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/export") {
		if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'Compose files exported',$2,200,$3)`, "user "+actor(r).ID, id, actor(r).Team); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not audit Compose file export", 503)
			return
		}
		respond(w, 200, map[string]any{"bundle": config.ComposeBundle})
		return
	}
	if r.Method == "PUT" {
		if in.Bundle != nil && !supported {
			http.Error(w, "upgrade and verify the worker before selecting Compose files or profiles", 409)
			return
		}
		var active bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); err != nil {
			http.Error(w, "operations unavailable", 503)
			return
		}
		if active {
			http.Error(w, "wait for the active operation before editing", 409)
			return
		}
		if in.Bundle != nil {
			var saved []byte
			err = tx.QueryRow(r.Context(), `SELECT configuration FROM preview_policies WHERE workload_id=$1`, id).Scan(&saved)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "preview settings unavailable", 503)
				return
			}
			if err == nil {
				var policy previewPolicy
				if s.unsealJSON(saved, &policy) != nil {
					http.Error(w, "preview settings unavailable", 503)
					return
				}
				if policy.Enabled {
					http.Error(w, "disable pull-request previews before selecting Compose files or profiles", 409)
					return
				}
			}
		}
		config.ComposeBundle = in.Bundle
		if err = validateWorkload(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		encrypted, err = s.sealJSON(config)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2 WHERE id=$1`, id, encrypted)
		}
		if err == nil {
			err = s.pruneImageUpdateRetention(r.Context(), tx, id, config)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save Compose files", 503)
			return
		}
	}
	files, assets, paths, profiles := []string{}, []string{}, []string{}, []string{}
	if bundle := config.ComposeBundle; bundle != nil {
		for _, file := range bundle.Files {
			files = append(files, file.Path)
		}
		for name := range bundle.Assets {
			assets = append(assets, name)
		}
		sort.Strings(assets)
		paths = append(paths, bundle.Paths...)
		profiles = append(profiles, bundle.Profiles...)
	}
	respond(w, 200, map[string]any{"enabled": config.ComposeBundle != nil, "available": supported, "files": files, "assets": assets, "paths": paths, "profiles": profiles})
}
