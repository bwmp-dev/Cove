package control

import (
	"cove/internal/worker"
	"net/http"
)

func (s *Server) cleanupPreview(w http.ResponseWriter, r *http.Request) {
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "cleanup_preview"})
	if err != nil || response.Cleanup == nil {
		http.Error(w, "cleanup preview unavailable; verify connectivity and upgrade the worker if necessary", 502)
		return
	}
	respond(w, 200, response.Cleanup)
}
