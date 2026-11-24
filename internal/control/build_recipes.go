package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) readBuildRecipe(ctx context.Context, q buildNodeQuery, workload string) (worker.BuildRecipe, error) {
	recipe := worker.BuildRecipe{Engine: "compose"}
	var encrypted []byte
	err := q.QueryRow(ctx, `SELECT configuration FROM workload_build_recipes WHERE workload_id=$1`, workload).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipe, nil
	}
	if err != nil {
		return recipe, err
	}
	if err = s.unsealJSON(encrypted, &recipe); err != nil {
		return recipe, errors.New("build recipe configuration is unavailable")
	}
	return recipe, worker.ValidateBuildRecipe(recipe)
}

func (s *Server) buildRecipe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		s.saveBuildRecipe(w, r)
		return
	}
	var encrypted []byte
	if err := s.DB.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2`, id, actor(r).Team).Scan(&encrypted); err != nil {
		http.Error(w, "resource configuration unavailable", 503)
		return
	}
	var config workloadInput
	if err := s.unsealJSON(encrypted, &config); err != nil {
		http.Error(w, "resource configuration unavailable", 503)
		return
	}
	recipe, err := s.readBuildRecipe(r.Context(), s.DB, id)
	if err != nil {
		http.Error(w, "build recipe unavailable", 503)
		return
	}
	var node, active string
	var supported bool
	err = s.DB.QueryRow(r.Context(), `SELECT n.id,n.state='ready' AND COALESCE((n.capabilities->>'build_recipes')::boolean,false),COALESCE((SELECT id FROM operations WHERE workload_id=w.id AND state IN ('queued','running') ORDER BY created_at LIMIT 1),'') FROM workloads w LEFT JOIN workload_build_nodes b ON b.workload_id=w.id JOIN nodes n ON n.id=COALESCE(b.node_id,w.node_id) WHERE w.id=$1`, id).Scan(&node, &supported, &active)
	if err != nil {
		http.Error(w, "build recipe capabilities unavailable", 503)
		return
	}
	level, err := s.access(r, "workload", id)
	if err != nil {
		http.Error(w, "build recipe permissions unavailable", 503)
		return
	}
	sourceReady := config.Source != nil && config.Source.InlineCompose
	reason := ""
	if !sourceReady {
		reason = "select a Git source using the saved Compose configuration before choosing a recipe"
	} else if !supported {
		reason = "upgrade and verify the effective build worker before using static output or Railpack"
	} else if recipe.Engine != "compose" {
		if err := worker.ValidateRecipeSource(recipe, config.Source, config.Compose, config.Kind); err != nil {
			reason = err.Error()
		} else if err := worker.ValidateRecipeSecrets(recipe, config.FileSecrets); err != nil {
			reason = err.Error()
		}
	}
	customBuild, customStart := recipe.BuildCommand != "", recipe.StartCommand != ""
	if level < 3 {
		recipe.BuildCommand, recipe.StartCommand = "", ""
	}
	respond(w, 200, map[string]any{"recipe": recipe, "can_edit": level >= 3, "source_ready": sourceReady, "supported": supported, "reason": reason, "build_node_id": node, "active_operation_id": active, "build_command_set": customBuild, "start_command_set": customStart, "railpack_version": worker.RailpackVersion, "pinned_images": []string{worker.RailpackFrontendImage, worker.RailpackBuilderImage, worker.RailpackRuntimeImage}})
}

func (s *Server) saveBuildRecipe(w http.ResponseWriter, r *http.Request) {
	var recipe worker.BuildRecipe
	if !decode(w, r, &recipe) {
		return
	}
	if err := worker.ValidateBuildRecipe(recipe); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, r.PathValue("id"), actor(r).Team).Scan(&encrypted); err != nil {
		http.Error(w, "resource configuration unavailable", 503)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, r.PathValue("id")).Scan(&active); err != nil || active {
		http.Error(w, "wait for active operations before switching the build recipe", 409)
		return
	}
	if recipe.Engine == "compose" {
		_, err = tx.Exec(r.Context(), `DELETE FROM workload_build_recipes WHERE workload_id=$1`, r.PathValue("id"))
	} else {
		var config workloadInput
		if s.unsealJSON(encrypted, &config) != nil {
			http.Error(w, "resource configuration unavailable", 503)
			return
		}
		if err = worker.ValidateRecipeSource(recipe, config.Source, config.Compose, config.Kind); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err = worker.ValidateRecipeSecrets(recipe, config.FileSecrets); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		node, problem := buildExecutionNode(r.Context(), tx, r.PathValue("id"), "")
		if problem != nil {
			http.Error(w, problem.Error(), 409)
			return
		}
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'build_recipes')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&supported); err != nil || !supported {
			http.Error(w, "upgrade and verify the effective build worker before using build recipes", 409)
			return
		}
		protected, problem := s.sealJSON(recipe)
		if problem != nil {
			http.Error(w, "build recipe could not be protected", 500)
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO workload_build_recipes(workload_id,configuration) VALUES($1,$2) ON CONFLICT(workload_id) DO UPDATE SET configuration=EXCLUDED.configuration,updated_at=now()`, r.PathValue("id"), protected)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "build recipe could not be saved", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) prepareBuildRecipe(ctx context.Context, request *worker.Request) error {
	if request.InfrastructurePrepared || request.Action != "build" && request.Action != "deploy" {
		return nil
	}
	recipe := worker.BuildRecipe{Engine: "compose"}
	var err error
	if request.BuildRecipe != nil {
		recipe = *request.BuildRecipe
	} else {
		recipe, err = s.readBuildRecipe(ctx, s.DB, request.Workload)
	}
	if err != nil || recipe.Engine == "compose" {
		return err
	}
	if request.Action == "deploy" {
		if recipe.Engine == "static" {
			if err := worker.ValidateRecipeSecrets(recipe, request.FileSecrets); err != nil {
				return err
			}
			if request.Kind == "job" || request.Source != nil && !request.Source.InlineCompose {
				return errors.New("the saved static runtime requires a service using saved Compose; clear the recipe to change that")
			}
			request.Compose, err = worker.StaticRuntimeCompose(request.Compose, recipe)
		}
		return err
	}
	if err = worker.ValidateRecipeSource(recipe, request.Source, request.Compose, request.Kind); err != nil {
		return err
	}
	node, err := buildExecutionNode(ctx, s.DB, request.Workload, request.ID)
	if err != nil {
		return err
	}
	var supported bool
	if err = s.DB.QueryRow(ctx, `SELECT COALESCE((capabilities->>'build_recipes')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&supported); err != nil || !supported {
		return errors.New("upgrade and verify the effective build worker before using build recipes")
	}
	request.BuildRecipe = &recipe
	return nil
}
