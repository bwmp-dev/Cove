package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cove/internal/worker"
)

type variableSource interface {
	Resolve(context.Context, string) (string, error)
}

type variableResolutionError struct {
	Name   string
	Reason string
}

func (e *variableResolutionError) Error() string {
	return fmt.Sprintf("Variable %s could not be resolved: %s.", e.Name, e.Reason)
}

func (s *Server) resolveDeploymentVariables(ctx context.Context, request *worker.Request) error {
	var err error
	if !request.Preview {
		request.Environment, err = s.resolveSharedVariables(ctx, request.Workload, request.Environment)
		if err != nil {
			return err
		}
	}
	request.Environment, err = s.resolveExternalVariables(ctx, request.Workload, request.Environment)
	if err != nil {
		return err
	}
	var project, environment string
	var supported bool
	err = s.DB.QueryRow(ctx, `SELECT COALESCE(p.name,''),COALESCE(e.name,''),COALESCE((n.capabilities->>'builtin_variables')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN projects p ON p.id=w.project_id LEFT JOIN environments e ON e.id=w.environment_id WHERE w.id=$1`, request.Workload).Scan(&project, &environment, &supported)
	if err != nil {
		return err
	}
	if !supported {
		return nil
	}
	defaults := map[string]string{worker.CoveFQDN: "", worker.CoveURL: "", worker.CoveCommitSHA: "", worker.CoveBranch: "", worker.CoveEnvironment: environment, worker.CoveProject: project, worker.CoveRelease: request.ID}
	for _, route := range request.Routes {
		if !strings.HasPrefix(route.Domain, "*.") {
			defaults[worker.CoveFQDN] = route.Domain
			defaults[worker.CoveURL] = "https://" + route.Domain
			break
		}
	}
	if request.Source != nil {
		defaults[worker.CoveCommitSHA] = request.Source.Revision
		defaults[worker.CoveBranch] = request.Source.Branch
	}
	request.BuiltinDefaults = defaults
	return nil
}

func (s *Server) builtinVariables(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 1) {
		return
	}
	var supported bool
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'builtin_variables')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, id).Scan(&supported); err != nil {
		http.Error(w, "builtin variables unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"available": supported, "variables": worker.BuiltinVariables()})
}

func validSecretValue(value string) bool {
	return len(value) <= 65536 && !strings.ContainsRune(value, 0)
}

var errSecretUnavailable = errors.New("secret is missing, inaccessible, invalid, or its provider is unavailable")
