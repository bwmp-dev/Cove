package control

import (
	"cove/internal/worker"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (s *Server) restorePITR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		Name      string    `json:"name"`
		Base      string    `json:"base"`
		Timestamp time.Time `json:"timestamp"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 1 || len(in.Name) > 120 || len(in.Base) != 62 || in.Timestamp.IsZero() || in.Timestamp.After(time.Now()) {
		http.Error(w, "select a retained base, a past UTC timestamp and a resource name of 1–120 characters", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node, project, environment string
	var supported, busy bool
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,w.project_id,COALESCE(w.environment_id,''),COALESCE((n.capabilities->>'postgres_pitr')::boolean,false),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, r.PathValue("id")).Scan(&node, &project, &environment, &supported, &busy)
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !supported || busy {
		http.Error(w, "verify an upgraded worker and wait for the source operation to finish", 409)
		return
	}
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), `SELECT configuration FROM workload_pitr WHERE workload_id=$1`, r.PathValue("id")).Scan(&encrypted); err != nil {
		http.Error(w, "PITR history unavailable", 409)
		return
	}
	config, err := s.decodePITR(encrypted)
	if err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	}
	if !strings.HasPrefix(in.Base, config.Policy.Archive+"_") {
		http.Error(w, "PITR base does not belong to this resource", 400)
		return
	}
	target := resourceID()
	request := worker.Request{ID: resourceID(), Workload: target, Action: "pitr_restore", Kind: "service", PITRRestore: &worker.PITRRestore{Source: r.PathValue("id"), Base: in.Base, Timestamp: in.Timestamp.UTC()}, Environment: map[string]string{"COVE_PITR_PASSWORD": resourceID() + resourceID()}, InfrastructurePrepared: true}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "pitr_copy_plan", ID: r.PathValue("id"), Request: &request})
	if err != nil || response.RestoreCopy == nil {
		http.Error(w, "PITR recovery plan unavailable; select a retained base and a timestamp inside its continuous observed window", 409)
		return
	}
	plan := response.RestoreCopy
	request.Compose, request.Environment = plan.Compose, plan.Environment
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "PITR recovery plan is invalid", 409)
		return
	}
	workload := workloadInput{Name: in.Name, NodeID: node, Compose: plan.Compose, Environment: plan.Environment, Kind: "service"}
	if err = validateWorkload(workload); err != nil {
		http.Error(w, "PITR recovery resource is invalid", 409)
		return
	}
	data, err := json.Marshal(workload)
	if err != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	encrypted, err = seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect recovery credentials", 503)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,manually_stopped) VALUES($1,$2,$3,$4,'service',NULLIF($5,''),$6,$7,true)`, target, node, in.Name, encrypted, environment, actor(r).Team, project)
	if err != nil {
		if !quotaError(w, err) {
			http.Error(w, "could not create PITR recovery resource", 503)
		}
		return
	}
	if err = s.insertPITROperation(r.Context(), tx, request); err != nil {
		http.Error(w, "could not queue PITR recovery", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save PITR recovery", 503)
		return
	}
	respond(w, 202, map[string]any{"id": request.ID, "resource_id": target, "credentials": map[string]string{"username": plan.Environment["COVE_PITR_USER"], "password": plan.Environment["COVE_PITR_PASSWORD"]}})
}
