package control

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"

	"github.com/jackc/pgx/v5"
)

func (s *Server) compareEnvironments(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" || to == "" || from == to {
		http.Error(w, "select two different environments", 400)
		return
	}
	for _, environment := range []string{from, to} {
		if !s.requireAccess(w, r, "environment", environment, 1) {
			return
		}
	}
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "environment comparison unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var firstName, secondName string
	if err = tx.QueryRow(r.Context(), `SELECT a.name,b.name FROM environments a JOIN environments b ON b.project_id=a.project_id AND b.team_id=a.team_id WHERE a.id=$1 AND b.id=$2 AND a.project_id=$3 AND a.team_id=$4 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id IN(a.id,b.id))`, from, to, project, actor(r).Team).Scan(&firstName, &secondName); err != nil {
		http.Error(w, "select two persistent environments in this project", 404)
		return
	}
	manifest, err := s.readProjectConfiguration(r.Context(), tx, actor(r).Team, project)
	if err != nil {
		http.Error(w, "environment configuration unavailable", 503)
		return
	}
	left, right := map[string]configurationResource{}, map[string]configurationResource{}
	names := map[string]bool{}
	for _, resource := range manifest.Resources {
		var target map[string]configurationResource
		if resource.Environment == firstName {
			target = left
		} else if resource.Environment == secondName {
			target = right
		} else {
			continue
		}
		name := resource.Configuration.Name
		if _, exists := target[name]; exists {
			http.Error(w, "resource names must be unique within each environment for comparison", 409)
			return
		}
		target[name] = resource
		names[name] = true
	}
	type comparison struct {
		Name         string              `json:"name"`
		State        string              `json:"state"`
		Fields       []string            `json:"fields"`
		Diffs        []configurationDiff `json:"diffs"`
		FromRevision string              `json:"from_revision"`
		ToRevision   string              `json:"to_revision"`
	}
	items := []comparison{}
	for name := range names {
		a, hasA := left[name]
		b, hasB := right[name]
		item := comparison{Name: name, State: "equal", Fields: []string{}, Diffs: []configurationDiff{}, FromRevision: a.Revision, ToRevision: b.Revision}
		if !hasA {
			item.State = "only_to"
		} else if !hasB {
			item.State = "only_from"
		} else {
			first, _ := json.Marshal(a.Configuration)
			second, _ := json.Marshal(b.Configuration)
			var before, after map[string]json.RawMessage
			_ = json.Unmarshal(first, &before)
			_ = json.Unmarshal(second, &after)
			item.Fields = configurationChanges(before, after)
			item.Diffs = safeConfigurationDiffs(before, after)
			if !reflect.DeepEqual(a.Bindings, b.Bindings) {
				item.Fields = append(item.Fields, "bindings")
			}
			if a.Revision != b.Revision {
				item.Fields = append(item.Fields, "revision")
			}
			if len(item.Fields) > 0 {
				item.State = "different"
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	var first, second configurationEnvironment
	for _, environment := range manifest.Environments {
		if environment.Name == firstName {
			first = environment
		} else if environment.Name == secondName {
			second = environment
		}
	}
	variableNames := map[string]bool{}
	for name := range first.Variables {
		variableNames[name] = true
	}
	for name := range second.Variables {
		variableNames[name] = true
	}
	variables := []string{}
	for name := range variableNames {
		if !reflect.DeepEqual(first.Variables[name], second.Variables[name]) {
			variables = append(variables, name)
		}
	}
	sort.Strings(variables)
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"from": firstName, "to": secondName, "resources": items, "variable_changes": variables, "policy_changed": !reflect.DeepEqual(first.Policy, second.Policy)})
}
