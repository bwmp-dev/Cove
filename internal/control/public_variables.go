package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

func (s *Server) publicVariable(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("variable")
	if !s.requireAccess(w, r, "workload", id, 3) {
		return
	}
	if !variableName.MatchString(name) || len(name) > 128 {
		http.Error(w, "invalid variable name", 400)
		return
	}
	if r.Method != "GET" && !s.requirePersistentResource(w, r, id) {
		return
	}
	var in apiVariableInput
	if r.Method == "PUT" && !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var config workloadInput
	if tx.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&raw) != nil || s.unsealJSON(raw, &config) != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	if r.Method == "GET" {
		value, exists := config.Environment[name]
		if !exists {
			http.NotFound(w, r)
			return
		}
		mac := hmac.New(sha256.New, s.Key)
		mac.Write([]byte(id + "\x00" + name + "\x00" + value))
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 200, apiVariableView{Name: name, Version: hex.EncodeToString(mac.Sum(nil))})
		return
	}
	var busy bool
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&busy) != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	if busy {
		http.Error(w, "wait for the active operation before editing", 409)
		return
	}
	if config.Environment == nil {
		config.Environment = map[string]string{}
	}
	if r.Method == "DELETE" {
		delete(config.Environment, name)
	} else {
		config.Environment[name] = in.Value
	}
	if validateWorkload(config) != nil {
		http.Error(w, "invalid or reserved variable", 400)
		return
	}
	raw, err = s.sealPreview(config)
	if err != nil {
		http.Error(w, "could not protect variables", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2 WHERE id=$1`, id, raw); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save variable", 503)
		return
	}
	if r.Method == "DELETE" {
		respond(w, 200, apiOK{true})
		return
	}
	mac := hmac.New(sha256.New, s.Key)
	mac.Write([]byte(id + "\x00" + name + "\x00" + in.Value))
	respond(w, 200, apiVariableView{Name: name, Version: hex.EncodeToString(mac.Sum(nil))})
}
