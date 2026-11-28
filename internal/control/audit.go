package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type statusResponse struct {
	http.ResponseWriter
	status int
}

func (w *statusResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *statusResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (w *statusResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (s *Server) authenticated(next http.Handler, w http.ResponseWriter, r *http.Request, actor string) {
	if r.Method == "GET" {
		next.ServeHTTP(w, r)
		return
	}
	response := &statusResponse{ResponseWriter: w}
	next.ServeHTTP(response, r)
	status := response.status
	if status == 0 {
		status = 200
	}
	// Terminal contents are never audited; session open/close and failed input requests are.
	if r.Pattern != "POST /api/terminal/{id}/input" || status >= 400 {
		s.recordAudit(r, actor, status)
	}
}
func (s *Server) recordAudit(r *http.Request, actor string, status int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if _, err := s.DB.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,$2,$3,$4,(SELECT id FROM teams WHERE id=$5 FOR KEY SHARE))", actor, r.Pattern, r.PathValue("id"), status, auditTeam(r)); err != nil {
		slog.Error("audit event could not be persisted", "action", r.Pattern, "status", status)
	}
}
func (s *Server) auditEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT id,actor,action,subject,status,created_at,details FROM audit_events WHERE team_id=$1 OR ($2 AND team_id IS NULL) ORDER BY id DESC LIMIT 100", actor(r).Team, actor(r).Admin)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type event struct {
		Details json.RawMessage `json:"details"`
		ID      int64           `json:"id"`
		Actor   string          `json:"actor"`
		Action  string          `json:"action"`
		Subject string          `json:"subject"`
		Status  int             `json:"status"`
		Created time.Time       `json:"created_at"`
	}
	events := []event{}
	for rows.Next() {
		var v event
		if rows.Scan(&v.ID, &v.Actor, &v.Action, &v.Subject, &v.Status, &v.Created, &v.Details) != nil {
			http.Error(w, "could not read audit events", 500)
			return
		}
		events = append(events, v)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read audit events", 500)
		return
	}
	respond(w, 200, events)
}

func auditTeam(r *http.Request) any {
	if strings.HasPrefix(r.URL.Path, "/api/teams/") {
		return r.PathValue("id")
	}
	if strings.HasPrefix(r.URL.Path, "/api/nodes") || strings.HasPrefix(r.URL.Path, "/api/users") || (r.URL.Path == "/api/settings" || strings.HasPrefix(r.URL.Path, "/api/settings/")) || r.URL.Path == "/api/updates" {
		return nil
	}
	p, _ := r.Context().Value(identityKey{}).(*identity)
	if p == nil || p.Team == "" {
		return nil
	}
	return p.Team
}
