package control

import (
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func (s *Server) projectStatusShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "project", id, 3) {
		return
	}
	if r.Method == "DELETE" {
		if _, err := s.DB.Exec(r.Context(), `DELETE FROM project_status_shares WHERE project_id=$1`, id); err != nil {
			http.Error(w, "could not revoke sharing", 503)
			return
		}
		respond(w, 200, apiOK{OK: true})
		return
	}
	if r.Method == "POST" {
		var in struct {
			Title       string `json:"title"`
			Days        int    `json:"days"`
			Acknowledge bool   `json:"acknowledge"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		if !in.Acknowledge || in.Title == "" || len(in.Title) > 80 || !utf8.ValidString(in.Title) || strings.ContainsAny(in.Title, "\x00\r\n") || in.Days < 1 || in.Days > 365 {
			http.Error(w, "acknowledge public deployment status, a title up to 80 bytes and expiry of 1-365 days", 400)
			return
		}
		token := randomToken()
		expires := time.Now().UTC().Add(time.Duration(in.Days) * 24 * time.Hour)
		if _, err := s.DB.Exec(r.Context(), `INSERT INTO project_status_shares(project_id,title,token_hash,expires_at,creator_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(project_id) DO UPDATE SET title=EXCLUDED.title,token_hash=EXCLUDED.token_hash,expires_at=EXCLUDED.expires_at,created_at=now(),creator_id=EXCLUDED.creator_id`, id, in.Title, tokenHash(token), expires, actor(r).ID); err != nil {
			http.Error(w, "could not enable sharing", 503)
			return
		}
		respond(w, 201, map[string]any{"title": in.Title, "expires_at": expires, "dashboard_url": s.PublicURL + "/shared/" + token, "badge_url": s.PublicURL + "/status/" + token + "/badge.svg"})
		return
	}
	var title string
	var expires, created time.Time
	var ownerAllowed bool
	err := s.DB.QueryRow(r.Context(), `SELECT s.title,s.expires_at,s.created_at,cove_automation_allowed(s.creator_id,p.team_id,p.id,'','edit_configuration') FROM project_status_shares s JOIN projects p ON p.id=s.project_id WHERE s.project_id=$1`, id).Scan(&title, &expires, &created, &ownerAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, map[string]any{"enabled": false})
		return
	}
	if err != nil {
		http.Error(w, "sharing unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"enabled": ownerAllowed && expires.After(time.Now()), "ownership_required": !ownerAllowed, "title": title, "expires_at": expires, "created_at": created})
}

var statusBadgeTemplate = template.Must(template.New("badge").Parse(`<svg xmlns="http://www.w3.org/2000/svg" width="220" height="24" role="img" aria-label="Deployment status: {{.}}"><title>Deployment status: {{.}}</title><rect width="220" height="24" fill="#262626"/><text x="8" y="16" fill="white" font-family="sans-serif" font-size="12">Cove deployment: {{.}}</text></svg>`))

func (s *Server) publicProjectStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'none'; sandbox")
	token := r.PathValue("share")
	if len(token) < 32 || len(token) > 128 {
		http.NotFound(w, r)
		return
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	if !s.limitAttempts(w, r, "status-ip:"+hex.EncodeToString(tokenHash(address)), 300) {
		return
	}
	var title string
	var counts struct {
		Total     int `json:"total"`
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Pending   int `json:"pending"`
		Stopped   int `json:"stopped"`
		Unknown   int `json:"unknown"`
	}
	err = s.DB.QueryRow(r.Context(), `SELECT s.title,count(w.id),
 count(w.id) FILTER(WHERE NOT w.manually_stopped AND o.state='succeeded'),
 count(w.id) FILTER(WHERE o.state IN ('failed','interrupted')),
 count(w.id) FILTER(WHERE o.state IN ('queued','running')),
 count(w.id) FILTER(WHERE w.manually_stopped AND o.state='succeeded'),
 count(w.id) FILTER(WHERE o.state IS NULL OR o.state NOT IN ('succeeded','failed','interrupted','queued','running'))
 FROM project_status_shares s JOIN projects p ON p.id=s.project_id LEFT JOIN workloads w ON w.project_id=s.project_id
 LEFT JOIN LATERAL(SELECT state FROM operations WHERE workload_id=w.id AND action IN ('deploy','rollback','adopt','start','restart','stop') ORDER BY created_at DESC,id DESC LIMIT 1)o ON true
 WHERE s.token_hash=$1 AND s.expires_at>now() AND cove_automation_allowed(s.creator_id,p.team_id,p.id,'','edit_configuration') GROUP BY s.title`, tokenHash(token)).Scan(&title, &counts.Total, &counts.Succeeded, &counts.Failed, &counts.Pending, &counts.Stopped, &counts.Unknown)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "status unavailable", 503)
		return
	}
	state := "unknown"
	switch {
	case counts.Failed > 0:
		state = "failed"
	case counts.Pending > 0:
		state = "pending"
	case counts.Stopped > 0:
		state = "stopped"
	case counts.Total > 0 && counts.Succeeded == counts.Total:
		state = "succeeded"
	}
	if strings.HasSuffix(r.URL.Path, "/badge.svg") {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		_ = statusBadgeTemplate.Execute(w, state)
		return
	}
	respond(w, 200, map[string]any{"title": title, "state": state, "counts": counts, "as_of": time.Now().UTC(), "description": "Only aggregate results are shared. Resource names and configuration remain private."})
}
