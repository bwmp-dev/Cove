package control

import (
	"cove/internal/worker"
	"encoding/json"
	"net/http"
)

func (s *Server) privateNetwork(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted); err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := unseal(s.Key, encrypted)
	var c workloadInput
	if err != nil || json.Unmarshal(data, &c) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	if r.Method == "GET" {
		if c.Private != nil {
			c.Private.AuthKey = ""
		}
		respond(w, 200, map[string]any{"private": c.Private})
		return
	}
	var in struct {
		Private *worker.PrivateNetwork `json:"private"`
	}
	if !decode(w, r, &in) {
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", r.PathValue("id")).Scan(&active); err != nil || active {
		http.Error(w, "wait for the active operation", 409)
		return
	}
	if in.Private != nil {
		var capable bool
		if err = tx.QueryRow(r.Context(), "SELECT COALESCE((n.capabilities->>'private_networks')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1", r.PathValue("id")).Scan(&capable); err != nil || !capable {
			http.Error(w, "upgrade and verify this worker before enabling private networking", 409)
			return
		}
		if len(c.Routes) > 0 || c.Kind == "job" {
			http.Error(w, "private access requires a service with no public domains", 409)
			return
		}
		if in.Private.AuthKey == "" && c.Private != nil && in.Private.Hostname == c.Private.Hostname {
			in.Private.AuthKey = c.Private.AuthKey
		}
		if err = worker.ValidatePrivateNetwork(in.Private); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	c.Private = in.Private
	raw, err := json.Marshal(c)
	if err != nil {
		http.Error(w, "invalid configuration", 500)
		return
	}
	encrypted, err = seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect private settings", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2 WHERE id=$1", r.PathValue("id"), encrypted); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save private networking", 503)
		return
	}
	respond(w, 200, map[string]string{"state": "saved", "message": "Deploy the resource to apply private networking changes."})
}
