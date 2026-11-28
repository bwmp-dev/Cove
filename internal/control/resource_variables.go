package control

import (
	"encoding/json"
	"net/http"
	"regexp"
)

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type resourceVariableBinding struct {
	Name      string `json:"name"`
	SourceID  string `json:"source_id"`
	SourceKey string `json:"source_key"`
}

func (s *Server) resourceVariables(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT name,source_id,source_key FROM resource_variable_bindings WHERE workload_id=$1 ORDER BY name`, id)
		if err != nil {
			http.Error(w, "bindings unavailable", 503)
			return
		}
		defer rows.Close()
		bindings := []resourceVariableBinding{}
		for rows.Next() {
			var b resourceVariableBinding
			if err = rows.Scan(&b.Name, &b.SourceID, &b.SourceKey); err != nil {
				http.Error(w, "bindings unavailable", 503)
				return
			}
			bindings = append(bindings, b)
		}
		if rows.Err() != nil {
			http.Error(w, "bindings unavailable", 503)
			return
		}
		respond(w, 200, bindings)
		return
	}
	var in resourceVariableBinding
	if !decode(w, r, &in) {
		return
	}
	if !variableName.MatchString(in.Name) || len(in.Name) > 128 {
		http.Error(w, "invalid binding name", 400)
		return
	}
	if r.Method == "DELETE" {
		if _, err := s.DB.Exec(r.Context(), `DELETE FROM resource_variable_bindings WHERE workload_id=$1 AND name=$2`, id, in.Name); err != nil {
			http.Error(w, "could not remove binding", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if in.SourceID == id || !variableName.MatchString(in.SourceKey) || len(in.SourceKey) > 128 {
		http.Error(w, "invalid source", 400)
		return
	}
	permission := "read_secrets"
	if databaseExportKey(in.SourceKey) && !databaseExportSecret(in.SourceKey) {
		var database bool
		if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1 AND team_id=$2 AND kind='database')", in.SourceID, actor(r).Team).Scan(&database); err != nil {
			http.Error(w, "source permissions unavailable", 503)
			return
		}
		if database {
			permission = "view"
		}
	}
	if !s.requirePermission(w, r, "workload", in.SourceID, permission) || !s.requirePersistentResource(w, r, in.SourceID) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "bindings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Lock both resources in a stable order against deletion and configuration changes.
	rows, err := tx.Query(r.Context(), `SELECT id,configuration FROM workloads WHERE id IN ($1,$2) AND team_id=$3 ORDER BY id FOR UPDATE`, id, in.SourceID, actor(r).Team)
	if err != nil {
		http.Error(w, "bindings unavailable", 503)
		return
	}
	count := 0
	var source []byte
	for rows.Next() {
		var key string
		var encrypted []byte
		if err = rows.Scan(&key, &encrypted); err != nil {
			break
		}
		count++
		if key == in.SourceID {
			source = encrypted
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		http.Error(w, "bindings unavailable", 503)
		return
	}
	if count != 2 {
		http.NotFound(w, r)
		return
	}
	raw, err := unseal(s.Key, source)
	var config workloadInput
	if err != nil || json.Unmarshal(raw, &config) != nil {
		http.Error(w, "source unavailable", 503)
		return
	}
	_, exists := config.Environment[in.SourceKey]
	if config.Database != nil {
		exists = false
	}
	if !exists && config.Database != nil && databaseExportKey(in.SourceKey) {
		values, err := databaseExports(config, nil)
		if err == nil {
			_, exists = values[in.SourceKey]
		}
	}
	if !exists {
		http.Error(w, "source variable does not exist", 400)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO resource_variable_bindings(workload_id,name,source_id,source_key) VALUES($1,$2,$3,$4) ON CONFLICT(workload_id,name) DO UPDATE SET source_id=EXCLUDED.source_id,source_key=EXCLUDED.source_key`, id, in.Name, in.SourceID, in.SourceKey); err != nil {
		http.Error(w, "could not save binding", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save binding", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
