package control

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cove/internal/catalog"
	"cove/internal/worker"
)

func (s *Server) saveWorkloadTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required to publish templates", 403)
		return
	}
	var in struct {
		Template catalog.Template `json:"template"`
		Confirm  bool             `json:"confirm"`
		Revision string           `json:"configuration_revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	var encrypted []byte
	if err := s.DB.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2", r.PathValue("id"), actor(r).Team).Scan(&encrypted); err != nil {
		http.Error(w, "source configuration unavailable", 503)
		return
	}
	plain, err := unseal(s.Key, encrypted)
	var config workloadInput
	if err != nil || json.Unmarshal(plain, &config) != nil {
		http.Error(w, "source configuration unavailable", 503)
		return
	}
	if config.Database != nil {
		http.Error(w, "create managed databases from their engine catalogue instead of exporting generated credentials and TLS settings", 409)
		return
	}
	sourceRevision := ""
	if config.Source != nil && !config.Source.InlineCompose {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(90 * time.Second))
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(90 * time.Second))
		request := worker.Request{ID: "template-export", Workload: r.PathValue("id"), Action: "deploy", Source: config.Source}
		if err = s.prepareAppDeployment(r.Context(), &request); err != nil {
			http.Error(w, "source repository credentials unavailable", 502)
			return
		}
		proxy, e := s.outboundConfig(r.Context())
		if e != nil {
			http.Error(w, "outbound configuration unavailable", 503)
			return
		}
		transport := publicWebhookTransport(proxy)
		defer transport.CloseIdleConnections()
		ref := request.Source.Revision
		if ref == "" {
			ref = "refs/heads/" + request.Source.Branch
		}
		data, revision, e := s.fetchTemplateGitFile(r.Context(), request.Source.Repository, ref, request.Source.ComposePath, request.Source.Token, transport)
		if e != nil {
			http.Error(w, "source Compose unavailable; export requires a reachable public HTTPS repository and a self-contained Compose file", 422)
			return
		}
		config.Compose = string(data)
		sourceRevision = revision
	}
	if config.Hooks != nil || config.Private != nil || config.Kind == "job" {
		http.Error(w, "hooks, private access and jobs require a reviewed custom repository template", 409)
		return
	}
	in.Template.FileSecrets = config.FileSecrets
	in.Template.Web = nil
	if len(config.Routes) > 0 {
		route := config.Routes[0]
		in.Template.Web = &catalog.Web{Service: route.Service, Port: route.Port, Route: false}
	}
	t, err := catalog.Export(in.Template, config.Compose)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	revision := fmt.Sprintf("%x", sha256.Sum256(append(append([]byte{}, encrypted...), []byte(sourceRevision)...)))
	if !in.Confirm {
		respond(w, 200, map[string]any{"template": t, "configuration_revision": revision, "warnings": []string{t.Notes}})
		return
	}
	if in.Revision != revision {
		http.Error(w, "source configuration changed; preview the template again", 409)
		return
	}
	data, templateRevision, err := templateRevision(t)
	if err != nil {
		http.Error(w, "template unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "template unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-team-templates:'||$1,0))`, actor(r).Team); err != nil {
		http.Error(w, "template unavailable", 503)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM team_templates WHERE team_id=$1", actor(r).Team).Scan(&count); err != nil {
		http.Error(w, "template unavailable", 503)
		return
	}
	if count >= 250 {
		http.Error(w, "at most 250 saved templates per team", 409)
		return
	}
	var current []byte
	if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE", r.PathValue("id"), actor(r).Team).Scan(&current); err != nil || fmt.Sprintf("%x", sha256.Sum256(append(append([]byte{}, current...), []byte(sourceRevision)...))) != revision {
		http.Error(w, "source configuration changed; preview the template again", 409)
		return
	}
	result, err := tx.Exec(r.Context(), "INSERT INTO team_templates(team_id,id,definition,revision) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", actor(r).Team, t.ID, data, templateRevision)
	if err == nil && result.RowsAffected() == 0 {
		http.Error(w, "a team template with this ID already exists", 409)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'save workload as template',$2,200,$3)", actor(r).ID, t.ID, actor(r).Team)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save template", 503)
		return
	}
	respond(w, 201, map[string]string{"id": t.ID, "revision": templateRevision})
}

func (s *Server) deleteTeamTemplate(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("id")
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "template unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), "DELETE FROM team_templates WHERE team_id=$1 AND id=$2", team, r.PathValue("template"))
	if err == nil && result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'delete team template',$2,200,$3)", actor(r).ID, r.PathValue("template"), team)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not delete template", 503)
		return
	}
	respond(w, 200, map[string]bool{"deleted": true})
}
