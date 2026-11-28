package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) workloadOwner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, team := r.PathValue("id"), actor(r).Team
	if r.Method == http.MethodGet {
		var out struct {
			UserID     string           `json:"user_id"`
			Name       string           `json:"name"`
			Candidates []ownerCandidate `json:"candidates"`
		}
		out.Candidates = []ownerCandidate{}
		err := s.DB.QueryRow(ctx, `SELECT COALESCE(w.owner_user_id,''),COALESCE(u.name,'') FROM workloads w LEFT JOIN users u ON u.id=w.owner_user_id WHERE w.id=$1 AND w.team_id=$2`, id, team).Scan(&out.UserID, &out.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "owner unavailable", 503)
			return
		}
		level, err := s.access(r, "workload", id)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return
		}
		if level >= 3 {
			rows, err := s.DB.Query(ctx, `SELECT u.id,u.name FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 AND NOT u.disabled ORDER BY u.name,u.id`, team)
			if err != nil {
				http.Error(w, "members unavailable", 503)
				return
			}
			defer rows.Close()
			for rows.Next() {
				var c ownerCandidate
				if err = rows.Scan(&c.UserID, &c.Name); err != nil {
					http.Error(w, "members unavailable", 503)
					return
				}
				out.Candidates = append(out.Candidates, c)
			}
			if rows.Err() != nil {
				http.Error(w, "members unavailable", 503)
				return
			}
		}
		respond(w, 200, out)
		return
	}
	var in struct {
		UserID string `json:"user_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	var found string
	err = tx.QueryRow(ctx, `SELECT id FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, id, team).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if in.UserID != "" {
		err = tx.QueryRow(ctx, `SELECT u.id FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 AND m.user_id=$2 AND NOT u.disabled FOR KEY SHARE OF m,u`, team, in.UserID).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "select an active team member", 400)
			return
		}
		if err != nil {
			http.Error(w, "members unavailable", 503)
			return
		}
	}
	_, err = tx.Exec(ctx, `UPDATE workloads SET owner_user_id=NULLIF($2,'') WHERE id=$1`, id, in.UserID)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "could not save owner", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type ownerCandidate struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
}

func (s *Server) notificationOwnerEmail(ctx context.Context, team, subject string) (string, error) {
	var email string
	err := s.DB.QueryRow(ctx, `SELECT u.email FROM workloads w JOIN team_members m ON m.team_id=w.team_id AND m.user_id=w.owner_user_id JOIN users u ON u.id=m.user_id WHERE w.team_id=$1 AND w.id=$2 AND u.email_verified AND NOT u.disabled AND u.email IS NOT NULL`, team, subject).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return email, err
}
