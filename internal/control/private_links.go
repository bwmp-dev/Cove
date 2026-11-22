package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.yaml.in/yaml/v3"
)

// Private connections grant one source service TCP access to one target service port. The control
// plane stores intent; workers enforce it. Each node receives its complete link configuration, and
// per-connection status rows record what every node last confirmed.

type privateConnectionInput struct {
	SourceID      string `json:"source_id"`
	SourceService string `json:"source_service"`
	TargetID      string `json:"target_id"`
	TargetService string `json:"target_service"`
	Port          int    `json:"port"`
	Alias         string `json:"alias"`
}

func (in *privateConnectionInput) normalize() error {
	in.SourceService, in.TargetService, in.Alias = strings.TrimSpace(in.SourceService), strings.TrimSpace(in.TargetService), strings.ToLower(strings.TrimSpace(in.Alias))
	if !worker.ValidServiceName(in.SourceService) || !worker.ValidServiceName(in.TargetService) {
		return errors.New("enter the Compose service names of the source and target")
	}
	if in.Port < 1 || in.Port > 65535 {
		return errors.New("enter a target port from 1 to 65535")
	}
	if !worker.ValidLinkAlias(in.Alias) {
		return errors.New("the private hostname must be a lowercase DNS name, such as postgres.internal, and cannot start with cove-")
	}
	return nil
}

type connectionEndpoint struct {
	ready   bool
	kind    string
	config  workloadInput
	capable bool
	preview bool
}

func composeServices(c workloadInput) []string {
	if c.Source != nil && !c.Source.InlineCompose {
		return nil
	}
	var parsed struct {
		Services map[string]yaml.Node `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(c.Compose), &parsed) != nil {
		return nil
	}
	services := make([]string, 0, len(parsed.Services))
	for name := range parsed.Services {
		services = append(services, name)
	}
	sort.Strings(services)
	return services
}

func (s *Server) lockConnectionEndpoint(ctx context.Context, tx pgx.Tx, team, id string) (connectionEndpoint, error) {
	var e connectionEndpoint
	var encrypted []byte
	err := tx.QueryRow(ctx, `SELECT w.kind,w.configuration,COALESCE((n.capabilities->>'private_links')::boolean,false),(EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) OR EXISTS(SELECT 1 FROM environment_preview_resources r WHERE r.resource_id=w.id)),n.state='ready' FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, id, team).Scan(&e.kind, &encrypted, &e.capable, &e.preview, &e.ready)
	if err != nil {
		return e, err
	}
	data, err := unseal(s.Key, encrypted)
	if err == nil {
		err = json.Unmarshal(data, &e.config)
	}
	return e, err
}

func checkEndpoint(e connectionEndpoint, role, service string) (int, string) {
	switch {
	case e.preview:
		return 409, "preview resources cannot have manually managed private connections; environment previews copy their production connections"
	case role == "target" && e.kind != "service":
		return 409, "the target must be a service; jobs cannot accept connections"
	// An unreachable worker reports no capabilities; changes for it stay pending until it returns.
	case e.ready && !e.capable:
		return 409, "upgrade and verify the " + role + "'s worker before adding private connections"
	case e.config.Private != nil && e.config.Private.Service == service:
		return 409, "the " + role + " service uses Tailscale access, which replaces its container networking"
	}
	if services := composeServices(e.config); services != nil {
		found := false
		for _, name := range services {
			found = found || name == service
		}
		if !found {
			return 400, "the " + role + " has no Compose service named " + service
		}
	}
	return 0, ""
}

// connectionAccess returns the caller's access to both endpoints. Missing endpoints are reported as
// not found so other teams' resource IDs are not confirmed.
func (s *Server) connectionAccess(w http.ResponseWriter, r *http.Request, source, target string) (int, int, bool) {
	sourceLevel, err := s.access(r, "workload", source)
	var targetLevel int
	if err == nil {
		targetLevel, err = s.access(r, "workload", target)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "resource not found", 404)
		return 0, 0, false
	}
	if err != nil {
		http.Error(w, "authorization unavailable", 503)
		return 0, 0, false
	}
	return sourceLevel, targetLevel, true
}

func connectionConflict(w http.ResponseWriter, err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		http.Error(w, "the source already uses this private hostname and port, or already connects to this target port", 409)
		return true
	}
	return false
}

func (s *Server) createPrivateConnection(w http.ResponseWriter, r *http.Request) {
	var in privateConnectionInput
	if !decode(w, r, &in) {
		return
	}
	if err := in.normalize(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.SourceID == in.TargetID {
		http.Error(w, "services in one resource already share its Compose network", 400)
		return
	}
	sourceLevel, targetLevel, ok := s.connectionAccess(w, r, in.SourceID, in.TargetID)
	if !ok {
		return
	}
	if sourceLevel < 3 || targetLevel < 3 {
		http.Error(w, "creating a connection requires edit access to both the source and the target", 403)
		return
	}
	p := actor(r)
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	endpoints := map[string]connectionEndpoint{}
	ids := []string{in.SourceID, in.TargetID}
	sort.Strings(ids)
	for _, id := range ids {
		if endpoints[id], err = s.lockConnectionEndpoint(r.Context(), tx, p.Team, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "resource not found", 404)
			} else {
				http.Error(w, "resource configuration unavailable", 503)
			}
			return
		}
	}
	for _, check := range []struct{ id, role, service string }{{in.SourceID, "source", in.SourceService}, {in.TargetID, "target", in.TargetService}} {
		if status, message := checkEndpoint(endpoints[check.id], check.role, check.service); status != 0 {
			http.Error(w, message, status)
			return
		}
	}
	id := resourceID()
	_, err = tx.Exec(r.Context(), `INSERT INTO resource_connections(id,team_id,source_id,source_service,target_id,target_service,port,alias,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, p.Team, in.SourceID, in.SourceService, in.TargetID, in.TargetService, in.Port, in.Alias, p.ID)
	if connectionConflict(w, err) {
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save the connection", 503)
		return
	}
	respond(w, 201, map[string]string{"id": id, "state": "pending"})
}

type storedConnection struct {
	source, target string
	removing       bool
}

func (s *Server) lockConnection(w http.ResponseWriter, r *http.Request, tx pgx.Tx) (storedConnection, bool) {
	var c storedConnection
	err := tx.QueryRow(r.Context(), "SELECT source_id,target_id,removing FROM resource_connections WHERE id=$1 AND team_id=$2 FOR UPDATE", r.PathValue("id"), actor(r).Team).Scan(&c.source, &c.target, &c.removing)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return c, false
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return c, false
	}
	return c, true
}

func (s *Server) updatePrivateConnection(w http.ResponseWriter, r *http.Request) {
	var in privateConnectionInput
	if !decode(w, r, &in) {
		return
	}
	if err := in.normalize(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	c, ok := s.lockConnection(w, r, tx)
	if !ok {
		return
	}
	if (in.SourceID != "" && in.SourceID != c.source) || (in.TargetID != "" && in.TargetID != c.target) {
		http.Error(w, "a connection's resources cannot change; remove it and create a new one", 400)
		return
	}
	sourceLevel, targetLevel, ok := s.connectionAccess(w, r, c.source, c.target)
	if !ok {
		return
	}
	if sourceLevel < 3 || targetLevel < 3 {
		http.Error(w, "editing a connection requires edit access to both the source and the target", 403)
		return
	}
	if c.removing {
		http.Error(w, "this connection is being removed", 409)
		return
	}
	for _, check := range []struct{ id, role, service string }{{c.source, "source", in.SourceService}, {c.target, "target", in.TargetService}} {
		endpoint, err := s.lockConnectionEndpoint(r.Context(), tx, actor(r).Team, check.id)
		if err != nil {
			http.Error(w, "resource configuration unavailable", 503)
			return
		}
		if status, message := checkEndpoint(endpoint, check.role, check.service); status != 0 {
			http.Error(w, message, status)
			return
		}
	}
	_, err = tx.Exec(r.Context(), `UPDATE resource_connections SET source_service=$2,target_service=$3,port=$4,alias=$5,version=version+1,updated_at=now() WHERE id=$1`, r.PathValue("id"), in.SourceService, in.TargetService, in.Port, in.Alias)
	if connectionConflict(w, err) {
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save the connection", 503)
		return
	}
	respond(w, 200, map[string]string{"id": r.PathValue("id"), "state": "pending"})
}

// deletePrivateConnection starts revocation. Either endpoint's editors can revoke, so a target's
// owners can always cut access to it.
func (s *Server) deletePrivateConnection(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	c, ok := s.lockConnection(w, r, tx)
	if !ok {
		return
	}
	sourceLevel, targetLevel, ok := s.connectionAccess(w, r, c.source, c.target)
	if !ok {
		return
	}
	if sourceLevel < 3 && targetLevel < 3 {
		http.Error(w, "removing a connection requires edit access to its source or target", 403)
		return
	}
	var managed bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id IN ($1,$2))`, c.source, c.target).Scan(&managed); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if managed {
		http.Error(w, "this connection belongs to an environment preview and is removed with it", 409)
		return
	}
	if !c.removing {
		if _, err = tx.Exec(r.Context(), "UPDATE resource_connections SET removing=true,version=version+1,updated_at=now() WHERE id=$1", r.PathValue("id")); err != nil {
			http.Error(w, "could not remove the connection", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not remove the connection", 503)
		return
	}
	respond(w, 202, map[string]string{"id": r.PathValue("id"), "state": "revoking"})
}

func (s *Server) retryPrivateConnection(w http.ResponseWriter, r *http.Request) {
	var source, target string
	err := s.DB.QueryRow(r.Context(), "SELECT source_id,target_id FROM resource_connections WHERE id=$1 AND team_id=$2", r.PathValue("id"), actor(r).Team).Scan(&source, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	sourceLevel, targetLevel, ok := s.connectionAccess(w, r, source, target)
	if !ok {
		return
	}
	if sourceLevel < 2 && targetLevel < 2 {
		http.Error(w, "retrying requires operator access to the source or target", 403)
		return
	}
	_, err = s.DB.Exec(r.Context(), `INSERT INTO node_links(node_id) SELECT DISTINCT node_id FROM workloads WHERE id IN ($1,$2) ON CONFLICT(node_id) DO UPDATE SET applied_digest='',failures=0,next_attempt_at=now()`, source, target)
	if err != nil {
		http.Error(w, "could not schedule a retry", 503)
		return
	}
	respond(w, 202, map[string]string{"id": r.PathValue("id"), "state": "pending"})
}

type graphResource struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	ProjectID       string   `json:"project_id"`
	Project         string   `json:"project"`
	EnvironmentID   string   `json:"environment_id"`
	Environment     string   `json:"environment"`
	NodeID          string   `json:"node_id"`
	Node            string   `json:"node"`
	NodeState       string   `json:"node_state"`
	ManuallyStopped bool     `json:"manually_stopped"`
	LastAction      string   `json:"last_action"`
	LastState       string   `json:"last_state"`
	Services        []string `json:"services"`
	Permissions     []string `json:"permissions"`
	PrivateLinks    bool     `json:"private_links"`
	Tailscale       string   `json:"tailscale_service,omitempty"`
}

type graphEndpoint struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Service    string `json:"service,omitempty"`
	NodeID     string `json:"node_id,omitempty"`
	Restricted bool   `json:"restricted"`
}

type graphSide struct {
	Role   string `json:"role"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type graphConnection struct {
	ID        string        `json:"id"`
	Source    graphEndpoint `json:"source"`
	Target    graphEndpoint `json:"target"`
	Port      int           `json:"port"`
	Alias     string        `json:"alias,omitempty"`
	Protocol  string        `json:"protocol"`
	CrossNode bool          `json:"cross_node"`
	State     string        `json:"state"`
	Detail    string        `json:"detail,omitempty"`
	Sides     []graphSide   `json:"sides"`
	Broader   []string      `json:"broader_access"`
	CanEdit   bool          `json:"can_edit"`
	CanRemove bool          `json:"can_remove"`
	CanRetry  bool          `json:"can_retry"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type graphSharedNetwork struct {
	Name      string   `json:"name"`
	Resources []string `json:"resources"`
}

type connectionRow struct {
	id, source, sourceService, target, targetService, alias, sourceNode, targetNode string
	port                                                                            int
	version                                                                         int64
	removing                                                                        bool
	updated                                                                         time.Time
}

type statusRow struct {
	node, role, state, detail string
	version                   int64
}

func (s *Server) projectConnections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := actor(r)
	project := r.PathValue("id")
	if environment := r.URL.Query().Get("environment"); environment != "" {
		var inProject bool
		if err := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM environments WHERE id=$1 AND project_id=$2 AND team_id=$3)", environment, project, p.Team).Scan(&inProject); err != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		if !inProject {
			http.NotFound(w, r)
			return
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT c.id,c.source_id,c.source_service,c.target_id,c.target_service,c.port,c.alias,c.version,c.removing,c.updated_at,ws.node_id,wt.node_id
 FROM resource_connections c JOIN workloads ws ON ws.id=c.source_id JOIN workloads wt ON wt.id=c.target_id
 WHERE c.team_id=$2 AND (ws.project_id=$1 OR wt.project_id=$1) ORDER BY c.created_at,c.id`, project, p.Team)
	if err != nil {
		http.Error(w, "connections unavailable", 503)
		return
	}
	var connections []connectionRow
	endpointIDs := []string{}
	for rows.Next() {
		var c connectionRow
		if err = rows.Scan(&c.id, &c.source, &c.sourceService, &c.target, &c.targetService, &c.port, &c.alias, &c.version, &c.removing, &c.updated, &c.sourceNode, &c.targetNode); err != nil {
			break
		}
		connections = append(connections, c)
		endpointIDs = append(endpointIDs, c.source, c.target)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "connections unavailable", 503)
		return
	}
	rows, err = s.DB.Query(ctx, `SELECT w.id,w.name,w.kind,w.project_id,pr.name,COALESCE(w.environment_id,''),COALESCE(e.name,''),w.node_id,n.name,n.state,w.manually_stopped,w.configuration,
 COALESCE((n.capabilities->>'private_links')::boolean,false),
 CASE WHEN $3 THEN 3 ELSE cove_access($2,w.team_id,w.project_id,COALESCE(w.environment_id,'')) END,
 COALESCE(o.action,''),COALESCE(o.state,'')
 FROM workloads w JOIN nodes n ON n.id=w.node_id JOIN projects pr ON pr.id=w.project_id LEFT JOIN environments e ON e.id=w.environment_id
 LEFT JOIN LATERAL (SELECT action,state FROM operations WHERE workload_id=w.id ORDER BY created_at DESC LIMIT 1) o ON true
 WHERE w.team_id=$4 AND (w.project_id=$1 OR w.id=ANY($5)) AND NOT EXISTS(SELECT 1 FROM previews pv WHERE pv.workload_id=w.id)
 ORDER BY w.created_at,w.id`, project, p.ID, p.Admin, p.Team, endpointIDs)
	if err != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	resources := []graphResource{}
	visible := map[string]*graphResource{}
	levels := map[string]int{}
	for rows.Next() {
		var g graphResource
		var encrypted []byte
		var level int
		if err = rows.Scan(&g.ID, &g.Name, &g.Kind, &g.ProjectID, &g.Project, &g.EnvironmentID, &g.Environment, &g.NodeID, &g.Node, &g.NodeState, &g.ManuallyStopped, &encrypted, &g.PrivateLinks, &level, &g.LastAction, &g.LastState); err != nil {
			break
		}
		levels[g.ID] = level
		if level < 1 {
			continue
		}
		g.Permissions = permissions(level)
		var config workloadInput
		if data, unsealErr := unseal(s.Key, encrypted); unsealErr == nil && json.Unmarshal(data, &config) == nil {
			g.Services = composeServices(config)
			if config.Private != nil {
				g.Tailscale = config.Private.Service
			}
		}
		if g.Services == nil {
			g.Services = []string{}
		}
		resources = append(resources, g)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "resources unavailable", 503)
		return
	}
	for i := range resources {
		visible[resources[i].ID] = &resources[i]
	}
	ids := make([]string, 0, len(connections))
	for _, c := range connections {
		ids = append(ids, c.id)
	}
	statuses := map[string][]statusRow{}
	nodeErrors := map[string]string{}
	if len(ids) > 0 {
		rows, err = s.DB.Query(ctx, "SELECT connection_id,node_id,role,version,state,detail FROM resource_connection_status WHERE connection_id=ANY($1)", ids)
		if err != nil {
			http.Error(w, "connection status unavailable", 503)
			return
		}
		for rows.Next() {
			var id string
			var st statusRow
			if err = rows.Scan(&id, &st.node, &st.role, &st.version, &st.state, &st.detail); err != nil {
				break
			}
			statuses[id] = append(statuses[id], st)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			http.Error(w, "connection status unavailable", 503)
			return
		}
		rows, err = s.DB.Query(ctx, "SELECT node_id,last_error FROM node_links WHERE failures>0 AND last_error<>''")
		if err != nil {
			http.Error(w, "connection status unavailable", 503)
			return
		}
		for rows.Next() {
			var node, message string
			if err = rows.Scan(&node, &message); err != nil {
				break
			}
			nodeErrors[node] = message
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			http.Error(w, "connection status unavailable", 503)
			return
		}
	}
	// Shared networks give all ports in both directions; the graph shows them so a connection is never
	// presented as exclusive access when a broader path exists.
	shared := map[string][]string{}
	sharedNames := map[string]bool{}
	networks := []graphSharedNetwork{}
	rows, err = s.DB.Query(ctx, `SELECT n.id,n.name,array_agg(DISTINCT wn.workload_id ORDER BY wn.workload_id) FROM shared_networks n JOIN workload_networks wn ON wn.network_id=n.id WHERE n.team_id=$1 GROUP BY n.id,n.name ORDER BY n.name,n.id`, p.Team)
	if err != nil {
		http.Error(w, "networks unavailable", 503)
		return
	}
	for rows.Next() {
		var id, name string
		var members []string
		if err = rows.Scan(&id, &name, &members); err != nil {
			break
		}
		var shown []string
		for _, member := range members {
			if visible[member] != nil {
				shown = append(shown, member)
			}
		}
		for i, a := range members {
			for _, b := range members[i+1:] {
				label := "a shared network you cannot view"
				if visible[a] != nil && visible[b] != nil {
					label = "shared network " + name
				}
				shared[a+"/"+b] = append(shared[a+"/"+b], label)
				shared[b+"/"+a] = append(shared[b+"/"+a], label)
			}
		}
		if len(shown) >= 2 && !sharedNames[id] {
			sharedNames[id] = true
			networks = append(networks, graphSharedNetwork{Name: name, Resources: shown})
		}
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "networks unavailable", 503)
		return
	}
	out := []graphConnection{}
	for _, c := range connections {
		sourceLevel, targetLevel := levels[c.source], levels[c.target]
		if sourceLevel < 1 && targetLevel < 1 {
			continue
		}
		g := graphConnection{ID: c.id, Port: c.port, Protocol: "tcp", CrossNode: c.sourceNode != c.targetNode, UpdatedAt: c.updated, Broader: shared[c.source+"/"+c.target]}
		if g.Broader == nil {
			g.Broader = []string{}
		}
		g.Source = graphEndpoint{Restricted: true}
		if sourceLevel >= 1 {
			g.Source = graphEndpoint{ID: c.source, Name: visible[c.source].Name, Service: c.sourceService, NodeID: c.sourceNode}
			g.Alias = c.alias
		}
		g.Target = graphEndpoint{Restricted: true}
		if targetLevel >= 1 {
			g.Target = graphEndpoint{ID: c.target, Name: visible[c.target].Name, Service: c.targetService, NodeID: c.targetNode}
		}
		g.CanEdit = sourceLevel >= 3 && targetLevel >= 3 && !c.removing
		g.CanRemove = (sourceLevel >= 3 || targetLevel >= 3) && !c.removing
		g.CanRetry = sourceLevel >= 2 || targetLevel >= 2
		g.State, g.Detail, g.Sides = connectionState(c, statuses[c.id], nodeErrors, sourceLevel >= 1, targetLevel >= 1)
		out = append(out, g)
	}
	respond(w, 200, map[string]any{"resources": resources, "connections": out, "shared_networks": networks})
}

// connectionState summarizes what each node last confirmed for the connection's current version.
func connectionState(c connectionRow, rows []statusRow, nodeErrors map[string]string, sourceVisible, targetVisible bool) (string, string, []graphSide) {
	pendingDetail := func(node string) string {
		if message := nodeErrors[node]; message != "" {
			return "Retrying: " + message
		}
		return "Waiting for the worker to apply this change."
	}
	if c.removing {
		detail := "Revoking access on the workers."
		targetRevoked := true
		for _, row := range rows {
			if row.role == "target" {
				targetRevoked = false
			}
			if message := nodeErrors[row.node]; message != "" {
				detail = "Revocation is waiting for a worker. Retrying: " + message
			}
		}
		if targetRevoked && len(rows) > 0 {
			detail = "Access is revoked on the target's node. " + detail
		}
		return "revoking", detail, []graphSide{}
	}
	sides := []graphSide{}
	state, detail := "applied", ""
	for _, side := range []struct {
		role, node string
		visible    bool
	}{{"source", c.sourceNode, sourceVisible}, {"target", c.targetNode, targetVisible}} {
		current := graphSide{Role: side.role, State: "pending", Detail: pendingDetail(side.node)}
		for _, row := range rows {
			if row.node == side.node && row.role == side.role && row.version == c.version && row.state != "sending" {
				current.State, current.Detail = row.state, row.detail
			}
		}
		if current.State == "error" {
			state = "error"
		} else if current.State == "pending" && state != "error" {
			state = "pending"
		}
		if current.Detail != "" && (detail == "" || current.State == "error") {
			detail = current.Detail
		}
		if !side.visible {
			current.Detail = ""
		}
		sides = append(sides, current)
	}
	for _, row := range rows {
		if row.node != c.sourceNode && row.node != c.targetNode && state == "applied" {
			state, detail = "pending", "Revoking access from a previous node."
		}
	}
	if !sourceVisible || !targetVisible {
		// Details may describe the hidden endpoint, so restricted viewers see only the state.
		detail = ""
	}
	return state, detail, sides
}

type linkNode struct {
	id, name, address, mode, state, fingerprint, linkAddress, applied string
	port, failures                                                    int
	capable, due                                                      bool
	appliedAt                                                         *time.Time
}

func (n linkNode) endpoint() string {
	if n.linkAddress != "" {
		return n.linkAddress
	}
	if n.mode == "ssh" {
		return n.address
	}
	return ""
}

type linkEntry struct {
	connection, role string
	version          int64
}

type linkPlan struct {
	config  worker.LinkConfig
	entries []linkEntry
	skipped map[linkEntry]string
}

// reconcileLinks pushes each node's private-link configuration until the node confirms it. A changed
// configuration, a retry request or a periodic refresh triggers a push; unreachable nodes back off.
func (s *Server) reconcileLinks(ctx context.Context) error {
	if _, err := s.DB.Exec(ctx, "DELETE FROM resource_connections c WHERE removing AND NOT EXISTS(SELECT 1 FROM resource_connection_status st WHERE st.connection_id=c.id)"); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `SELECT c.id,c.source_id,c.source_service,c.target_id,c.target_service,c.port,c.alias,c.version,ws.node_id,wt.node_id
 FROM resource_connections c JOIN workloads ws ON ws.id=c.source_id JOIN workloads wt ON wt.id=c.target_id WHERE NOT c.removing ORDER BY c.id`)
	if err != nil {
		return err
	}
	var connections []connectionRow
	nodeSet := map[string]bool{}
	for rows.Next() {
		var c connectionRow
		if err = rows.Scan(&c.id, &c.source, &c.sourceService, &c.target, &c.targetService, &c.port, &c.alias, &c.version, &c.sourceNode, &c.targetNode); err != nil {
			break
		}
		connections = append(connections, c)
		nodeSet[c.sourceNode], nodeSet[c.targetNode] = true, true
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = s.DB.Query(ctx, "SELECT node_id FROM node_links WHERE applied_digest<>'' UNION SELECT node_id FROM resource_connection_status")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		nodeSet[id] = true
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(nodeSet) == 0 {
		return nil
	}
	ids := make([]string, 0, len(nodeSet))
	for id := range nodeSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows, err = s.DB.Query(ctx, `SELECT n.id,n.name,n.address,n.mode,n.state,COALESCE((n.capabilities->>'private_links')::boolean,false),COALESCE(l.fingerprint,''),COALESCE(l.link_address,''),COALESCE(l.link_port,$2),COALESCE(l.applied_digest,''),l.applied_at,COALESCE(l.failures,0),COALESCE(l.next_attempt_at<=now(),true)
 FROM nodes n LEFT JOIN node_links l ON l.node_id=n.id WHERE n.id=ANY($1)`, ids, worker.DefaultLinkPort)
	if err != nil {
		return err
	}
	nodes := map[string]*linkNode{}
	for rows.Next() {
		n := &linkNode{}
		if err = rows.Scan(&n.id, &n.name, &n.address, &n.mode, &n.state, &n.capable, &n.fingerprint, &n.linkAddress, &n.port, &n.applied, &n.appliedAt, &n.failures, &n.due); err != nil {
			break
		}
		nodes[n.id] = n
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, n := range nodes {
		if n.fingerprint == "" && n.capable && n.due {
			response, rpcErr := s.nodeRPC(ctx, n.id, worker.RPCRequest{Method: "link_identity"})
			if rpcErr == nil && (response.Links == nil || !worker.ValidLinkFingerprint(response.Links.Fingerprint)) {
				rpcErr = errors.New("invalid worker response")
			}
			if rpcErr != nil {
				if err = s.linkFailure(ctx, n, rpcErr); err != nil {
					return err
				}
				continue
			}
			n.fingerprint = response.Links.Fingerprint
			if _, err = s.DB.Exec(ctx, "INSERT INTO node_links(node_id,fingerprint) VALUES($1,$2) ON CONFLICT(node_id) DO UPDATE SET fingerprint=EXCLUDED.fingerprint", n.id, n.fingerprint); err != nil {
				return err
			}
		}
	}
	plans := map[string]*linkPlan{}
	for _, id := range ids {
		if n := nodes[id]; n != nil {
			plans[id] = &linkPlan{config: worker.LinkConfig{Port: n.port}, skipped: map[linkEntry]string{}}
		}
	}
	for _, c := range connections {
		source, target := nodes[c.sourceNode], nodes[c.targetNode]
		if source == nil || target == nil {
			continue
		}
		sourcePlan, targetPlan := plans[source.id], plans[target.id]
		sourceEntry, targetEntry := linkEntry{c.id, "source", c.version}, linkEntry{c.id, "target", c.version}
		if source.id == target.id {
			sourcePlan.config.Sources = append(sourcePlan.config.Sources, worker.LinkSource{ID: c.id, Version: c.version, Workload: c.source, Service: c.sourceService, Alias: c.alias, Port: c.port, TargetWorkload: c.target, TargetService: c.targetService})
			sourcePlan.config.Targets = append(sourcePlan.config.Targets, worker.LinkTarget{ID: c.id, Version: c.version, Workload: c.target, Service: c.targetService, Port: c.port})
			sourcePlan.entries = append(sourcePlan.entries, sourceEntry, targetEntry)
			continue
		}
		switch {
		case target.fingerprint == "":
			sourcePlan.skipped[sourceEntry] = "Waiting for the target worker's link identity; upgrade and verify " + target.name + "."
		case target.endpoint() == "" || !worker.ValidLinkAddress(target.endpoint()):
			sourcePlan.skipped[sourceEntry] = "Set a private link address for " + target.name + " in its node settings."
		default:
			sourcePlan.config.Sources = append(sourcePlan.config.Sources, worker.LinkSource{ID: c.id, Version: c.version, Workload: c.source, Service: c.sourceService, Alias: c.alias, Port: c.port, Peer: &worker.LinkPeer{Address: target.endpoint(), Port: target.port, Fingerprint: target.fingerprint}})
			sourcePlan.entries = append(sourcePlan.entries, sourceEntry)
		}
		if source.fingerprint == "" {
			targetPlan.skipped[targetEntry] = "Waiting for the source worker's link identity; upgrade and verify " + source.name + "."
		} else {
			targetPlan.config.Targets = append(targetPlan.config.Targets, worker.LinkTarget{ID: c.id, Version: c.version, Workload: c.target, Service: c.targetService, Port: c.port, Peer: source.fingerprint})
			targetPlan.entries = append(targetPlan.entries, targetEntry)
		}
	}
	semaphore := make(chan struct{}, 4)
	failures := make(chan error, len(plans))
	var wg sync.WaitGroup
	for _, id := range ids {
		n, plan := nodes[id], plans[id]
		if n == nil {
			continue
		}
		semaphore <- struct{}{}
		wg.Go(func() {
			defer func() { <-semaphore }()
			if err := s.pushLinks(ctx, n, plan); err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}

func (s *Server) recordLinkStatus(ctx context.Context, node string, entries []linkEntry, state string, details map[linkEntry]string, onlyNewVersions bool) error {
	for _, entry := range entries {
		query := `INSERT INTO resource_connection_status(connection_id,node_id,role,version,state,detail) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(connection_id,node_id,role) DO UPDATE SET version=EXCLUDED.version,state=EXCLUDED.state,detail=EXCLUDED.detail,updated_at=now()`
		if onlyNewVersions {
			query += " WHERE resource_connection_status.version<>EXCLUDED.version"
		}
		// The connection may have been deleted with its workload since the plan was built.
		if _, err := s.DB.Exec(ctx, query, entry.connection, node, entry.role, entry.version, state, details[entry]); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Server) linkFailure(ctx context.Context, n *linkNode, failure error) error {
	message := "the worker could not be reached"
	var remote *RemoteError
	if errors.As(failure, &remote) {
		message = remote.Message
	}
	delay := 5 * time.Second << min(n.failures, 6)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO node_links(node_id,last_error,failures,next_attempt_at) VALUES($1,$2,1,now()+$3::interval)
 ON CONFLICT(node_id) DO UPDATE SET last_error=EXCLUDED.last_error,failures=node_links.failures+1,next_attempt_at=EXCLUDED.next_attempt_at`, n.id, message, fmt.Sprintf("%d milliseconds", delay.Milliseconds()))
	return err
}

func (s *Server) pushLinks(ctx context.Context, n *linkNode, plan *linkPlan) error {
	var skipped []linkEntry
	for entry := range plan.skipped {
		skipped = append(skipped, entry)
	}
	if !n.capable && n.state != "ready" {
		// An unreachable worker reports no capabilities; keep its changes pending until it is verified again.
		_, err := s.DB.Exec(ctx, `INSERT INTO node_links(node_id,last_error,failures) VALUES($1,$2,1) ON CONFLICT(node_id) DO UPDATE SET last_error=EXCLUDED.last_error,failures=GREATEST(node_links.failures,1)`, n.id, n.name+" is not ready; changes apply when it is reachable and verified")
		return err
	}
	if !n.capable {
		details := map[linkEntry]string{}
		for _, entry := range append(plan.entries, skipped...) {
			details[entry] = "Upgrade and verify " + n.name + " to use private connections."
		}
		return s.recordLinkStatus(ctx, n.id, append(plan.entries, skipped...), "error", details, false)
	}
	if err := s.recordLinkStatus(ctx, n.id, skipped, "error", plan.skipped, false); err != nil {
		return err
	}
	sort.Slice(plan.config.Sources, func(i, j int) bool { return plan.config.Sources[i].ID < plan.config.Sources[j].ID })
	sort.Slice(plan.config.Targets, func(i, j int) bool { return plan.config.Targets[i].ID < plan.config.Targets[j].ID })
	plan.config.Digest = worker.LinkDigest(plan.config)
	empty := len(plan.config.Sources) == 0 && len(plan.config.Targets) == 0
	fresh := n.appliedAt != nil && time.Since(*n.appliedAt) < 2*time.Minute
	if plan.config.Digest == n.applied && (fresh || empty) || !n.due || n.fingerprint == "" {
		return nil
	}
	// Record the attempt before sending: an ambiguous reply may still have applied the change.
	if err := s.recordLinkStatus(ctx, n.id, plan.entries, "sending", nil, true); err != nil {
		return err
	}
	config := plan.config
	response, err := s.nodeRPC(ctx, n.id, worker.RPCRequest{Method: "links_apply", Links: &config})
	// A malformed fingerprint would make every peer's configuration invalid, so it is never stored.
	if err == nil && (response.Links == nil || response.Links.Digest != config.Digest || !worker.ValidLinkFingerprint(response.Links.Fingerprint)) {
		err = errors.New("invalid worker response")
	}
	if err != nil {
		return s.linkFailure(ctx, n, err)
	}
	reported := map[linkEntry]worker.LinkResult{}
	for _, result := range response.Links.Results {
		reported[linkEntry{result.ID, result.Role, result.Version}] = result
	}
	details := map[linkEntry]string{}
	var applied, failed []linkEntry
	for _, entry := range plan.entries {
		result, ok := reported[entry]
		switch {
		case !ok:
			details[entry] = "The worker did not report this connection."
			failed = append(failed, entry)
		case result.State == "applied":
			details[entry] = result.Detail
			applied = append(applied, entry)
		default:
			details[entry] = result.Detail
			failed = append(failed, entry)
		}
	}
	if err = s.recordLinkStatus(ctx, n.id, applied, "applied", details, false); err != nil {
		return err
	}
	if err = s.recordLinkStatus(ctx, n.id, failed, "error", details, false); err != nil {
		return err
	}
	keep := []string{}
	for _, entry := range append(plan.entries, skipped...) {
		keep = append(keep, entry.connection+"/"+entry.role)
	}
	if _, err = s.DB.Exec(ctx, "DELETE FROM resource_connection_status WHERE node_id=$1 AND NOT (connection_id||'/'||role)=ANY($2)", n.id, keep); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO node_links(node_id,fingerprint,applied_digest,applied_at) VALUES($1,$2,$3,now())
 ON CONFLICT(node_id) DO UPDATE SET fingerprint=EXCLUDED.fingerprint,applied_digest=EXCLUDED.applied_digest,applied_at=now(),last_error='',failures=0,next_attempt_at=now()`, n.id, response.Links.Fingerprint, config.Digest)
	return err
}

func (s *Server) nodePrivateLinks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var in struct {
			Address string `json:"link_address"`
			Port    int    `json:"link_port"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Address = strings.TrimSpace(in.Address)
		if (in.Address != "" && !worker.ValidLinkAddress(in.Address)) || in.Port < 1 || in.Port > 65535 {
			http.Error(w, "enter a hostname or IP address and a port from 1 to 65535", 400)
			return
		}
		tag, err := s.DB.Exec(r.Context(), `INSERT INTO node_links(node_id,link_address,link_port) SELECT id,$2,$3 FROM nodes WHERE id=$1
 ON CONFLICT(node_id) DO UPDATE SET link_address=EXCLUDED.link_address,link_port=EXCLUDED.link_port,next_attempt_at=now(),failures=0`, id, in.Address, in.Port)
		if err != nil {
			http.Error(w, "could not save private link settings", 503)
			return
		}
		if tag.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
	}
	var out struct {
		Fingerprint string     `json:"fingerprint"`
		Address     string     `json:"link_address"`
		Port        int        `json:"link_port"`
		Default     string     `json:"default_address"`
		Capable     bool       `json:"capable"`
		AppliedAt   *time.Time `json:"applied_at"`
		LastError   string     `json:"last_error"`
		Connections int        `json:"connections"`
	}
	var mode string
	err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(l.fingerprint,''),COALESCE(l.link_address,''),COALESCE(l.link_port,$2),n.address,n.mode,COALESCE((n.capabilities->>'private_links')::boolean,false),l.applied_at,COALESCE(l.last_error,''),
 (SELECT count(*) FROM resource_connections c JOIN workloads ws ON ws.id=c.source_id JOIN workloads wt ON wt.id=c.target_id WHERE NOT c.removing AND (ws.node_id=n.id OR wt.node_id=n.id))
 FROM nodes n LEFT JOIN node_links l ON l.node_id=n.id WHERE n.id=$1`, id, worker.DefaultLinkPort).Scan(&out.Fingerprint, &out.Address, &out.Port, &out.Default, &mode, &out.Capable, &out.AppliedAt, &out.LastError, &out.Connections)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "private link settings unavailable", 503)
		return
	}
	if mode != "ssh" {
		out.Default = ""
	}
	respond(w, 200, out)
}
