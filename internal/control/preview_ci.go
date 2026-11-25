package control

import (
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

const previewCIMaxResources = 16
const previewCIMaxSlots = 1024
const previewCIMaxDeliveries = 4096

type previewCIBinding struct {
	RegistryID    string   `json:"registry_id"`
	Server        string   `json:"registry_server"`
	Services      []string `json:"services"`
	Configuration string   `json:"configuration"`
}

type previewCIInputs struct {
	Revision      string                       `json:"revision"`
	Identity      string                       `json:"identity"`
	RecipeVersion int64                        `json:"recipe_version"`
	Bindings      map[string]previewCIBinding  `json:"bindings"`
	Images        map[string]map[string]string `json:"images"`
}

type previewCIInputView struct {
	Identity      string                       `json:"identity"`
	Revision      string                       `json:"revision"`
	RecipeVersion int64                        `json:"recipe_version"`
	Images        map[string]map[string]string `json:"images"`
}

type previewCIResource struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Services []string          `json:"services"`
	Registry map[string]string `json:"registry"`
	Eligible bool              `json:"eligible"`
	Reason   string            `json:"reason"`
}

func previewCIHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func previewCIInputIdentity(revision string, images map[string]map[string]string) string {
	return previewCIHash(struct {
		Revision string
		Images   map[string]map[string]string
	}{revision, images})
}

func previewCIServices(compose string) ([]string, error) {
	var document struct {
		Include  any `yaml:"include"`
		Services map[string]struct {
			Extends any `yaml:"extends"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(compose), &document) != nil || document.Include != nil || len(document.Services) == 0 || len(document.Services) > 64 {
		return nil, errors.New("CI images require saved Compose with 1-64 services and no includes or extends")
	}
	names := []string{}
	for name, service := range document.Services {
		if !worker.ValidServiceName(name) || service.Extends != nil {
			return nil, errors.New("CI images require named services without Compose extends")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) previewCIBinding(ctx context.Context, tx pgx.Tx, team, environment, id string, recipe previewRecipe) (previewCIBinding, string, error) {
	var binding previewCIBinding
	var raw []byte
	var name, kind string
	var managed bool
	var capable, deployed bool
	err := tx.QueryRow(ctx, `SELECT w.name,w.kind,w.configuration,EXISTS(SELECT 1 FROM managed_registries m WHERE m.id=w.id),n.state='ready' AND COALESCE((n.capabilities->>'preview_ci_images')::boolean,false),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','rollback') AND o.state='succeeded') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 AND w.environment_id=$3 FOR SHARE OF w`, id, team, environment).Scan(&name, &kind, &raw, &managed, &capable, &deployed)
	if err != nil {
		return binding, name, err
	}
	if kind != "service" || managed || recipe.Resources[id].Exclude {
		return binding, name, errors.New("select an included service resource, not a job or managed registry")
	}
	if !capable {
		return binding, name, errors.New("CI images require a ready worker reporting preview_ci_images support")
	}
	if !deployed {
		return binding, name, errors.New("CI images require a deployed production baseline")
	}
	var config workloadInput
	if s.unsealJSON(raw, &config) != nil {
		return binding, name, errors.New("resource configuration is unavailable")
	}
	if config.Database != nil {
		return binding, name, errors.New("CI images require an unmanaged service; managed databases keep their typed preview copy")
	}
	if config.Source != nil && !config.Source.InlineCompose {
		return binding, name, errors.New("CI images require saved Compose, not a Compose file read from Git")
	}
	binding.Services, err = previewCIServices(config.Compose)
	if err != nil {
		return binding, name, err
	}
	binding.RegistryID, _, binding.Server, err = s.connectedWebhookRegistry(ctx, tx, id)
	if err != nil {
		return binding, name, err
	}
	if binding.RegistryID == "" {
		return binding, name, errors.New("select a team registry connection on the resource first")
	}
	if err = worker.ValidatePreviewRegistryServer(binding.Server); err != nil {
		return binding, name, err
	}
	binding.Configuration = previewCIHash(raw)
	return binding, name, nil
}

func (s *Server) previewCISettings(w http.ResponseWriter, r *http.Request) {
	project, team := r.PathValue("id"), actor(r).Team
	recipe, version, saved, err := s.loadPreviewRecipe(r.Context(), s.DB, project)
	if err != nil {
		http.Error(w, "preview recipe unavailable", 503)
		return
	}
	if !saved {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"configuration": nil, "resources": []previewCIResource{}, "endpoint": "/hooks/preview-images/" + project, "can_manage": false, "setup_reason": "Save a preview recipe first.", "max_resources": previewCIMaxResources, "max_preview_keys": previewCIMaxSlots, "max_deliveries": previewCIMaxDeliveries, "used_preview_keys": 0, "retained_deliveries": 0})
			return
		}
		http.Error(w, "save a preview recipe first", 409)
		return
	}
	if !s.requireAccess(w, r, "environment", recipe.BaseEnvironment, 1) {
		return
	}
	if r.Method != "GET" && (!s.requireAccess(w, r, "project", project, 3) || !s.requireAccess(w, r, "environment", recipe.BaseEnvironment, 3)) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-preview-recipe:'||$1,0))`, project); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	recipe, version, _, err = s.loadPreviewRecipe(r.Context(), tx, project)
	if err != nil {
		http.Error(w, "preview recipe unavailable", 503)
		return
	}
	if r.Method == "DELETE" {
		_, err = tx.Exec(r.Context(), `DELETE FROM preview_ci_credentials WHERE project_id=$1 AND team_id=$2`, project, team)
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not revoke CI credential", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == "PUT" {
		var input struct {
			Resources []string `json:"resource_ids"`
			Days      int      `json:"days"`
		}
		if !decode(w, r, &input) {
			return
		}
		if !recipe.Enabled || input.Days < 1 || input.Days > 365 || len(input.Resources) < 1 || len(input.Resources) > previewCIMaxResources {
			http.Error(w, "enable the preview recipe and choose 1-16 resources with an expiry of 1-365 days", 400)
			return
		}
		bindings := map[string]previewCIBinding{}
		for _, id := range input.Resources {
			if _, duplicate := bindings[id]; duplicate {
				http.Error(w, "resource IDs must be unique", 400)
				return
			}
			binding, _, err := s.previewCIBinding(r.Context(), tx, team, recipe.BaseEnvironment, id, recipe)
			if err != nil {
				http.Error(w, "CI resource scope is unavailable: "+safePreviewCIError(err), 409)
				return
			}
			bindings[id] = binding
		}
		token := randomToken()
		expires := time.Now().Add(time.Duration(input.Days) * 24 * time.Hour)
		raw, _ := json.Marshal(bindings)
		_, err = tx.Exec(r.Context(), `INSERT INTO preview_ci_credentials(project_id,team_id,user_id,token_hash,bindings,recipe_version,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(project_id) DO UPDATE SET team_id=EXCLUDED.team_id,user_id=EXCLUDED.user_id,token_hash=EXCLUDED.token_hash,bindings=EXCLUDED.bindings,recipe_version=EXCLUDED.recipe_version,expires_at=EXCLUDED.expires_at,created_at=now(),last_received_at=NULL`, project, team, actor(r).ID, tokenHash(token), raw, version, expires)
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not issue CI credential", 503)
			return
		}
		respond(w, 201, map[string]any{"token": token, "expires_at": expires})
		return
	}
	resources := []previewCIResource{}
	rows, err := tx.Query(r.Context(), `SELECT id,name FROM workloads WHERE environment_id=$1 AND team_id=$2 ORDER BY name,id LIMIT 65`, recipe.BaseEnvironment, team)
	if err != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	for rows.Next() {
		var v previewCIResource
		if err = rows.Scan(&v.ID, &v.Name); err != nil {
			break
		}
		resources = append(resources, v)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil || len(resources) > 64 {
		http.Error(w, "resource scope exceeds its bound or is unavailable", 409)
		return
	}
	for i := range resources {
		binding, _, problem := s.previewCIBinding(r.Context(), tx, team, recipe.BaseEnvironment, resources[i].ID, recipe)
		resources[i].Services = binding.Services
		if resources[i].Services == nil {
			resources[i].Services = []string{}
		}
		if binding.RegistryID != "" {
			resources[i].Registry = map[string]string{"id": binding.RegistryID, "server": binding.Server}
		}
		resources[i].Eligible = recipe.Enabled && problem == nil
		if problem != nil {
			resources[i].Reason = safePreviewCIError(problem)
		} else if !recipe.Enabled {
			resources[i].Reason = "enable the preview recipe first"
		}
	}
	var raw []byte
	var expires, created time.Time
	var received *time.Time
	var boundVersion int64
	var ownerAllowed bool
	err = tx.QueryRow(r.Context(), `SELECT bindings,recipe_version,expires_at,created_at,last_received_at,EXISTS(SELECT 1 FROM users u WHERE u.id=c.user_id AND cove_automation_allowed(u.id,c.team_id,c.project_id,$3,'edit_configuration') AND (u.is_admin OR cove_access(u.id,c.team_id,c.project_id,$3)>=3)) FROM preview_ci_credentials c WHERE project_id=$1 AND team_id=$2`, project, team, recipe.BaseEnvironment).Scan(&raw, &boundVersion, &expires, &created, &received, &ownerAllowed)
	var configuration any
	if err == nil {
		var bindings map[string]previewCIBinding
		if json.Unmarshal(raw, &bindings) != nil {
			http.Error(w, "CI credential metadata unavailable", 503)
			return
		}
		ids := []string{}
		matches := recipe.Enabled && boundVersion == version && expires.After(time.Now()) && ownerAllowed
		for id, binding := range bindings {
			ids = append(ids, id)
			current, _, problem := s.previewCIBinding(r.Context(), tx, team, recipe.BaseEnvironment, id, recipe)
			matches = matches && problem == nil && previewCIHash(binding) == previewCIHash(current)
		}
		sort.Strings(ids)
		configuration = map[string]any{"resource_ids": ids, "recipe_version": boundVersion, "matches_scope": matches, "expires_at": expires, "created_at": created, "last_received_at": received}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "CI credential metadata unavailable", 503)
		return
	}
	level, _ := s.access(r, "environment", recipe.BaseEnvironment)
	projectLevel, _ := s.access(r, "project", project)
	var used, retained int
	if tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM preview_ci_slots WHERE project_id=$1),(SELECT count(*) FROM preview_ci_deliveries WHERE project_id=$1)`, project).Scan(&used, &retained) != nil {
		http.Error(w, "CI replay metadata unavailable", 503)
		return
	}
	setup := ""
	if !recipe.Enabled {
		setup = "Enable the preview recipe first."
	}
	respond(w, 200, map[string]any{"configuration": configuration, "resources": resources, "endpoint": "/hooks/preview-images/" + project, "can_manage": level >= 3 && projectLevel >= 3, "setup_reason": setup, "max_resources": previewCIMaxResources, "max_preview_keys": previewCIMaxSlots, "max_deliveries": previewCIMaxDeliveries, "used_preview_keys": used, "retained_deliveries": retained})
}

func safePreviewCIError(err error) string {
	if errors.Is(err, pgx.ErrNoRows) {
		return "resource or registry is no longer in the saved scope"
	}
	text := err.Error()
	for _, prefix := range []string{"select an included", "resource configuration", "CI images require", "select a team registry"} {
		if strings.HasPrefix(text, prefix) {
			return text
		}
	}
	return "resource or registry metadata could not be verified"
}
