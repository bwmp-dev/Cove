package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Held for the rest of a transaction that claims hostnames so concurrent saves cannot claim the same one.
const routeClaimLock = 1129272902

type routeInput struct {
	worker.Route
	Auth            *string `json:"auth"`
	extensionFields map[string]json.RawMessage
}

func retainedRouteAuth(input []routeInput, previous []worker.Route) ([]worker.Route, error) {
	if input == nil {
		return nil, nil
	}
	routes := make([]worker.Route, len(input))
	for i, next := range input {
		routes[i] = next.Route
		if next.Auth != nil {
			routes[i].Auth = *next.Auth
		}
		prefix := ""
		if next.Policy != nil {
			prefix = next.Policy.PathPrefix
		}
		var matches []worker.Route
		for _, old := range previous {
			if old.Domain != next.Domain {
				continue
			}
			oldPrefix := ""
			if old.Policy != nil {
				oldPrefix = old.Policy.PathPrefix
			}
			if oldPrefix == prefix {
				matches = []worker.Route{old}
				break
			}
			matches = append(matches, old)
		}
		// GET hides password hashes. An omitted credential keeps the same route's protection,
		// including a changed prefix when the hostname's previous credentials agree.
		for j, old := range matches {
			if next.Auth == nil && j > 0 && old.Auth != routes[i].Auth {
				return nil, errors.New("set authentication explicitly when changing a path on a hostname with different route credentials")
			}
			if next.Auth == nil {
				routes[i].Auth = old.Auth
			}
			if err := retainRouteExtensions(&routes[i], next, old, j > 0); err != nil {
				return nil, err
			}
		}
	}
	return routes, nil
}

// routeConflict returns a hostname from routes that the dashboard or another resource already uses,
// including hostnames waiting in another resource's queued route update. Callers must hold a transaction.
func (s *Server) routeConflict(ctx context.Context, tx pgx.Tx, workload string, routes []worker.Route) (string, error) {
	if len(routes) == 0 {
		return "", nil
	}
	wanted := map[string]bool{}
	for _, route := range routes {
		wanted[route.Domain] = true
	}
	if panel, err := url.Parse(s.PublicURL); err == nil {
		for domain := range wanted {
			if worker.RouteDomainsOverlap(domain, strings.ToLower(panel.Hostname())) {
				return panel.Hostname(), nil
			}
		}
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		return "", err
	}
	claimed := func(list []worker.Route) string {
		for _, route := range list {
			for domain := range wanted {
				if worker.RouteDomainsOverlap(domain, route.Domain) {
					return route.Domain
				}
			}
		}
		return ""
	}
	rows, err := tx.Query(ctx, "SELECT configuration FROM workloads WHERE id<>$1", workload)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var encrypted []byte
		if err = rows.Scan(&encrypted); err != nil {
			return "", err
		}
		data, err := unseal(s.Key, encrypted)
		if err != nil {
			return "", err
		}
		var config workloadInput
		if err = json.Unmarshal(data, &config); err != nil {
			return "", err
		}
		if domain := claimed(config.Routes); domain != "" {
			return domain, nil
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	rows.Close()
	pending, err := tx.Query(ctx, "SELECT payload FROM operations WHERE workload_id<>$1 AND action='routes' AND state IN ('queued','running')", workload)
	if err != nil {
		return "", err
	}
	defer pending.Close()
	for pending.Next() {
		var encrypted []byte
		if err = pending.Scan(&encrypted); err != nil {
			return "", err
		}
		data, err := unseal(s.Key, encrypted)
		if err != nil {
			return "", err
		}
		var request worker.Request
		if err = json.Unmarshal(data, &request); err != nil {
			return "", err
		}
		if domain := claimed(request.Routes); domain != "" {
			return domain, nil
		}
	}
	if err := pending.Err(); err != nil {
		return "", err
	}
	pending.Close()
	issuers, err := tx.Query(ctx, "SELECT domains FROM route_certificate_issuers WHERE workload_id<>$1", workload)
	if err != nil {
		return "", err
	}
	defer issuers.Close()
	for issuers.Next() {
		var domains []string
		if err := issuers.Scan(&domains); err != nil {
			return "", err
		}
		for _, name := range domains {
			for domain := range wanted {
				if worker.RouteDomainsOverlap(domain, name) {
					return name, nil
				}
			}
		}
	}
	return "", issuers.Err()
}

// checkRouteClaims writes the HTTP error for an unavailable hostname and reports whether the routes may be saved.
func (s *Server) checkRouteClaims(w http.ResponseWriter, ctx context.Context, tx pgx.Tx, workload string, routes []worker.Route) bool {
	if err := s.validateRouteCertificates(ctx, tx, workload, routes); err != nil {
		if errors.Is(err, errInvalidRouteCertificate) {
			http.Error(w, err.Error(), 400)
		} else {
			http.Error(w, "route certificates unavailable", 503)
		}
		return false
	}
	domain, err := s.routeConflict(ctx, tx, workload, routes)
	if err != nil {
		http.Error(w, "could not check route hostnames", 503)
		return false
	}
	if domain != "" {
		http.Error(w, fmt.Sprintf("%s is already used by the dashboard or another resource", domain), 409)
		return false
	}
	return true
}

// A resource counts as deployed once the worker has a successful release that route updates can change.
func deployedWorkload(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workload string) (bool, error) {
	var deployed bool
	err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback') AND state='succeeded')", workload).Scan(&deployed)
	return deployed, err
}

// deployAttempted reports whether any deploy or rollback was queued for the resource. Even a failed,
// interrupted or late-cancelled one may have applied routes to the node's proxy, so only the worker
// can change routes from then on; saving them directly would release hostnames the proxy may serve.
func deployAttempted(ctx context.Context, tx pgx.Tx, workload string) (bool, error) {
	var attempted bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback'))", workload).Scan(&attempted)
	return attempted, err
}

// updateRoutes changes a service's public routes without redeploying it. Services that were never
// deployed only store the routes; others queue a route operation that updates the node's proxy and
// saves the routes once the worker reports success, so a failed update leaves the saved routes unchanged.
func (s *Server) updateRoutes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Routes []routeInput `json:"routes"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var liveRoutes bool
	err = tx.QueryRow(r.Context(), "SELECT w.configuration,COALESCE((n.capabilities->>'live_routes')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w", id).Scan(&encrypted, &liveRoutes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	var config workloadInput
	if json.Unmarshal(data, &config) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	if config.Kind == "job" {
		http.Error(w, "only services can have routes", 400)
		return
	}
	routes, err := retainedRouteAuth(in.Routes, config.Routes)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if config.Private != nil && len(routes) > 0 {
		http.Error(w, "disable private networking and deploy before adding public domains", 409)
		return
	}
	if err = worker.ValidateRoutes(routes, "service"); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if worker.HasRoutePolicies(routes) {
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'route_policies')::boolean,false) FROM nodes WHERE id=(SELECT node_id FROM workloads WHERE id=$1)`, id).Scan(&supported); err != nil {
			http.Error(w, "worker capabilities unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker before applying route policies", 409)
			return
		}
	}
	if err = checkRouteProtocol(r.Context(), tx, id, routes); err != nil {
		if errors.Is(err, errRouteProtocolUnsupported) {
			http.Error(w, err.Error(), 409)
		} else {
			http.Error(w, "worker capabilities unavailable", 503)
		}
		return
	}
	if worker.RoutesEqual(config.Routes, routes) {
		respond(w, 200, map[string]string{"state": "unchanged"})
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before changing routes", 409)
		return
	}
	if !s.checkRouteClaims(w, r.Context(), tx, id, routes) {
		return
	}
	attempted, err := deployAttempted(r.Context(), tx, id)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !attempted {
		config.Routes = routes
		if err = validateWorkload(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if data, err = json.Marshal(config); err == nil {
			encrypted, err = seal(s.Key, data)
		}
		if err != nil {
			http.Error(w, "could not protect configuration", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2 WHERE id=$1", id, encrypted); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save routes", 500)
			return
		}
		respond(w, 200, map[string]string{"state": "saved"})
		return
	}
	if !liveRoutes {
		http.Error(w, "upgrade and verify this resource's worker before changing routes on a resource that has been deployed", 409)
		return
	}
	request := worker.Request{ID: resourceID(), Workload: id, Action: "routes", Kind: config.Kind, Routes: routes}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data, err = json.Marshal(request)
	if err != nil {
		http.Error(w, "invalid routes", 500)
		return
	}
	payload, err := seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect operation", 500)
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'routes',$3)", request.ID, id, payload)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		http.Error(w, "wait for the active operation before changing routes", 409)
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not queue route update", 500)
		return
	}
	respond(w, 202, map[string]string{"id": request.ID, "state": "queued"})
}

// commitRoutes records a successful route operation and saves its routes in the same transaction,
// so redeploys keep the applied routes. Replaying it after a dispatcher restart is harmless.
func (s *Server) commitRoutes(ctx context.Context, id string, request worker.Request, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	if err = tx.QueryRow(ctx, "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", request.Workload).Scan(&encrypted); err != nil {
		return err
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(data, &config); err != nil {
		return err
	}
	config.Routes = request.Routes
	if data, err = json.Marshal(config); err != nil {
		return err
	}
	if encrypted, err = seal(s.Key, data); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE workloads SET configuration=$2 WHERE id=$1", request.Workload, encrypted); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE operations SET state='succeeded',result=$1,updated_at=now() WHERE id=$2", result, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
