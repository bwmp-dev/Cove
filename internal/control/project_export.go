package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

func (s *Server) exportProjectConfiguration(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	if !s.requireAccess(w, r, "project", project, 3) {
		return
	}
	var in struct {
		EnvironmentID string `json:"environment_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	manifest, err := s.readProjectConfiguration(ctx, tx, actor(r).Team, project)
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	ids := map[string]string{}
	for _, resource := range manifest.Resources {
		ids[resource.Key] = resource.ID
	}
	if in.EnvironmentID != "" {
		var name string
		if err = tx.QueryRow(ctx, `SELECT name FROM environments WHERE id=$1 AND project_id=$2 AND team_id=$3 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id=environments.id)`, in.EnvironmentID, project, actor(r).Team).Scan(&name); err != nil {
			http.Error(w, "select a persistent environment in this project", 404)
			return
		}
		selected := []configurationResource{}
		keys := map[string]bool{}
		for _, resource := range manifest.Resources {
			if resource.Environment == name {
				selected = append(selected, resource)
				keys[resource.Key] = true
			}
		}
		for i := range selected {
			for j := range selected[i].Bindings {
				binding := &selected[i].Bindings[j]
				if binding.Resource != "" && !keys[binding.Resource] {
					binding.SourceID = ids[binding.Resource]
					binding.Resource = ""
				}
			}
		}
		manifest.Resources = selected
		for _, environment := range manifest.Environments {
			if environment.Name == name {
				manifest.Environments = []configurationEnvironment{environment}
				break
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	files := map[string]string{}
	warnings := []string{}
	for i := range manifest.Resources {
		resource := &manifest.Resources[i]
		config := resource.Configuration
		id := ids[resource.Key]
		request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Compose: config.Compose, ComposeBundle: config.ComposeBundle, Source: config.Source, Environment: config.Environment, Routes: config.Routes, Kind: config.Kind, JobService: config.JobService}
		prefix := "resources/" + resource.Key
		if config.Source != nil && !config.Source.InlineCompose {
			var capable bool
			if err = s.DB.QueryRow(ctx, `SELECT COALESCE((capabilities->>'source_files')::boolean,false) FROM nodes WHERE id=$1`, config.NodeID).Scan(&capable); err != nil || !capable {
				http.Error(w, "upgrade and verify Git workers before exporting repository Compose files", 409)
				return
			}
			if err = s.prepareAppDeployment(ctx, &request); err != nil {
				http.Error(w, "repository credentials unavailable", 409)
				return
			}
			result, err := s.nodeRPC(ctx, config.NodeID, worker.RPCRequest{Method: "source_files", Request: &request})
			if err != nil || result.SourceFiles == nil {
				http.Error(w, "could not export repository Compose files; check the source selection and worker", 502)
				return
			}
			expected := map[string]bool{config.Source.ComposePath: true}
			if config.ComposeBundle != nil {
				for _, name := range config.ComposeBundle.Paths {
					expected[name] = true
				}
			}
			if !gitCommit.MatchString(result.SourceFiles.Revision) || len(result.SourceFiles.Files) != len(expected) {
				http.Error(w, "worker returned an incomplete repository selection or invalid revision", 502)
				return
			}
			for name, content := range result.SourceFiles.Files {
				if !expected[name] || len(content) > 1<<20 || strings.ContainsRune(content, 0) || path.IsAbs(name) || path.Clean(name) == ".." || strings.HasPrefix(path.Clean(name), "../") || strings.ContainsAny(name, "\\\x00") || len(name) > 1024 {
					http.Error(w, "worker returned an invalid repository path", 502)
					return
				}
				files[path.Join(prefix, name)] = content
			}
			resource.Configuration.Source.Revision = result.SourceFiles.Revision
			warnings = append(warnings, resource.Key+": repository build contexts, included files and env_file assets remain in the Git working tree; check out the exported pinned revision before using these plain Compose files.")
		} else {
			files[prefix+"/compose.yaml"] = config.Compose
			if config.ComposeBundle != nil {
				for _, file := range config.ComposeBundle.Files {
					files[path.Join(prefix, file.Path)] = file.Content
				}
				for name, content := range config.ComposeBundle.Assets {
					files[path.Join(prefix, name)] = content
				}
			}
			if config.Source != nil {
				warnings = append(warnings, resource.Key+": Cove-managed Git Compose requires the repository working tree for build contexts; see the source in cove.yaml.")
			}
		}
		selected := []string{prefix + "/compose.yaml"}
		if config.Source != nil && !config.Source.InlineCompose {
			selected = []string{path.Join(prefix, config.Source.ComposePath)}
			if config.ComposeBundle != nil {
				for _, name := range config.ComposeBundle.Paths {
					selected = append(selected, path.Join(prefix, name))
				}
			}
		} else if config.ComposeBundle != nil {
			for _, file := range config.ComposeBundle.Files {
				selected = append(selected, path.Join(prefix, file.Path))
			}
		}
		for _, name := range selected {
			normalized, err := worker.NormalizeComposeText(files[name])
			if err != nil {
				http.Error(w, "a selected Compose file cannot be exported as valid YAML", 409)
				return
			}
			files[name] = normalized
		}
		// Resolve only saved Cove variables; repository env_file values remain in the source tree.
		if err = s.resolveDeploymentVariables(ctx, &request); err != nil {
			http.Error(w, "could not resolve exported variables; check references and secret providers", 409)
			return
		}
		var release string
		err = s.DB.QueryRow(ctx, `SELECT id FROM operations WHERE workload_id=$1 AND state='succeeded' AND action IN ('deploy','rollback') ORDER BY updated_at DESC LIMIT 1`, id).Scan(&release)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "release metadata unavailable", 503)
			return
		}
		if request.BuiltinDefaults != nil {
			request.BuiltinDefaults[worker.CoveRelease] = release
		}
		values := map[string]string{}
		for name, value := range request.BuiltinDefaults {
			values[name] = value
		}
		for name, value := range request.Environment {
			values[name] = value
		}
		if existing := files[prefix+"/.env"]; strings.TrimSpace(existing) != "" {
			base, err := parseEnvironmentFile(existing)
			if err != nil {
				http.Error(w, "the .env ancillary file cannot be merged with exported variables; use the supported dotenv syntax", 409)
				return
			}
			for name, value := range values {
				base[name] = value
			}
			values = base
		}
		files[prefix+"/.env"] = encodeEnvironmentFile(values)
	}
	public := publicProjectConfiguration(manifest, true)
	raw, err := json.Marshal(public)
	if err != nil || len(raw) > 2<<20 {
		http.Error(w, "project manifest is too large; export one environment at a time", 413)
		return
	}
	var node yaml.Node
	if yaml.Unmarshal(raw, &node) != nil {
		http.Error(w, "could not encode project manifest", 500)
		return
	}
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		http.Error(w, "could not encode project manifest", 500)
		return
	}
	files["cove.yaml"] = string(encoded)
	notes, err := json.Marshal(map[string]any{"warnings": warnings, "secrets_included": true, "runtime_changed": false, "compose_order": "primary file followed by compose_bundle.files or compose_bundle.paths in listed order; use --profile for each saved profile"})
	if err != nil {
		http.Error(w, "could not encode export notes", 500)
		return
	}
	files["export.json"] = string(notes)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	names := make([]string, 0, len(files))
	size := 0
	for name, content := range files {
		size += len(content)
		names = append(names, name)
	}
	if size > 16<<20 {
		http.Error(w, "export exceeds 16 MiB; export one environment at a time", 413)
		return
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			http.Error(w, "could not encode project archive", 500)
			return
		}
		if _, err = entry.Write([]byte(files[name])); err != nil {
			http.Error(w, "could not encode project archive", 500)
			return
		}
	}
	if err = writer.Close(); err != nil {
		http.Error(w, "could not encode project archive", 500)
		return
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'project configuration and secrets exported',$2,200,$3)`, "user "+actor(r).ID, project, actor(r).Team); err != nil {
		http.Error(w, "could not audit project export", 503)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="cove-project.zip"`)
	w.WriteHeader(200)
	_, _ = w.Write(archive.Bytes())
}
