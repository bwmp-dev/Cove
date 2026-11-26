package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"
)

func (s *Server) routeCertificateRetries(w http.ResponseWriter, r *http.Request) {
	var node string
	if err := s.DB.QueryRow(r.Context(), `SELECT node_id FROM workloads WHERE id=$1`, r.PathValue("id")).Scan(&node); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "certificate_retries", ID: r.PathValue("id")})
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) && remote.Message == "unsupported method" {
			http.Error(w, "Upgrade this worker to enable automatic public certificate retries.", 409)
			return
		}
		http.Error(w, "certificate retry status unavailable", 503)
		return
	}
	if response.CertificateRetries == nil {
		response.CertificateRetries = []worker.RouteCertificateRetry{}
	}
	respond(w, 200, response.CertificateRetries)
}
