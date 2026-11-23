package control

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) reconcileNodeProvision(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		Confirm  bool   `json:"confirm"`
		ServerID string `json:"server_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm || in.ServerID != "" && !providerValue.MatchString(in.ServerID) {
		http.Error(w, "confirm looking up the existing owned server; creation is never repeated", 400)
		return
	}
	job := nodeProvision{ID: r.PathValue("id")}
	err := s.DB.QueryRow(r.Context(), `SELECT provider_id,cloud_id,cloud_name,phase FROM node_provisions WHERE id=$1`, job.ID).Scan(&job.ProviderID, &job.CloudID, &job.CloudName, &job.Phase)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "provisioning state unavailable", 503)
		return
	}
	if job.Phase != "uncertain" || job.ProviderID == nil {
		http.Error(w, "only unresolved creations can be reconciled", 409)
		return
	}
	if job.CloudID != "" && in.ServerID != "" && job.CloudID != in.ServerID {
		http.Error(w, "the acknowledged server identity cannot be replaced", 409)
		return
	}
	if in.ServerID != "" {
		job.CloudID = in.ServerID
	}
	provider, err := s.loadNodeProvider(r.Context(), *job.ProviderID)
	if err != nil {
		http.Error(w, "provider credentials unavailable", 503)
		return
	}
	instance, err := s.providerInstance(r.Context(), provider, job)
	if err != nil || instance == nil {
		http.Error(w, "no unique server with the exact creation name and ownership marker was confirmed; inspect the provider console", 409)
		return
	}
	tag, err := s.DB.Exec(r.Context(), `UPDATE node_provisions SET cloud_id=$2,phase='server_pending',last_error='',deadline=now()+interval '20 minutes',next_check_at=now(),updated_at=now() WHERE id=$1 AND phase='uncertain'`, job.ID, instance.ID)
	if err != nil {
		http.Error(w, "reconciliation could not be confirmed; refresh history", 503)
		return
	}
	if tag.RowsAffected() != 1 {
		http.Error(w, "the request changed; refresh history", 409)
		return
	}
	respond(w, 202, map[string]string{"id": job.ID})
}

func (s *Server) nodeCloudMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var provider *string
	var kind, id, name string
	err := s.DB.QueryRow(r.Context(), `SELECT cloud_provider_id,cloud_kind,cloud_server_id,cloud_server_name FROM nodes WHERE id=$1`, r.PathValue("id")).Scan(&provider, &kind, &id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "cloud origin unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"provider_id": provider, "kind": kind, "server_id": id, "server_name": name, "checked_at": time.Now().UTC()})
}
