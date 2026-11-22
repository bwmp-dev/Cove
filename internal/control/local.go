package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
)

func (s *Server) localRPC(ctx context.Context, request worker.RPCRequest) (worker.RPCResponse, error) {
	var response worker.RPCResponse
	if s.LocalSocket == "" {
		return response, errors.New("local worker is not configured")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	var out bytes.Buffer
	if err = worker.Proxy(ctx, s.LocalSocket, bytes.NewReader(data), &out); err != nil {
		return response, err
	}
	if err = json.Unmarshal(out.Bytes(), &response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, &RemoteError{Message: response.Error}
	}
	return response, nil
}
func (s *Server) addLocalNode(w http.ResponseWriter, r *http.Request) {
	result, err := s.localRPC(r.Context(), worker.RPCRequest{Method: "probe"})
	if err != nil || result.Probe == nil || !result.Probe.Ready {
		http.Error(w, "configure and start the local worker before enabling local workloads", 409)
		return
	}
	data, err := json.Marshal(result.Probe)
	if err != nil {
		http.Error(w, "invalid probe result", 500)
		return
	}
	id := resourceID()
	err = s.DB.QueryRow(r.Context(), "INSERT INTO nodes(id,name,address,port,public_key,private_key,mode,state,capabilities) VALUES($1,'Main node','local',22,'',$2,'local','ready',$3) ON CONFLICT (mode) WHERE mode='local' DO UPDATE SET state='ready',capabilities=EXCLUDED.capabilities RETURNING id", id, []byte{}, data).Scan(&id)
	if err != nil {
		http.Error(w, "could not enable local workloads", 500)
		return
	}
	if _, err = s.DB.Exec(r.Context(), "INSERT INTO team_nodes(team_id,node_id) VALUES($1,$2) ON CONFLICT DO NOTHING", actor(r).Team, id); err != nil {
		http.Error(w, "could not assign node", 500)
		return
	}
	respond(w, 201, map[string]string{"id": id, "state": "ready"})
}
