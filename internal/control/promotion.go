package control

import (
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"time"
)

func (s *Server) promoteRelease(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
	var in struct {
		Source     string `json:"source_id"`
		Release    string `json:"release"`
		Repository string `json:"registry_repository"`
	}
	if !decode(w, r, &in) {
		return
	}
	target := r.PathValue("id")
	if in.Source == target || in.Release == "" {
		http.Error(w, "select another resource and a successful release", 400)
		return
	}
	if !s.requireAccess(w, r, "workload", in.Source, 3) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `SELECT w.id,w.node_id,w.project_id,w.configuration,COALESCE((n.capabilities->>'exact_promotion')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id IN ($1,$2) AND w.team_id=$3 ORDER BY w.id FOR UPDATE OF w`, target, in.Source, actor(r).Team)
	if err != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	type resource struct {
		node, project string
		configuration []byte
		supported     bool
	}
	resources := map[string]resource{}
	for rows.Next() {
		var id string
		var item resource
		if err = rows.Scan(&id, &item.node, &item.project, &item.configuration, &item.supported); err != nil {
			break
		}
		resources[id] = item
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	if len(resources) != 2 {
		http.NotFound(w, r)
		return
	}
	src, dst := resources[in.Source], resources[target]
	if src.project != dst.project {
		http.Error(w, "promotion requires resources in the same project", 400)
		return
	}
	if !src.supported || !dst.supported {
		http.Error(w, "upgrade and verify both workers before promotion", 409)
		return
	}
	var valid, active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE id=$1 AND workload_id=$2 AND state='succeeded' AND action IN ('deploy','rollback','adopt')),EXISTS(SELECT 1 FROM operations WHERE workload_id IN ($2,$3) AND state IN ('queued','running'))`, in.Release, in.Source, target).Scan(&valid, &active); err != nil {
		http.Error(w, "operations unavailable", 503)
		return
	}
	if !valid {
		http.Error(w, "select a successful source release", 400)
		return
	}
	if active {
		http.Error(w, "wait for active operations on both resources", 409)
		return
	}
	raw, err := unseal(s.Key, dst.configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(raw, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if config.Kind == "job" {
		http.Error(w, "promotion requires a service resource", 400)
		return
	}
	export := worker.PromotionExport{Workload: in.Source, Release: in.Release, Portable: src.node != dst.node, Repository: in.Repository}
	if in.Repository != "" {
		var connection string
		if err = tx.QueryRow(r.Context(), `SELECT connection_id FROM workload_connections WHERE workload_id=$1 AND kind='registry'`, in.Source).Scan(&connection); err != nil {
			http.Error(w, "assign a Docker registry to the source resource before publishing", 409)
			return
		}
		saved, err := s.readConnection(r.Context(), connection, actor(r).Team, "registry")
		if err != nil {
			http.Error(w, "source registry unavailable", 503)
			return
		}
		export.Registry = saved.Registry
	}
	result, err := s.nodeRPC(r.Context(), src.node, worker.RPCRequest{Method: "export_release", Promotion: &export})
	if err != nil || len(result.Images) == 0 {
		http.Error(w, "source images unavailable; verify the retained release and registry publication settings", 502)
		return
	}
	request := worker.Request{ID: resourceID(), Workload: target, Action: "deploy", ComposeBundle: config.ComposeBundle, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Kind: config.Kind, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, ImagePins: result.Images, Registry: export.Registry}
	if err = loadNetworkRequest(r.Context(), tx, &request); err != nil {
		http.Error(w, "target networking unavailable", 409)
		return
	}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "invalid promotion configuration", 400)
		return
	}
	raw, err = json.Marshal(request)
	if err != nil {
		http.Error(w, "could not prepare promotion", 500)
		return
	}
	encrypted, err := seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect promotion", 500)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, request.ID, target, encrypted)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		http.Error(w, "target already has an active operation", 409)
		return
	}
	if err != nil {
		http.Error(w, "could not queue promotion", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE workloads SET manually_stopped=false WHERE id=$1`, target); err != nil {
		http.Error(w, "could not save target intent", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not queue promotion", 503)
		return
	}
	respond(w, 202, map[string]string{"id": request.ID, "state": "queued"})
}
