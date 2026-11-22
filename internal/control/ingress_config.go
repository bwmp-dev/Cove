package control

import (
	"cove/internal/worker"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Custom proxy configuration lives on the worker's proxy volume. The control plane relays it
// without storing or logging file contents; only instance administrators reach these handlers.

const workerUpgradeRequired = "this worker does not support custom proxy configuration; upgrade the worker first"

func (s *Server) ingressRPC(w http.ResponseWriter, r *http.Request, request worker.RPCRequest) (worker.RPCResponse, bool) {
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), request)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return response, false
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		switch remote.Message {
		case "unsupported method":
			http.Error(w, workerUpgradeRequired, 409)
		case worker.IngressFileMissing:
			http.Error(w, worker.IngressFileMissing, 404)
		default:
			http.Error(w, remote.Message, 409)
		}
		return response, false
	}
	if err != nil {
		http.Error(w, "worker unavailable; verify the node and upgrade its worker if this persists", 502)
		return response, false
	}
	return response, true
}

func (s *Server) ingressConfig(w http.ResponseWriter, r *http.Request) {
	response, ok := s.ingressRPC(w, r, worker.RPCRequest{Method: "ingress_config"})
	if !ok {
		return
	}
	if response.Ingress == nil {
		http.Error(w, workerUpgradeRequired, 409)
		return
	}
	respond(w, 200, response.Ingress)
}

func (s *Server) ingressConfigFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !worker.ValidIngressFileName(name) {
		http.Error(w, "invalid file name", 400)
		return
	}
	response, ok := s.ingressRPC(w, r, worker.RPCRequest{Method: "ingress_config_file", Ingress: &worker.IngressConfigRequest{Name: name}})
	if !ok {
		return
	}
	if response.IngressFile == nil {
		http.Error(w, workerUpgradeRequired, 409)
		return
	}
	respond(w, 200, response.IngressFile)
}

func (s *Server) validateIngressConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.changeIngressConfig(w, r, worker.IngressConfigRequest{Action: "validate", Name: in.Name, Content: &in.Content})
}

func (s *Server) putIngressConfigFile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content  string `json:"content"`
		Revision string `json:"revision"`
		ChangeID string `json:"change_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.changeIngressConfig(w, r, worker.IngressConfigRequest{Action: "put", Name: r.PathValue("name"), Content: &in.Content, Revision: in.Revision, ChangeID: in.ChangeID})
}

// importIngressConfigFile and deleteIngressConfigFile name the revision the administrator reviewed,
// so a file changed since then is never adopted or removed.
func (s *Server) importIngressConfigFile(w http.ResponseWriter, r *http.Request) {
	s.revisionChange(w, r, "import")
}

func (s *Server) deleteIngressConfigFile(w http.ResponseWriter, r *http.Request) {
	s.revisionChange(w, r, "delete")
}

func (s *Server) revisionChange(w http.ResponseWriter, r *http.Request, action string) {
	var in struct {
		Revision string `json:"revision"`
		ChangeID string `json:"change_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.changeIngressConfig(w, r, worker.IngressConfigRequest{Action: action, Name: r.PathValue("name"), Revision: in.Revision, ChangeID: in.ChangeID})
}

func (s *Server) changeIngressConfig(w http.ResponseWriter, r *http.Request, request worker.IngressConfigRequest) {
	if !worker.ValidIngressFileName(request.Name) {
		http.Error(w, "file names must be lowercase .yaml or .yml names", 400)
		return
	}
	if request.Content != nil && len(*request.Content) > worker.MaxCustomIngressBytes {
		http.Error(w, fmt.Sprintf("custom configuration must not exceed %d KiB", worker.MaxCustomIngressBytes/1024), 400)
		return
	}
	response, ok := s.ingressRPC(w, r, worker.RPCRequest{Method: "ingress_config_change", Ingress: &request})
	if !ok {
		return
	}
	result := response.IngressResult
	if result == nil {
		http.Error(w, workerUpgradeRequired, 409)
		return
	}
	if result.Change != nil {
		respond(w, 200, result.Change)
		return
	}
	status := map[string]int{"conflict": 409, "blocked": 409, "invalid": 400, "not_found": 404, "unavailable": 409}[result.Code]
	if status == 0 {
		status = 502
	}
	http.Error(w, result.Error, status)
}
