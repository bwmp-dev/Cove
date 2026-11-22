package control

import (
	"cove/internal/worker"
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) restoreCopy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		BackupID string `json:"backup_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) == 0 || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node, project, environment string
	var encrypted []byte
	var capable bool
	if err = tx.QueryRow(r.Context(), `SELECT w.node_id,w.project_id,COALESCE(w.environment_id,''),w.configuration,COALESCE((n.capabilities->>'restore_copies')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, r.PathValue("id")).Scan(&node, &project, &environment, &encrypted, &capable); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !capable {
		http.Error(w, "upgrade and verify the worker before restoring a separate resource", 409)
		return
	}
	if _, err = environmentOperationGate(r.Context(), tx, r.PathValue("id"), "restore"); err != nil {
		http.Error(w, "resume the environment before restoring a resource copy", 409)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "operations unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the source operation to finish", 409)
		return
	}
	raw, err := unseal(s.Key, encrypted)
	var source workloadInput
	if err != nil || json.Unmarshal(raw, &source) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if source.Backup == nil || source.Kind == "job" {
		http.Error(w, "select a service backup", 400)
		return
	}
	target, operation := resourceID(), resourceID()
	request := worker.Request{ID: operation, Workload: r.PathValue("id"), Action: "restore", BackupID: in.BackupID, Backup: source.Backup}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.prepareInfrastructure(r.Context(), &request); err != nil {
		http.Error(w, "backup storage unavailable", 503)
		return
	}
	request.Workload = target
	result, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "restore_copy_plan", ID: r.PathValue("id"), Request: &request})
	if err != nil || result.RestoreCopy == nil {
		http.Error(w, "backup cannot be copied automatically; verify its retained release and remove external mounts or privileged options from a recovery configuration", 409)
		return
	}
	plan := result.RestoreCopy
	config := workloadInput{Name: in.Name, NodeID: node, Compose: plan.Compose, Environment: plan.Environment, Kind: "service", Limits: source.Limits}
	if err = validateWorkload(config); err != nil {
		http.Error(w, "backup recovery configuration is invalid", 409)
		return
	}
	raw, err = json.Marshal(config)
	if err != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	encrypted, err = seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect recovery configuration", 503)
		return
	}
	cpu, memory := configuredUsage(config)
	if _, err = tx.Exec(r.Context(), `INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,manually_stopped,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,'service',NULLIF($5,''),$6,$7,true,$8,$9)`, target, node, in.Name, encrypted, environment, actor(r).Team, project, cpu, memory); err != nil {
		if !quotaError(w, err) {
			http.Error(w, "could not create recovery resource", 503)
		}
		return
	}
	request.Action = "restore_copy"
	request.Compose = plan.Compose
	request.Environment = plan.Environment
	request.RestoreCopy = &worker.RestoreCopy{Source: r.PathValue("id")}
	request.InfrastructurePrepared = true
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "invalid recovery operation", 500)
		return
	}
	raw, err = json.Marshal(request)
	if err != nil {
		http.Error(w, "invalid recovery operation", 500)
		return
	}
	encrypted, err = seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect recovery operation", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'restore_copy',$3)`, operation, target, encrypted); err != nil {
		http.Error(w, "could not queue recovery", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save recovery", 503)
		return
	}
	respond(w, 202, map[string]string{"id": operation, "resource_id": target})
}
