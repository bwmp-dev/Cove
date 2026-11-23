package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) ingressExposures(w http.ResponseWriter, r *http.Request) {
	s.ingressExposureRPC(w, r, worker.RPCRequest{Method: "ingress_exposures"})
}

func (s *Server) initializeIngressExposures(w http.ResponseWriter, r *http.Request) {
	s.ingressExposureRPC(w, r, worker.RPCRequest{Method: "ingress_exposures_initialize"})
}

func (s *Server) planIngressExposures(w http.ResponseWriter, r *http.Request) {
	var settings worker.IngressExposureSettings
	if !decode(w, r, &settings) {
		return
	}
	s.ingressExposureRPC(w, r, worker.RPCRequest{Method: "ingress_exposures_plan", Exposures: &worker.IngressExposureRequest{Settings: settings}})
}

func (s *Server) changeIngressExposures(w http.ResponseWriter, r *http.Request) {
	var request worker.IngressExposureRequest
	if !decode(w, r, &request) {
		return
	}
	if len(request.Revision) != 64 || len(request.Digest) != 64 || !request.AcceptInterruption {
		http.Error(w, "a reviewed listener plan and confirmation of the shared proxy interruption are required", 400)
		return
	}
	s.ingressExposureRPC(w, r, worker.RPCRequest{Method: "ingress_exposures_apply", Exposures: &request})
}

func (s *Server) ingressExposureRPC(w http.ResponseWriter, r *http.Request, request worker.RPCRequest) {
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), request)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		if remote.Message == "unsupported method" {
			http.Error(w, "this worker does not support managed listeners; upgrade and verify it first", 409)
		} else {
			http.Error(w, remote.Message, 409)
		}
		return
	}
	if err != nil {
		http.Error(w, "worker unavailable; verify the node before changing listeners", 502)
		return
	}
	if request.Method == "ingress_exposures_plan" {
		if response.ExposurePlan == nil {
			http.Error(w, "upgrade and verify the worker before planning listeners", 409)
			return
		}
		respond(w, 200, response.ExposurePlan)
		return
	}
	if response.Exposures == nil {
		http.Error(w, "upgrade and verify the worker before changing listeners", 409)
		return
	}
	respond(w, 200, response.Exposures)
}
