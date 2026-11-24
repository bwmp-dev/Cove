package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) routeListeners(w http.ResponseWriter, r *http.Request) {
	var node string
	if err := s.DB.QueryRow(r.Context(), "SELECT node_id FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&node); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "worker unavailable", 503)
		}
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "ingress_exposures"})
	if err != nil {
		http.Error(w, "route listeners unavailable; verify or upgrade the worker", 502)
		return
	}
	if response.Exposures == nil {
		http.Error(w, "upgrade and verify the worker before using these route features", 409)
		return
	}
	listeners := response.Exposures.Settings.Listeners
	if listeners == nil {
		listeners = []worker.IngressListener{}
	}
	respond(w, 200, map[string]any{"listeners": listeners})
}

var errRouteListenerClaim = errors.New("route listener is reserved")

func (s *Server) withIngressListenerClaims(w http.ResponseWriter, r *http.Request, settings worker.IngressExposureSettings, request worker.RPCRequest) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		http.Error(w, "route reservations unavailable", 503)
		return
	}
	if err := s.checkIngressListenerClaims(r.Context(), tx, r.PathValue("id"), settings); err != nil {
		if errors.Is(err, errRouteListenerClaim) {
			http.Error(w, "a saved resource or pending operation still references a removed or changed route listener", 409)
		} else {
			http.Error(w, "route reservations unavailable", 503)
		}
		return
	}
	s.ingressExposureRPC(w, r, request)
}

func (s *Server) checkIngressListenerClaims(ctx context.Context, tx pgx.Tx, node string, settings worker.IngressExposureSettings) error {
	for _, query := range []struct {
		sql     string
		pending bool
	}{
		{"SELECT configuration FROM workloads WHERE node_id=$1", false},
		{"SELECT w.configuration FROM workload_moves m JOIN operations o ON o.id=m.operation_id JOIN workloads w ON w.id=o.workload_id WHERE m.target_node=$1 AND o.state IN ('queued','running')", false},
		{"SELECT o.payload FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.node_id=$1 AND o.state IN ('queued','running')", true},
	} {
		rows, err := tx.Query(ctx, query.sql, node)
		if err != nil {
			return err
		}
		for rows.Next() {
			var encrypted []byte
			if err := rows.Scan(&encrypted); err != nil {
				rows.Close()
				return err
			}
			data, err := unseal(s.Key, encrypted)
			if err != nil {
				rows.Close()
				return err
			}
			var routes []worker.Route
			if query.pending {
				var input worker.Request
				if err := json.Unmarshal(data, &input); err != nil {
					rows.Close()
					return err
				}
				routes = input.Routes
			} else {
				var input workloadInput
				if err := json.Unmarshal(data, &input); err != nil {
					rows.Close()
					return err
				}
				routes = input.Routes
			}
			if err := worker.ValidateRouteListeners(routes, settings); err != nil {
				rows.Close()
				return fmt.Errorf("%w: %w", errRouteListenerClaim, err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) checkRouteListenerSelection(w http.ResponseWriter, r *http.Request, node string, routes []worker.Route) bool {
	private := false
	for _, route := range routes {
		if route.IsTransportRoute() || route.Policy != nil && (route.Policy.Visibility == "lan" || route.Policy.Visibility == "tailscale" || route.Policy.Timeouts != nil) {
			private = true
		}
	}
	if !private {
		return true
	}
	var version int
	if err := s.DB.QueryRow(r.Context(), "SELECT COALESCE((capabilities->>'route_protocol')::integer,0) FROM nodes WHERE id=$1", node).Scan(&version); err != nil {
		http.Error(w, "worker capabilities unavailable", 503)
		return false
	}
	if version < worker.RouteProtocol(routes) {
		http.Error(w, "upgrade and verify the worker before selecting these route features", 409)
		return false
	}
	managed := false
	for _, route := range routes {
		if route.IsTransportRoute() || route.Policy != nil && (route.Policy.Visibility == "lan" || route.Policy.Visibility == "tailscale") {
			managed = true
		}
	}
	if !managed {
		return true
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "ingress_exposures"})
	if err != nil || response.Exposures == nil {
		http.Error(w, "route listeners unavailable; verify the node before saving listener routes", 409)
		return false
	}
	if err := worker.ValidateRouteListeners(routes, response.Exposures.Settings); err != nil {
		http.Error(w, err.Error(), 409)
		return false
	}
	return true
}
