package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var resourceTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func (s *Server) resourceTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var in struct {
			Tags []string `json:"tags"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in.Tags) > 12 {
			http.Error(w, "use at most 12 tags", 400)
			return
		}
		for i, tag := range in.Tags {
			in.Tags[i] = strings.ToLower(strings.TrimSpace(tag))
			if !resourceTagPattern.MatchString(in.Tags[i]) {
				http.Error(w, "tags must contain 1-32 lowercase letters, numbers, dots, underscores or hyphens", 400)
				return
			}
		}
		slices.Sort(in.Tags)
		in.Tags = slices.Compact(in.Tags)
		if in.Tags == nil {
			in.Tags = []string{}
		}
		if _, err := s.DB.Exec(r.Context(), `INSERT INTO workload_tags(workload_id,tags) VALUES($1,$2) ON CONFLICT(workload_id) DO UPDATE SET tags=EXCLUDED.tags`, id, in.Tags); err != nil {
			http.Error(w, "could not save tags", 503)
			return
		}
	}
	var tags []string
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT tags FROM workload_tags WHERE workload_id=$1),ARRAY[]::text[])`, id).Scan(&tags); err != nil {
		http.Error(w, "tags unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"tags": tags})
}

func (s *Server) personalPins(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		var in struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Pinned bool   `json:"pinned"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Kind != "workload" && in.Kind != "project" && in.Kind != "environment" {
			http.Error(w, "pin a project, environment or workload", 400)
			return
		}
		if !s.requireAccess(w, r, in.Kind, in.ID, 1) {
			return
		}
		var err error
		if in.Pinned {
			_, err = s.DB.Exec(r.Context(), `INSERT INTO user_pins(user_id,team_id,kind,resource_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, actor(r).ID, actor(r).Team, in.Kind, in.ID)
		} else {
			_, err = s.DB.Exec(r.Context(), `DELETE FROM user_pins WHERE user_id=$1 AND team_id=$2 AND kind=$3 AND resource_id=$4`, actor(r).ID, actor(r).Team, in.Kind, in.ID)
		}
		if err != nil {
			http.Error(w, "could not save pin", 503)
			return
		}
	}
	rows, err := s.DB.Query(r.Context(), `SELECT u.kind,u.resource_id FROM user_pins u WHERE u.user_id=$1 AND u.team_id=$2 AND (
 (u.kind='workload' AND EXISTS(SELECT 1 FROM workloads w WHERE w.id=u.resource_id AND w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0))) OR
 (u.kind='project' AND EXISTS(SELECT 1 FROM projects p WHERE p.id=u.resource_id AND p.team_id=$2 AND ($3 OR cove_access($1,p.team_id,p.id,'')>0))) OR
 (u.kind='environment' AND EXISTS(SELECT 1 FROM environments e WHERE e.id=u.resource_id AND e.team_id=$2 AND ($3 OR cove_access($1,e.team_id,e.project_id,e.id)>0)))) ORDER BY u.created_at,u.resource_id`, actor(r).ID, actor(r).Team, actor(r).Admin)
	if err != nil {
		http.Error(w, "pins unavailable", 503)
		return
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var kind, id string
		if rows.Scan(&kind, &id) != nil {
			http.Error(w, "pins unavailable", 503)
			return
		}
		items = append(items, map[string]string{"kind": kind, "id": id})
	}
	if rows.Err() != nil {
		http.Error(w, "pins unavailable", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"items": items})
}

type bulkActionResult struct {
	ID          string `json:"id"`
	Status      int    `json:"status"`
	OperationID string `json:"operation_id,omitempty"`
	Error       string `json:"error,omitempty"`
}
type bulkResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *bulkResponse) Header() http.Header { return b.header }
func (b *bulkResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bulkResponse) Write(data []byte) (int, error) {
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(data)
}

func (s *Server) bulkResourceAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs         []string `json:"ids"`
		Reason      string   `json:"reason,omitempty"`
		Action      string   `json:"action"`
		Acknowledge bool     `json:"acknowledge"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validOperationReason(in.Reason) || !in.Acknowledge || len(in.IDs) < 1 || len(in.IDs) > 50 || !slices.Contains([]string{"deploy", "restart", "stop"}, in.Action) {
		http.Error(w, "acknowledge 1-50 resources and choose deploy, restart or stop", 400)
		return
	}
	seen := map[string]bool{}
	for _, id := range in.IDs {
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "/?#") || seen[id] {
			http.Error(w, "resource IDs must be unique and valid", 400)
			return
		}
		seen[id] = true
	}
	items := []bulkActionResult{}
	auditActor := "user " + actor(r).ID
	for _, id := range in.IDs {
		if r.Context().Err() != nil {
			break
		}
		sub := r.Clone(r.Context())
		u := *r.URL
		sub.URL = &u
		sub.URL.Path = "/api/workloads/" + id + "/operations"
		sub.Pattern = "POST /api/workloads/{id}/operations"
		sub.SetPathValue("id", id)
		payload, _ := json.Marshal(map[string]string{"action": in.Action, "reason": in.Reason})
		sub.Body = io.NopCloser(bytes.NewReader(payload))
		sub.ContentLength = int64(len(payload))
		capture := &bulkResponse{header: make(http.Header)}
		s.serveAuthorized(http.HandlerFunc(s.queueOperation), capture, sub, auditActor)
		item := bulkActionResult{ID: id, Status: capture.status}
		if capture.status == 202 {
			var result apiID
			if json.Unmarshal(capture.body.Bytes(), &result) == nil {
				item.OperationID = result.ID
			}
		} else {
			item.Error = strings.TrimSpace(capture.body.String())
			if capture.status == 403 || capture.status == 404 {
				s.recordAudit(sub, auditActor, capture.status)
			}
		}
		items = append(items, item)
	}
	respond(w, 200, map[string]any{"items": items})
}

func browsePage(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	page, size := 1, 25
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil {
			page = 0
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		size, err = strconv.Atoi(raw)
		if err != nil {
			size = 0
		}
	}
	if page < 1 || page > 10000 || size < 1 || size > 100 || len(r.URL.Query().Get("q")) > 200 {
		http.Error(w, "use page 1-10000, page_size 1-100 and search up to 200 bytes", 400)
		return 0, 0, false
	}
	return page, size, true
}

func (s *Server) browseResources(w http.ResponseWriter, r *http.Request) {
	page, size, ok := browsePage(w, r)
	if !ok {
		return
	}
	p := actor(r)
	var raw []byte
	var total int
	err := s.DB.QueryRow(r.Context(), `WITH visible AS (
 SELECT w.id,w.name,w.node_id,n.name AS node,w.kind,w.project_id,p.name AS project,COALESCE(w.environment_id,'') AS environment_id,COALESCE(e.name,'') AS environment,
 cove_permission_names($1,w.team_id,w.project_id,COALESCE(w.environment_id,''),$3,$12) AS permissions,
 COALESCE(t.tags,ARRAY[]::text[]) AS tags,EXISTS(SELECT 1 FROM user_pins u WHERE u.user_id=$1 AND u.team_id=$2 AND u.kind='workload' AND u.resource_id=w.id) AS pinned
 FROM workloads w JOIN nodes n ON n.id=w.node_id JOIN projects p ON p.id=w.project_id LEFT JOIN environments e ON e.id=w.environment_id LEFT JOIN workload_tags t ON t.workload_id=w.id
 WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)
 AND strpos(lower(w.name),lower($4))>0 AND ($5='' OR $5=ANY(COALESCE(t.tags,ARRAY[]::text[])))
 AND ($6='' OR w.project_id=$6) AND ($7='' OR w.node_id=$7) AND ($8='' OR w.environment_id=$8)
) SELECT COALESCE(jsonb_agg(page),'[]'::jsonb),(SELECT count(*) FROM visible WHERE NOT $9 OR pinned)
 FROM (SELECT * FROM visible WHERE NOT $9 OR pinned ORDER BY pinned DESC,lower(name),id LIMIT $10 OFFSET $11)page`, p.ID, p.Team, p.Admin, r.URL.Query().Get("q"), r.URL.Query().Get("tag"), r.URL.Query().Get("project_id"), r.URL.Query().Get("node_id"), r.URL.Query().Get("environment_id"), r.URL.Query().Get("pinned") == "true", size, (page-1)*size, permissionCatalogue).Scan(&raw, &total)
	if err != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	items := []map[string]any{}
	if json.Unmarshal(raw, &items) != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	for _, item := range items {
		raw, err := json.Marshal(item["permissions"])
		var names []string
		if err != nil || json.Unmarshal(raw, &names) != nil {
			http.Error(w, "resources unavailable", 503)
			return
		}
		item["permissions"] = permissionMetadata(r, names)
	}
	respond(w, 200, map[string]any{"items": items, "page": page, "page_size": size, "total": total, "has_more": len(items) == size && page*size < total})
}
