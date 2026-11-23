package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

type releaseConfigurationChange struct {
	Field   string `json:"field"`
	Changed bool   `json:"changed"`
}

func compareConfiguration(a, b worker.Request) []releaseConfigurationChange {
	out := []releaseConfigurationChange{}
	add := func(field string, left, right any) {
		out = append(out, releaseConfigurationChange{field, !reflect.DeepEqual(left, right)})
	}
	add("compose", a.Compose, b.Compose)
	add("source", sourceWithoutCredentials(a.Source), sourceWithoutCredentials(b.Source))
	token := func(source *worker.GitSource) string {
		if source == nil {
			return ""
		}
		return source.Token
	}
	add("source.credentials", token(a.Source), token(b.Source))
	keys := map[string]bool{}
	for k := range a.Environment {
		keys[k] = true
	}
	for k := range b.Environment {
		keys[k] = true
	}
	for k := range keys {
		av, ae := a.Environment[k]
		bv, be := b.Environment[k]
		out = append(out, releaseConfigurationChange{"variables." + k, ae != be || av != bv})
	}
	add("limits", a.Limits, b.Limits)
	add("routes", a.Routes, b.Routes)
	add("hooks", a.Hooks, b.Hooks)
	add("file_secrets", a.FileSecrets, b.FileSecrets)
	add("private_network", a.Private, b.Private)
	add("registry_credentials", a.Registry, b.Registry)
	add("backup_credentials_and_configuration", a.Backup, b.Backup)
	add("networks", a.Networks, b.Networks)
	add("guarded_deploy", a.GuardedDeploy, b.GuardedDeploy)
	add("compose_bundle", a.ComposeBundle, b.ComposeBundle)
	add("rollout", a.Rollout, b.Rollout)
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func sourceWithoutCredentials(source *worker.GitSource) *worker.GitSource {
	if source == nil {
		return nil
	}
	copy := *source
	copy.Token = ""
	copy.Revision = ""
	return &copy
}

func (s *Server) releaseConfiguration(ctx context.Context, workload, id string) (worker.Request, error) {
	seen := map[string]bool{}
	for len(seen) < 64 {
		if seen[id] {
			return worker.Request{}, errors.New("invalid rollback history")
		}
		seen[id] = true
		var encrypted []byte
		var action, state string
		if err := s.DB.QueryRow(ctx, "SELECT payload,action,state FROM operations WHERE id=$1 AND workload_id=$2", id, workload).Scan(&encrypted, &action, &state); err != nil {
			return worker.Request{}, err
		}
		if state != "succeeded" || action != "deploy" && action != "rollback" {
			return worker.Request{}, errors.New("successful workload release required")
		}
		plain, err := unseal(s.Key, encrypted)
		if err != nil {
			return worker.Request{}, err
		}
		var request worker.Request
		if err = json.Unmarshal(plain, &request); err != nil {
			return request, err
		}
		if action != "rollback" {
			return request, nil
		}
		id = request.Release
	}
	return worker.Request{}, errors.New("rollback history exceeds safe traversal limit")
}

func (s *Server) releaseDiff(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" || to == "" || len(from) > 64 || len(to) > 64 {
		http.Error(w, "two release IDs are required", 400)
		return
	}
	var node string
	var supported bool
	if err := s.DB.QueryRow(ctx, `SELECT w.node_id,COALESCE((n.capabilities->>'release_diff')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, r.PathValue("id")).Scan(&node, &supported); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify the worker to compare retained releases", 409)
		return
	}
	a, err := s.releaseConfiguration(ctx, r.PathValue("id"), from)
	if err != nil {
		http.Error(w, "source release configuration unavailable", 409)
		return
	}
	b, err := s.releaseConfiguration(ctx, r.PathValue("id"), to)
	if err != nil {
		http.Error(w, "target release configuration unavailable", 409)
		return
	}
	var source *worker.GitSource
	if b.Source != nil {
		copy := *b.Source
		source = &copy
		var installation, repository int64
		e := s.DB.QueryRow(ctx, `SELECT g.installation_id,g.repository_id FROM github_sources g JOIN workloads w ON w.id=g.workload_id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.workload_id=$1 AND g.repository=$2`, r.PathValue("id"), source.Repository).Scan(&installation, &repository)
		if e == nil {
			token, e := s.installationToken(ctx, installation, repository)
			if e != nil {
				http.Error(w, "repository credentials unavailable", 502)
				return
			}
			source.Token = token
		} else if !errors.Is(e, pgx.ErrNoRows) {
			http.Error(w, "repository settings unavailable", 503)
			return
		}
	}
	different := a.Source != nil && b.Source != nil && a.Source.Repository != b.Source.Repository
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "release_comparison", Diff: &worker.ReleaseComparisonRequest{Workload: r.PathValue("id"), From: from, To: to, Source: source, DifferentRepositories: different}})
	if err != nil || response.Comparison == nil {
		http.Error(w, "retained release comparison unavailable; check worker connectivity, active operations and repository access", 502)
		return
	}
	configuration := compareConfiguration(a, b)
	for i := range configuration {
		if name, ok := strings.CutPrefix(configuration[i].Field, "variables."); ok {
			configuration[i].Changed = configuration[i].Changed || response.Comparison.EnvironmentChanges[name]
			delete(response.Comparison.EnvironmentChanges, name)
		}
	}
	for name, changed := range response.Comparison.EnvironmentChanges {
		configuration = append(configuration, releaseConfigurationChange{"variables." + name, changed})
	}
	sort.Slice(configuration, func(i, j int) bool { return configuration[i].Field < configuration[j].Field })
	response.Comparison.EnvironmentChanges = nil
	respond(w, 200, map[string]any{"from": from, "to": to, "configuration": configuration, "release": response.Comparison})
}
