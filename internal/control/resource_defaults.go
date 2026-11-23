package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) resourceDefaults(w http.ResponseWriter, r *http.Request) {
	environment := r.PathValue("id")
	if environment == "" && r.Method != "GET" && !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator permission required", 403)
		return
	}
	if r.Method == "PUT" {
		var in worker.ServiceLimits
		if !decode(w, r, &in) {
			return
		}
		if err := worker.ValidateLimits(map[string]worker.ServiceLimits{"default": in}); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		query := `UPDATE teams SET resource_default_cpu=$2,resource_default_memory_mb=$3 WHERE id=$1`
		id := actor(r).Team
		if environment != "" {
			query = `UPDATE environments SET resource_default_cpu=$2,resource_default_memory_mb=$3 WHERE id=$1 AND team_id=$4`
			id = environment
		}
		args := []any{id, in.CPUs, in.MemoryMB}
		if environment != "" {
			args = append(args, actor(r).Team)
		}
		result, err := s.DB.Exec(r.Context(), query, args...)
		if err != nil {
			http.Error(w, "resource defaults could not be saved", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.NotFound(w, r)
			return
		}
	}
	var team, local worker.ServiceLimits
	err := s.DB.QueryRow(r.Context(), `SELECT resource_default_cpu,resource_default_memory_mb FROM teams WHERE id=$1`, actor(r).Team).Scan(&team.CPUs, &team.MemoryMB)
	if err == nil && environment != "" {
		err = s.DB.QueryRow(r.Context(), `SELECT resource_default_cpu,resource_default_memory_mb FROM environments WHERE id=$1 AND team_id=$2`, environment, actor(r).Team).Scan(&local.CPUs, &local.MemoryMB)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource defaults unavailable", 503)
		return
	}
	effective := inheritResourceDefaults(team, local)
	configured := team
	if environment != "" {
		configured = local
	}
	respond(w, 200, map[string]any{"configured": configured, "team": team, "effective": effective})
}

func inheritResourceDefaults(team, environment worker.ServiceLimits) worker.ServiceLimits {
	if environment.CPUs > 0 {
		team.CPUs = environment.CPUs
	}
	if environment.MemoryMB > 0 {
		team.MemoryMB = environment.MemoryMB
	}
	return team
}

func (s *Server) prepareResourceDefaults(ctx context.Context, request *worker.Request) error {
	if request.Action != "deploy" {
		return nil
	}
	var team, environment worker.ServiceLimits
	var supported bool
	err := s.DB.QueryRow(ctx, `SELECT t.resource_default_cpu,t.resource_default_memory_mb,COALESCE(e.resource_default_cpu,0),COALESCE(e.resource_default_memory_mb,0),COALESCE((n.capabilities->>'resource_defaults')::boolean,false) FROM workloads w JOIN teams t ON t.id=w.team_id JOIN nodes n ON n.id=w.node_id LEFT JOIN environments e ON e.id=w.environment_id WHERE w.id=$1`, request.Workload).Scan(&team.CPUs, &team.MemoryMB, &environment.CPUs, &environment.MemoryMB, &supported)
	if err != nil {
		return err
	}
	limits := inheritResourceDefaults(team, environment)
	request.DefaultLimits = nil
	if limits.CPUs > 0 || limits.MemoryMB > 0 {
		if !supported {
			return errors.New("upgrade and verify the worker before deploying team or environment resource defaults")
		}
		request.DefaultLimits = &limits
	}
	return nil
}

type placementSuggestion struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	State           string     `json:"state"`
	SampledAt       *time.Time `json:"sampled_at"`
	AvailableCPUs   *float64   `json:"available_cpus"`
	AvailableMemory *uint64    `json:"available_memory_bytes"`
	Fits            *bool      `json:"fits"`
	Notice          string     `json:"notice"`
	score           float64
}

func (s *Server) placementSuggestions(w http.ResponseWriter, r *http.Request) {
	project, environment := r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id")
	if project == "" {
		http.Error(w, "select a project for placement suggestions", 400)
		return
	}
	scope, target := "project", project
	if environment != "" {
		scope, target = "environment", environment
	}
	if !s.requireAccess(w, r, scope, target, 3) {
		return
	}
	if environment != "" && !s.allowManualPlacement(w, r, environment) {
		return
	}
	var matches bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects p WHERE p.id=$1 AND p.team_id=$2 AND ($3='' OR EXISTS(SELECT 1 FROM environments e WHERE e.id=$3 AND e.project_id=p.id AND e.team_id=p.team_id)))`, project, actor(r).Team, environment).Scan(&matches); err != nil {
		http.Error(w, "placement scope unavailable", 503)
		return
	}
	if !matches {
		http.Error(w, "project and environment must belong to the current team", 400)
		return
	}
	needed := worker.ServiceLimits{}
	var err error
	if value := r.URL.Query().Get("cpus"); value != "" {
		needed.CPUs, err = strconv.ParseFloat(value, 64)
	}
	if value := r.URL.Query().Get("memory_mb"); value != "" && err == nil {
		needed.MemoryMB, err = strconv.ParseInt(value, 10, 64)
	}
	if err != nil || worker.ValidateLimits(map[string]worker.ServiceLimits{"placement": needed}) != nil {
		http.Error(w, "use valid CPU and memory requirements", 400)
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT n.id,n.name,n.state,jsonb_build_object('sampled_at',n.capabilities->'metrics'->'sampled_at','cpus',n.capabilities->'metrics'->'cpus','cpu_percent',n.capabilities->'metrics'->'cpu_percent','memory_total',n.capabilities->'metrics'->'memory_total','memory_available',n.capabilities->'metrics'->'memory_available') FROM nodes n JOIN team_nodes tn ON tn.node_id=n.id WHERE tn.team_id=$1 ORDER BY n.name,n.id LIMIT 257`, actor(r).Team)
	if err != nil {
		http.Error(w, "placement measurements unavailable", 503)
		return
	}
	defer rows.Close()
	out := []placementSuggestion{}
	limited := false
	for rows.Next() {
		if len(out) == 256 {
			limited = true
			break
		}
		var item placementSuggestion
		var payload []byte
		if err = rows.Scan(&item.ID, &item.Name, &item.State, &payload); err != nil {
			http.Error(w, "placement measurements unavailable", 503)
			return
		}
		var sample worker.NodeMetrics
		if item.State != "ready" || json.Unmarshal(payload, &sample) != nil || sample.SampledAt.IsZero() || time.Since(sample.SampledAt) > 3*time.Minute || sample.SampledAt.After(time.Now().Add(time.Minute)) || sample.CPUs <= 0 || sample.CPUs > 65536 || sample.MemoryTotal == 0 || sample.MemoryAvailable > sample.MemoryTotal || math.IsNaN(sample.CPUPercent) || math.IsInf(sample.CPUPercent, 0) || sample.CPUPercent < 0 || sample.CPUPercent > 100 {
			item.Notice = "Fresh node capacity is unavailable."
		} else {
			cpu := float64(sample.CPUs) * (1 - sample.CPUPercent/100)
			fits := cpu >= needed.CPUs && sample.MemoryAvailable >= uint64(needed.MemoryMB)*(1<<20)
			item.Fits, item.AvailableCPUs, item.AvailableMemory, item.SampledAt = &fits, &cpu, &sample.MemoryAvailable, &sample.SampledAt
			item.score = math.Min(cpu/float64(sample.CPUs), float64(sample.MemoryAvailable)/float64(sample.MemoryTotal))
			item.Notice = "Measured free capacity; limits are caps, not reservations. Placement remains your choice."
		}
		out = append(out, item)
	}
	if rows.Err() != nil {
		http.Error(w, "placement measurements unavailable", 503)
		return
	}
	sort.SliceStable(out, func(i, j int) bool {
		rank := func(item placementSuggestion) int {
			if item.Fits == nil {
				return 0
			}
			if *item.Fits {
				return 2
			}
			return 1
		}
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) > rank(out[j])
		}
		return out[i].score > out[j].score
	})
	respond(w, 200, map[string]any{"requirements": needed, "nodes": out, "limited": limited})
}
