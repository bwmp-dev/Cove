package control

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
)

type nodeCost struct {
	MonthlyMinor int64  `json:"monthly_minor"`
	Currency     string `json:"currency"`
	Configured   bool   `json:"configured"`
}

func validNodeCost(c nodeCost) bool {
	if c.MonthlyMinor < 0 || c.MonthlyMinor > 100000000000 || len(c.Currency) != 3 {
		return false
	}
	for _, ch := range c.Currency {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}

func (s *Server) nodeCost(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	c := nodeCost{}
	if r.Method == http.MethodPut {
		if !decode(w, r, &c) {
			return
		}
		c.Currency = strings.ToUpper(strings.TrimSpace(c.Currency))
		if !validNodeCost(c) {
			http.Error(w, "select a three-letter currency and a monthly amount between zero and 100000000000 minor units", 400)
			return
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO node_costs(node_id,monthly_minor,currency) VALUES($1,$2,$3) ON CONFLICT(node_id) DO UPDATE SET monthly_minor=EXCLUDED.monthly_minor,currency=EXCLUDED.currency`, id, c.MonthlyMinor, c.Currency); err != nil {
			http.Error(w, "could not save node cost", 503)
			return
		}
	}
	err := s.DB.QueryRow(ctx, `SELECT monthly_minor,currency FROM node_costs WHERE node_id=$1`, id).Scan(&c.MonthlyMinor, &c.Currency)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "node cost unavailable", 503)
		return
	}
	c.Configured = err == nil
	respond(w, 200, c)
}

func (s *Server) projectCosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$2)`, r.PathValue("id"), actor(r).Team).Scan(&exists); err != nil {
		http.Error(w, "project unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	rows, err := s.DB.Query(ctx, `WITH placements AS (SELECT node_id,count(*) AS total FROM workloads GROUP BY node_id) SELECT COALESCE(w.environment_id,''),COALESCE(e.name,''),n.id,n.name,c.currency,(c.monthly_minor::numeric*count(*)/p.total)::double precision,count(*) FROM workloads w JOIN nodes n ON n.id=w.node_id JOIN placements p ON p.node_id=w.node_id LEFT JOIN environments e ON e.id=w.environment_id LEFT JOIN node_costs c ON c.node_id=w.node_id WHERE w.project_id=$1 AND w.team_id=$2 GROUP BY w.environment_id,e.name,n.id,n.name,c.currency,c.monthly_minor,p.total ORDER BY e.name NULLS LAST,n.name,n.id`, r.PathValue("id"), actor(r).Team)
	if err != nil {
		http.Error(w, "project costs unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		EnvironmentID   string   `json:"environment_id"`
		EnvironmentName string   `json:"environment_name"`
		NodeID          string   `json:"node_id"`
		NodeName        string   `json:"node_name"`
		Currency        *string  `json:"currency"`
		MonthlyMinor    *float64 `json:"monthly_minor"`
		Resources       int      `json:"resources"`
	}
	out := []item{}
	for rows.Next() {
		var i item
		if rows.Scan(&i.EnvironmentID, &i.EnvironmentName, &i.NodeID, &i.NodeName, &i.Currency, &i.MonthlyMinor, &i.Resources) != nil {
			http.Error(w, "project costs unavailable", 503)
			return
		}
		out = append(out, i)
	}
	if rows.Err() != nil {
		http.Error(w, "project costs unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"allocation": "Equal share per configured resource on a node; currencies are not converted.", "items": out})
}
