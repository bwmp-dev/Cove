package control

import (
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
	var configuration []byte
	var node string
	var supported bool
	err := s.DB.QueryRow(r.Context(), `SELECT w.configuration,w.node_id,COALESCE((n.capabilities->>'preflight')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, r.PathValue("id")).Scan(&configuration, &node, &supported)
	if err != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify the worker to run preflight", 409)
		return
	}
	raw, err := unseal(s.Key, configuration)
	var c workloadInput
	if err != nil || json.Unmarshal(raw, &c) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	request := worker.Request{ID: resourceID(), Workload: r.PathValue("id"), Action: "deploy", ComposeBundle: c.ComposeBundle, Compose: c.Compose, Source: c.Source, Environment: c.Environment, Limits: c.Limits, Kind: c.Kind, JobService: c.JobService, Routes: c.Routes, Private: c.Private, Hooks: c.Hooks, FileSecrets: c.FileSecrets, GuardedDeploy: c.GuardedDeploy}
	// The inspection has no operation row. Preparation enriches only this request;
	// its persistence UPDATE matches no row, and no deployment is queued.
	if err = s.prepareInfrastructure(r.Context(), &request); err != nil {
		var resolution *variableResolutionError
		if errors.As(err, &resolution) {
			http.Error(w, resolution.Error(), 409)
			return
		}
		http.Error(w, "check shared variable references, worker capabilities, and selected infrastructure connections", 409)
		return
	}
	if err = s.prepareAppDeployment(r.Context(), &request); err != nil {
		http.Error(w, "GitHub repository access unavailable", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "preflight", Request: &request})
	if err != nil || response.Preflight == nil {
		http.Error(w, "preflight unavailable; check worker connectivity", 502)
		return
	}
	respond(w, 200, response.Preflight)
}
