package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type coolifyImportItem struct {
	Kind     string `json:"kind"`
	Resource struct {
		UUID      string `json:"uuid"`
		Name      string `json:"name"`
		BuildPack string `json:"build_pack"`
		Compose   string `json:"docker_compose_raw"`
	} `json:"resource"`
	Variables []struct {
		Key       string  `json:"key"`
		Value     *string `json:"value"`
		RealValue *string `json:"real_value"`
		Preview   bool    `json:"is_preview"`
		Locked    bool    `json:"is_shown_once"`
	} `json:"environment_variables"`
}

func (item *coolifyImportItem) UnmarshalJSON(data []byte) error {
	// Native API exports contain unrelated credential fields; retain only the import allowlist.
	type fields coolifyImportItem
	var selected fields
	if err := json.Unmarshal(data, &selected); err != nil {
		return err
	}
	*item = coolifyImportItem(selected)
	return nil
}

type importCandidate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Blockers    []string          `json:"blockers"`
	Compose     string            `json:"-"`
	Environment map[string]string `json:"-"`
}

type importMapping struct {
	SourceID    string `json:"source_id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	NodeID      string `json:"node_id"`
}

type importDryRunInput struct {
	Mappings []importMapping `json:"mappings"`
}

func discoverCoolify(items []coolifyImportItem) ([]importCandidate, error) {
	if len(items) == 0 || len(items) > 32 {
		return nil, errors.New("provide 1-32 Coolify application or service exports")
	}
	seen := map[string]bool{}
	result := []importCandidate{}
	for _, item := range items {
		if item.Resource.UUID == "" || len(item.Resource.UUID) > 128 || seen[item.Resource.UUID] || len(item.Resource.Name) > 120 {
			return nil, errors.New("exports require unique resource UUIDs and names of at most 120 characters")
		}
		seen[item.Resource.UUID] = true
		candidate := importCandidate{ID: item.Resource.UUID, Name: item.Resource.Name, Kind: item.Kind, Blockers: []string{}, Compose: item.Resource.Compose, Environment: map[string]string{}}
		if item.Kind != "service" && (item.Kind != "application" || item.Resource.BuildPack != "dockercompose") {
			candidate.Blockers = append(candidate.Blockers, "Only Docker Compose applications and services are supported; export a built image and explicit Compose definition for other build packs.")
		}
		if strings.TrimSpace(candidate.Compose) == "" {
			candidate.Blockers = append(candidate.Blockers, "The export is missing plain-text docker_compose_raw. Export with permission to read source secrets; masked or base64-encoded definitions are not accepted.")
		}
		for _, variable := range item.Variables {
			if variable.Preview {
				continue
			}
			value := variable.RealValue
			if value == nil {
				value = variable.Value
			}
			if variable.Locked || value == nil || strings.Contains(*value, "{{") || *value == "********" || *value == "REDACTED" {
				candidate.Blockers = append(candidate.Blockers, "An environment value is locked, masked, missing or still references a Coolify shared/external variable. Supply its actual value in the export.")
				continue
			}
			if _, duplicate := candidate.Environment[variable.Key]; duplicate {
				return nil, errors.New("duplicate production environment variable in export")
			}
			candidate.Environment[variable.Key] = *value
		}
		if len(candidate.Environment) > 256 {
			return nil, errors.New("at most 256 environment variables per imported resource")
		}
		result = append(result, candidate)
	}
	return result, nil
}

type savedImportCandidate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Blockers    []string          `json:"blockers"`
	Compose     string            `json:"compose"`
	Environment map[string]string `json:"environment"`
}

func (s *Server) migrationImports(w http.ResponseWriter, r *http.Request) {
	project, importID := r.PathValue("id"), r.PathValue("import")
	if !s.requireAccess(w, r, "project", project, 3) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, team := r.Context(), actor(r).Team
	if r.Method == "GET" && importID == "" {
		rows, err := s.DB.Query(ctx, `SELECT id,created_at,expires_at FROM migration_imports WHERE project_id=$1 AND team_id=$2 AND expires_at>now() ORDER BY created_at DESC LIMIT 20`, project, team)
		if err != nil {
			http.Error(w, "imports unavailable", 503)
			return
		}
		defer rows.Close()
		items := []struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
			ExpiresAt time.Time `json:"expires_at"`
		}{}
		for rows.Next() {
			var item struct {
				ID        string    `json:"id"`
				CreatedAt time.Time `json:"created_at"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if rows.Scan(&item.ID, &item.CreatedAt, &item.ExpiresAt) != nil {
				http.Error(w, "imports unavailable", 503)
				return
			}
			items = append(items, item)
		}
		if rows.Err() != nil {
			http.Error(w, "imports unavailable", 503)
			return
		}
		respond(w, 200, items)
		return
	}
	if importID == "" {
		var in struct {
			Items []coolifyImportItem `json:"items"`
		}
		if !decode(w, r, &in) {
			return
		}
		candidates, err := discoverCoolify(in.Items)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		stored := []savedImportCandidate{}
		for _, candidate := range candidates {
			stored = append(stored, savedImportCandidate{candidate.ID, candidate.Name, candidate.Kind, candidate.Blockers, candidate.Compose, candidate.Environment})
		}
		sealed, err := s.sealJSON(stored)
		if err != nil {
			http.Error(w, "could not protect import", 503)
			return
		}
		id := resourceID()
		if _, err = s.DB.Exec(ctx, `DELETE FROM migration_imports WHERE project_id=$1 AND expires_at<=now()`, project); err != nil {
			http.Error(w, "could not expire old imports", 503)
			return
		}
		result, err := s.DB.Exec(ctx, `INSERT INTO migration_imports(id,project_id,team_id,payload) SELECT $1,$2,$3,$4 WHERE (SELECT count(*) FROM migration_imports WHERE project_id=$2 AND expires_at>now())<20`, id, project, team, sealed)
		if err != nil {
			http.Error(w, "could not save import", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.Error(w, "delete an old import before discovering another; at most 20 active imports per project", 409)
			return
		}
		respond(w, 201, map[string]any{"id": id, "candidates": candidates, "warnings": importWarnings()})
		return
	}
	if r.Method == "DELETE" {
		result, err := s.DB.Exec(ctx, `DELETE FROM migration_imports WHERE id=$1 AND project_id=$2 AND team_id=$3`, importID, project, team)
		if err != nil {
			http.Error(w, "could not delete import", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, apiOK{true})
		return
	}
	var sealed []byte
	err := s.DB.QueryRow(ctx, `SELECT payload FROM migration_imports WHERE id=$1 AND project_id=$2 AND team_id=$3 AND expires_at>now()`, importID, project, team).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	var stored []savedImportCandidate
	if err != nil || s.unsealJSON(sealed, &stored) != nil {
		http.Error(w, "import unavailable", 503)
		return
	}
	if r.Method == "GET" {
		candidates := []importCandidate{}
		for _, item := range stored {
			candidates = append(candidates, importCandidate{ID: item.ID, Name: item.Name, Kind: item.Kind, Blockers: item.Blockers})
		}
		respond(w, 200, map[string]any{"id": importID, "candidates": candidates, "warnings": importWarnings()})
		return
	}
	var in importDryRunInput
	if !decode(w, r, &in) {
		return
	}
	if len(in.Mappings) == 0 || len(in.Mappings) > len(stored) {
		http.Error(w, "select at least one discovered resource", 400)
		return
	}
	byID := map[string]savedImportCandidate{}
	for _, item := range stored {
		byID[item.ID] = item
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "destination unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	before, err := s.readProjectConfiguration(ctx, tx, team, project)
	if err != nil {
		http.Error(w, "destination unavailable", 503)
		return
	}
	if len(before.Resources)+len(in.Mappings) > 128 {
		http.Error(w, "import would exceed the canonical project limit of 128 resources", 409)
		return
	}
	knownEnvironments := map[string]bool{}
	for _, environment := range before.Environments {
		knownEnvironments[environment.Name] = true
	}
	if err = tx.Rollback(ctx); err != nil {
		http.Error(w, "could not finish reading destination metadata", 503)
		return
	}
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: before.Project, Environments: []configurationEnvironment{}, Resources: []configurationResource{}}
	keys := map[string]bool{}
	for _, resource := range before.Resources {
		keys[resource.Key] = true
	}
	environments := map[string]bool{"": true}
	used := map[string]bool{}
	for _, mapping := range in.Mappings {
		item, ok := byID[mapping.SourceID]
		if !ok || used[item.ID] || !configurationKey.MatchString(mapping.Key) || keys[mapping.Key] || mapping.NodeID == "" {
			http.Error(w, "mapping requires unique discovered resources, unused valid resource keys and an enrolled node", 400)
			return
		}
		if len(item.Blockers) > 0 {
			http.Error(w, "a selected resource has discovery blockers; correct its source export first", 409)
			return
		}
		used[item.ID], keys[mapping.Key] = true, true
		if !environments[mapping.Environment] {
			knownEnvironments[mapping.Environment] = true
			if len(knownEnvironments) > 32 {
				http.Error(w, "import would exceed the canonical project limit of 32 environments", 409)
				return
			}
			manifest.Environments = append(manifest.Environments, configurationEnvironment{Name: mapping.Environment})
			environments[mapping.Environment] = true
		}
		var capable, assigned bool
		err = s.DB.QueryRow(ctx, `SELECT COALESCE((capabilities->>'imports')::boolean,false),EXISTS(SELECT 1 FROM team_nodes WHERE node_id=n.id AND team_id=$2) FROM nodes n WHERE n.id=$1`, mapping.NodeID, team).Scan(&capable, &assigned)
		if err != nil || !capable {
			http.Error(w, "upgrade and verify the selected worker before importing", 409)
			return
		}
		if !actor(r).Admin && !assigned {
			http.Error(w, "node is not assigned to this team", 403)
			return
		}
		response, err := s.nodeRPC(ctx, mapping.NodeID, worker.RPCRequest{Method: "import_compose", Request: &worker.Request{Compose: item.Compose, Environment: item.Environment}})
		if err != nil || response.ImportedCompose == nil {
			http.Error(w, "Compose dry-run failed; flatten supporting files, supply required variables, explicit existing volume names and absolute bind paths, then retry", 409)
			return
		}
		config := workloadInput{Name: mapping.Name, NodeID: mapping.NodeID, Compose: response.ImportedCompose.Compose, Environment: response.ImportedCompose.Environment, Kind: "service"}
		if config.Name == "" {
			config.Name = item.Name
		}
		manifest.Resources = append(manifest.Resources, configurationResource{Key: mapping.Key, Environment: mapping.Environment, Configuration: config, Bindings: []configurationBinding{}})
	}
	if err = validateProjectConfiguration(manifest); err != nil {
		http.Error(w, "mapped configuration is invalid: "+err.Error(), 400)
		return
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		http.Error(w, "could not encode mapped configuration", 503)
		return
	}
	body, _ := json.Marshal(map[string]string{"content": string(content)})
	staging := r.Clone(ctx)
	staging.Method = "POST"
	staging.Body = io.NopCloser(bytes.NewReader(body))
	capture := &apiCapture{headers: http.Header{}}
	s.configurationDrafts(capture, staging)
	if capture.status != 201 || capture.failed {
		http.Error(w, "could not stage import; check project draft limits and current permissions", 409)
		return
	}
	var draft struct {
		ID          string                `json:"id"`
		BaseVersion string                `json:"base_version"`
		Changes     []configurationChange `json:"changes"`
	}
	if json.Unmarshal([]byte(capture.body.String()), &draft) != nil || draft.ID == "" {
		http.Error(w, "staged import unavailable", 503)
		return
	}
	if draft.BaseVersion != s.configurationVersion(before) {
		if _, err = s.DB.Exec(ctx, `DELETE FROM configuration_drafts WHERE id=$1 AND project_id=$2 AND team_id=$3 AND applied_at IS NULL`, draft.ID, project, team); err != nil {
			http.Error(w, "destination changed and the staged draft could not be removed; delete it before retrying", 503)
			return
		}
		http.Error(w, "destination changed during dry-run; map and review the import again", 409)
		return
	}
	respond(w, 201, map[string]any{"draft_id": draft.ID, "base_version": draft.BaseVersion, "changes": draft.Changes, "warnings": importWarnings()})
}

func importWarnings() []string {
	return []string{
		"Import creates a reviewed cove/v1 draft. Apply it explicitly through the configuration draft flow. It creates saved configuration only; it never starts, stops or changes source workloads and never deploys the imported resources.",
		"Verify the source manager's deployment, cleanup and patching automation is paused before a later cutover. Cove cannot verify this from an API export.",
		"All Compose profiles are included. Git connections, proxy routes, middleware, domains, backups, schedules, provider credentials and supporting files are not inferred. Configure and verify them separately; no security middleware is removed from the source.",
		"Persistent data stays at the declared physical volume names and absolute bind paths. Verify destination node, ownership, restore tests and independent backups before deploying. Import does not copy data or change DNS.",
		"Imported string values are stored as sealed COVE_IMPORT variables so source secrets are not exposed by ordinary configuration reads. Later exports require secret-read permission.",
	}
}
