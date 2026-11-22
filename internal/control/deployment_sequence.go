package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
)

type deploymentSequence struct {
	Workloads []string `json:"workload_ids"`
	Automatic bool     `json:"automatic"`
}

func (s *Server) deploymentSequence(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		var sequence deploymentSequence
		sequence.Workloads = []string{}
		err := s.DB.QueryRow(r.Context(), "SELECT workload_ids,automatic FROM deployment_sequences WHERE project_id=$1", r.PathValue("id")).Scan(&sequence.Workloads, &sequence.Automatic)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "sequence unavailable", 503)
			return
		}
		respond(w, 200, sequence)
		return
	}
	var sequence deploymentSequence
	if !decode(w, r, &sequence) {
		return
	}
	if sequence.Workloads == nil {
		sequence.Workloads = []string{}
	}
	if len(sequence.Workloads) > 32 || len(sequence.Workloads) == 1 || len(sequence.Workloads) == 0 && sequence.Automatic {
		http.Error(w, "select 2-32 resources, or clear the sequence", 400)
		return
	}
	seen := map[string]bool{}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var repository, branch string
	for _, id := range sequence.Workloads {
		if seen[id] {
			http.Error(w, "resources cannot repeat", 400)
			return
		}
		seen[id] = true
		if !s.requireAccess(w, r, "workload", id, 3) {
			return
		}
		if !s.requirePersistentResource(w, r, id) {
			return
		}
		var exists bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1 AND project_id=$2 AND team_id=$3)", id, r.PathValue("id"), actor(r).Team).Scan(&exists)
		if err != nil || !exists {
			http.Error(w, "resource unavailable in this project", 400)
			return
		}
		if sequence.Automatic {
			var encrypted []byte
			var configuredRepository string
			err = tx.QueryRow(r.Context(), "SELECT w.configuration,g.repository FROM workloads w JOIN github_sources g ON g.workload_id=w.id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE w.id=$1 AND g.automatic", id).Scan(&encrypted, &configuredRepository)
			if err != nil {
				http.Error(w, "enable GitHub automatic deployments on every sequence resource first", 400)
				return
			}
			data, e := unseal(s.Key, encrypted)
			var config workloadInput
			if e != nil || json.Unmarshal(data, &config) != nil || config.Source == nil {
				http.Error(w, "source configuration unavailable", 500)
				return
			}
			if config.Source.Repository != configuredRepository || repository != "" && (repository != configuredRepository || branch != config.Source.Branch) {
				http.Error(w, "automatic sequence resources must use the same GitHub repository and branch", 400)
				return
			}
			repository, branch = configuredRepository, config.Source.Branch
		}

	}
	raw, err := json.Marshal(sequence.Workloads)
	if err != nil {
		http.Error(w, "invalid sequence", 400)
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO deployment_sequences(project_id,workload_ids,automatic) VALUES($1,$2,$3) ON CONFLICT(project_id) DO UPDATE SET workload_ids=EXCLUDED.workload_ids,automatic=EXCLUDED.automatic", r.PathValue("id"), raw, sequence.Automatic)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save sequence", 503)
		return
	}
	respond(w, 200, sequence)
}

// A push only runs a sequence when every step matches that push and is enabled.
// Otherwise suppress its matching members rather than running a partial migration.
func (s *Server) sequenceWebhookRequests(ctx context.Context, tx pgx.Tx, requests []worker.Request) ([]worker.Request, map[string]string, error) {
	rows, err := tx.Query(ctx, "SELECT project_id,workload_ids FROM deployment_sequences WHERE automatic ORDER BY project_id FOR SHARE")
	if err != nil {
		return nil, nil, err
	}
	type savedSequence struct {
		project string
		ids     []string
	}
	var sequences []savedSequence
	for rows.Next() {
		var sequence savedSequence
		if err = rows.Scan(&sequence.project, &sequence.ids); err != nil {
			break
		}
		sequences = append(sequences, sequence)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	included := map[string]bool{}
	predecessors := map[string]string{}
	for _, request := range requests {
		included[request.Workload] = true
	}
	for _, sequence := range sequences {
		ids := sequence.ids
		members, err := tx.Query(ctx, "SELECT id FROM workloads WHERE project_id=$1 AND id=ANY($2)", sequence.project, ids)
		if err != nil {
			return nil, nil, err
		}
		owned := map[string]bool{}
		for members.Next() {
			var id string
			if err = members.Scan(&id); err != nil {
				break
			}
			owned[id] = true
		}
		if err == nil {
			err = members.Err()
		}
		members.Close()
		if err != nil {
			return nil, nil, err
		}
		complete := len(ids) >= 2 && len(owned) == len(ids)
		for _, id := range ids {
			if !included[id] {
				complete = false
			}
		}
		if !complete {
			for _, id := range ids {
				if owned[id] {
					delete(included, id)
				}
			}
			continue
		}
		for i := 1; i < len(ids); i++ {
			predecessors[ids[i]] = ids[i-1]
		}
	}
	out := []worker.Request{}
	for _, request := range requests {
		if included[request.Workload] {
			out = append(out, request)
		}
	}
	return out, predecessors, nil
}
