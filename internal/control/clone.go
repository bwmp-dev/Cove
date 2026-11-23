package control

import (
	"cove/internal/catalog"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) clone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name           string `json:"name"`
		IncludeSecrets bool   `json:"include_secrets"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	kind := "workload"
	if strings.HasPrefix(r.URL.Path, "/api/projects/") {
		kind = "project"
	}
	if strings.HasPrefix(r.URL.Path, "/api/environments/") {
		kind = "environment"
	}
	ctx := r.Context()
	p := actor(r)
	source := r.PathValue("id")
	parent := ""
	if kind == "project" {
		if !s.requireAccess(w, r, "team", "", 3) {
			return
		}
	} else if kind == "environment" {
		if err := s.DB.QueryRow(ctx, "SELECT project_id FROM environments WHERE id=$1 AND team_id=$2", source, p.Team).Scan(&parent); err != nil {
			groupError(w, err)
			return
		}
		if !s.requireAccess(w, r, "project", parent, 3) {
			return
		}
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	var present bool
	err = tx.QueryRow(ctx, `SELECT CASE $1
 WHEN 'project' THEN EXISTS(SELECT 1 FROM projects WHERE id=$2 AND team_id=$3)
 WHEN 'environment' THEN EXISTS(SELECT 1 FROM environments WHERE id=$2 AND team_id=$3 AND project_id=$4)
 ELSE EXISTS(SELECT 1 FROM workloads WHERE id=$2 AND team_id=$3) END`, kind, source, p.Team, parent).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !present) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		groupError(w, err)
		return
	}
	id := resourceID()
	environments := map[string]string{}
	if kind == "project" {
		_, err = tx.Exec(ctx, "INSERT INTO projects(id,name,team_id) VALUES($1,$2,$3)", id, in.Name, p.Team)
		if err != nil {
			groupError(w, err)
			return
		}
		rows, e := tx.Query(ctx, "SELECT id,name FROM environments WHERE project_id=$1 AND team_id=$2 ORDER BY id", source, p.Team)
		if e != nil {
			groupError(w, e)
			return
		}
		names := map[string]string{}
		for rows.Next() {
			var old, name string
			if err = rows.Scan(&old, &name); err != nil {
				break
			}
			names[old] = name
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			groupError(w, err)
			return
		}
		for old, name := range names {
			next := resourceID()
			if _, err = tx.Exec(ctx, "INSERT INTO environments(id,project_id,name,team_id) VALUES($1,$2,$3,$4)", next, id, name, p.Team); err != nil {
				groupError(w, err)
				return
			}
			environments[old] = next
		}
	} else if kind == "environment" {
		if _, err = tx.Exec(ctx, "INSERT INTO environments(id,project_id,name,team_id) VALUES($1,$2,$3,$4)", id, parent, in.Name, p.Team); err != nil {
			groupError(w, err)
			return
		}
		environments[source] = id
	}
	rows, err := tx.Query(ctx, `SELECT w.id,w.configuration,COALESCE(w.environment_id,''),w.project_id,w.node_id,
 CASE WHEN $4 THEN 3 ELSE cove_access($5,w.team_id,w.project_id,COALESCE(e.id,'')) END
 FROM workloads w LEFT JOIN environments e ON e.id=w.environment_id
	 WHERE w.team_id=$1 AND (($2='workload' AND w.id=$3) OR ($2='environment' AND e.id=$3) OR ($2='project' AND w.project_id=$3)) ORDER BY w.id`, p.Team, kind, source, p.Admin, p.ID)
	if err != nil {
		groupError(w, err)
		return
	}
	type item struct {
		configuration                  []byte
		id, environment, project, node string
		level                          int
	}
	items := []item{}
	clonedResources := map[string]string{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.configuration, &v.environment, &v.project, &v.node, &v.level); err != nil {
			break
		}
		items = append(items, v)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		groupError(w, err)
		return
	}
	if kind == "workload" && len(items) != 1 {
		http.NotFound(w, r)
		return
	}
	for _, v := range items {
		if v.level < 3 {
			http.Error(w, "permission denied", 403)
			return
		}
		var allowed bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_nodes WHERE team_id=$1 AND node_id=$2)", p.Team, v.node).Scan(&allowed); err != nil {
			groupError(w, err)
			return
		}
		if !p.Admin && !allowed {
			http.Error(w, "a source node is no longer assigned to this team", 403)
			return
		}
		data, e := unseal(s.Key, v.configuration)
		var config workloadInput
		if e != nil || json.Unmarshal(data, &config) != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		config.Autoscale = nil
		config.Routes = nil
		config.Backup = nil
		config.Private = nil
		config.EnvironmentID, config.ProjectID = "", ""
		config.EnvironmentPatch = nil
		config.NodeID = v.node
		if config.Kind == "" {
			config.Kind = "service"
		}
		if !in.IncludeSecrets {
			if config.ComposeBundle != nil && (len(config.ComposeBundle.Files) > 0 || len(config.ComposeBundle.Assets) > 0) {
				config.ComposeBundle = nil
			}
			config.Environment = nil
			if config.Database != nil {
				engine, ok := findDatabaseEngine(config.Database.Engine)
				if !ok {
					http.Error(w, "database engine unavailable", 409)
					return
				}
				definition, _, e := databaseTemplate(engine)
				if e != nil {
					http.Error(w, "database template unavailable", 503)
					return
				}
				config.Environment, e = catalog.Values(definition, nil, false)
				if e != nil {
					http.Error(w, "database credentials unavailable", 503)
					return
				}
			}
			if config.Source != nil {
				config.Source.Token = ""
			}
		}
		next := resourceID()
		target := environments[v.environment]
		project := v.project
		if kind == "project" {
			project = id
		}
		if kind == "workload" {
			next = id
			target = v.environment
			config.Name = in.Name
		}
		if config.Database != nil {
			if err = isolateDatabaseCopy(&config, next); err != nil {
				http.Error(w, "could not isolate database configuration", 503)
				return
			}
		}
		clonedResources[v.id] = next
		data, err = json.Marshal(config)
		if err != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		encrypted, e := seal(s.Key, data)
		if e != nil {
			http.Error(w, "could not protect configuration", 500)
			return
		}
		cpu, memory := configuredUsage(config)
		if _, err = tx.Exec(ctx, "INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10)", next, v.node, config.Name, encrypted, resourceKind(config), target, p.Team, project, cpu, memory); err != nil {
			if quotaError(w, err) {
				return
			}
			groupError(w, err)
			return
		}
		if in.IncludeSecrets {
			if _, err = tx.Exec(ctx, `INSERT INTO workload_connections(workload_id,kind,connection_id) SELECT $1,kind,connection_id FROM workload_connections WHERE workload_id=$2 AND kind='registry'`, next, v.id); err != nil {
				groupError(w, err)
				return
			}
		}
	}
	if in.IncludeSecrets {
		if kind == "project" {
			if _, err = tx.Exec(ctx, `INSERT INTO shared_variables(team_id,project_id,environment_id,name,value) SELECT team_id,$1,NULL,name,value FROM shared_variables WHERE team_id=$2 AND project_id=$3 AND environment_id IS NULL`, id, p.Team, source); err != nil {
				groupError(w, err)
				return
			}
		}
		for old, next := range environments {
			project := parent
			if kind == "project" {
				project = id
			}
			if _, err = tx.Exec(ctx, `INSERT INTO shared_variables(team_id,project_id,environment_id,name,value) SELECT team_id,$1,$2,name,value FROM shared_variables WHERE team_id=$3 AND environment_id=$4`, project, next, p.Team, old); err != nil {
				groupError(w, err)
				return
			}
		}
		for old, next := range clonedResources {
			rows, err := tx.Query(ctx, `SELECT b.name,b.source_id,b.source_key,CASE WHEN $3 THEN 3 ELSE cove_access($2,w.team_id,w.project_id,COALESCE(w.environment_id,'')) END FROM resource_variable_bindings b JOIN workloads w ON w.id=b.source_id WHERE b.workload_id=$1 AND w.team_id=$4`, old, p.ID, p.Admin, p.Team)
			if err != nil {
				groupError(w, err)
				return
			}
			bindings := []resourceVariableBinding{}
			denied := false
			for rows.Next() {
				var b resourceVariableBinding
				var level int
				if err = rows.Scan(&b.Name, &b.SourceID, &b.SourceKey, &level); err != nil {
					break
				}
				if level < 3 {
					denied = true
				}
				if replacement := clonedResources[b.SourceID]; replacement != "" {
					b.SourceID = replacement
				}
				bindings = append(bindings, b)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				groupError(w, err)
				return
			}
			if denied {
				http.Error(w, "a referenced variable source is no longer editable; clone without secrets or ask its administrator", 403)
				return
			}
			for _, b := range bindings {
				if _, err = tx.Exec(ctx, `INSERT INTO resource_variable_bindings(workload_id,name,source_id,source_key) VALUES($1,$2,$3,$4)`, next, b.Name, b.SourceID, b.SourceKey); err != nil {
					groupError(w, err)
					return
				}
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		groupError(w, err)
		return
	}
	respond(w, 201, map[string]string{"id": id})
}
