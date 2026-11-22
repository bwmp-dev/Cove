package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

func (s *Server) sharedVariables(w http.ResponseWriter, r *http.Request) {
	var project, environment string
	team := actor(r).Team
	if strings.Contains(r.URL.Path, "/projects/") {
		project = r.PathValue("id")
	}
	if strings.Contains(r.URL.Path, "/environments/") {
		environment = r.PathValue("id")
		if err := s.DB.QueryRow(r.Context(), "SELECT project_id FROM environments WHERE id=$1 AND team_id=$2", environment, team).Scan(&project); err != nil {
			http.Error(w, "environment unavailable", 404)
			return
		}
	}
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT name FROM shared_variables WHERE team_id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'') AND environment_id IS NOT DISTINCT FROM NULLIF($3,'') ORDER BY name`, team, project, environment)
		if err != nil {
			http.Error(w, "variables unavailable", 503)
			return
		}
		defer rows.Close()
		names := []string{}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				http.Error(w, "variables unavailable", 503)
				return
			}
			names = append(names, name)
		}
		if rows.Err() != nil {
			http.Error(w, "variables unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"keys": names})
		return
	}
	var in struct {
		Patch map[string]*string `json:"patch"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Patch) > 256 {
		http.Error(w, "at most 256 variables per update", 400)
		return
	}
	values := map[string]string{}
	for name, value := range in.Patch {
		values[name] = ""
		if value != nil {
			values[name] = *value
		}
	}
	if err := worker.ValidateRequest(worker.Request{ID: "validation", Workload: "validation", Action: "deploy", Compose: "services: {}", Environment: values}); err != nil {
		http.Error(w, "invalid variable name or value", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	names := make([]string, 0, len(in.Patch))
	for name := range in.Patch {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := in.Patch[name]
		if value == nil {
			_, err = tx.Exec(r.Context(), `DELETE FROM shared_variables WHERE team_id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'') AND environment_id IS NOT DISTINCT FROM NULLIF($3,'') AND name=$4`, team, project, environment, name)
		} else {
			var encrypted []byte
			encrypted, err = seal(s.Key, []byte(*value))
			if err == nil {
				_, err = tx.Exec(r.Context(), `INSERT INTO shared_variables(team_id,project_id,environment_id,name,value) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5) ON CONFLICT(team_id,project_id,environment_id,name) DO UPDATE SET value=EXCLUDED.value`, team, project, environment, name, encrypted)
			}
		}
		if err != nil {
			http.Error(w, "could not save variables", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save variables", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

var errSharedVariables = errors.New("shared variable references could not be resolved")

var sharedReference = regexp.MustCompile(`\{\{(team|project|environment|resource)\.([A-Za-z_][A-Za-z0-9_]*)\}\}`)

func (s *Server) resolveSharedVariables(ctx context.Context, workload string, environment map[string]string) (map[string]string, error) {
	needs := false
	for _, value := range environment {
		if sharedReference.MatchString(value) {
			needs = true
			break
		}
	}
	if !needs {
		return environment, nil
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	cache := map[string]string{}
	visiting := map[string]bool{}
	var resolve func(string, string, int) (string, error)
	resolve = func(id, value string, depth int) (string, error) {
		if depth > 32 {
			return "", errSharedVariables
		}
		var resolutionError error
		size := len(value)
		bounded := func(reference, resolved string) string {
			size += len(resolved) - len(reference)
			if size > 65536 {
				resolutionError = errSharedVariables
				return ""
			}
			return resolved
		}
		result := sharedReference.ReplaceAllStringFunc(value, func(reference string) string {
			if resolutionError != nil {
				return ""
			}
			scope, key, _ := strings.Cut(reference[2:len(reference)-2], ".")
			cacheKey := id + ":" + scope + ":" + key
			if v, ok := cache[cacheKey]; ok {
				return bounded(reference, v)
			}
			if visiting[cacheKey] {
				resolutionError = errSharedVariables
				return ""
			}
			visiting[cacheKey] = true
			defer delete(visiting, cacheKey)
			var encrypted []byte
			var resolved string
			if scope == "resource" {
				var source, sourceKey string
				resolutionError = tx.QueryRow(ctx, `SELECT b.source_id,b.source_key,src.configuration FROM resource_variable_bindings b JOIN workloads dst ON dst.id=b.workload_id JOIN workloads src ON src.id=b.source_id AND src.team_id=dst.team_id WHERE b.workload_id=$1 AND b.name=$2`, id, key).Scan(&source, &sourceKey, &encrypted)
				if resolutionError != nil {
					return ""
				}
				var raw []byte
				raw, resolutionError = unseal(s.Key, encrypted)
				if resolutionError != nil {
					return ""
				}
				var config workloadInput
				resolutionError = json.Unmarshal(raw, &config)
				if resolutionError != nil {
					return ""
				}
				var ok bool
				resolved, ok = config.Environment[sourceKey]
				if !ok {
					resolutionError = errSharedVariables
					return ""
				}
				resolved, resolutionError = resolve(source, resolved, depth+1)
			} else {
				resolutionError = tx.QueryRow(ctx, `SELECT v.value FROM shared_variables v JOIN workloads w ON w.team_id=v.team_id WHERE w.id=$1 AND v.name=$2 AND (($3='team' AND v.project_id IS NULL AND v.environment_id IS NULL) OR ($3='project' AND v.project_id=w.project_id AND v.environment_id IS NULL) OR ($3='environment' AND v.environment_id=COALESCE((SELECT p.base_environment_id FROM environment_previews p WHERE p.environment_id=w.environment_id AND p.team_id=w.team_id),w.environment_id)))`, id, key, scope).Scan(&encrypted)
				if resolutionError != nil {
					return ""
				}
				var raw []byte
				raw, resolutionError = unseal(s.Key, encrypted)
				resolved = string(raw)
			}
			if resolutionError != nil {
				return ""
			}
			cache[cacheKey] = resolved
			return bounded(reference, resolved)
		})
		if resolutionError != nil || len(result) > 65536 || strings.ContainsRune(result, 0) {
			return "", errSharedVariables
		}
		return result, nil
	}
	resolved := map[string]string{}
	for name, value := range environment {
		resolved[name], err = resolve(workload, value, 0)
		if err != nil {
			return nil, err
		}
	}
	return resolved, nil
}
