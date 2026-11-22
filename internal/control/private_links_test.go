package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeLinkWorkers struct {
	mu           sync.Mutex
	fingerprints map[string]string
	applied      map[string]worker.LinkConfig
	unreachable  map[string]bool
	// lostReplies applies the configuration but loses the reply, as an interrupted RPC can.
	lostReplies map[string]int
	failures    map[string]string
}

func (f *fakeLinkWorkers) rpc(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable[node] {
		return worker.RPCResponse{}, errors.New("dial tcp: connection refused")
	}
	switch request.Method {
	case "link_identity":
		return worker.RPCResponse{Links: &worker.LinkStatus{Fingerprint: f.fingerprints[node]}}, nil
	case "links_apply":
		if err := worker.ValidateLinkConfig(*request.Links); err != nil {
			return worker.RPCResponse{}, &RemoteError{Message: err.Error()}
		}
		f.applied[node] = *request.Links
		if f.lostReplies[node] > 0 {
			f.lostReplies[node]--
			return worker.RPCResponse{}, errors.New("ssh: unexpected EOF")
		}
		status := worker.LinkStatus{Digest: request.Links.Digest, Fingerprint: f.fingerprints[node]}
		for _, s := range request.Links.Sources {
			status.Results = append(status.Results, worker.LinkResult{ID: s.ID, Version: s.Version, Role: "source", State: "applied"})
		}
		for _, t := range request.Links.Targets {
			result := worker.LinkResult{ID: t.ID, Version: t.Version, Role: "target", State: "applied"}
			if message := f.failures[t.ID]; message != "" {
				result.State, result.Detail = "error", message
			}
			status.Results = append(status.Results, result)
		}
		return worker.RPCResponse{Links: &status}, nil
	}
	return worker.RPCResponse{}, &RemoteError{Message: "unsupported method"}
}

type graphResponse struct {
	Resources      []graphResource      `json:"resources"`
	Connections    []graphConnection    `json:"connections"`
	SharedNetworks []graphSharedNetwork `json:"shared_networks"`
}

func TestPrivateConnections(t *testing.T) {
	f := newAccountFixture(t)
	workers := &fakeLinkWorkers{fingerprints: map[string]string{"n1": strings.Repeat("1", 64), "n2": strings.Repeat("2", 64)}, applied: map[string]worker.LinkConfig{}, unreachable: map[string]bool{}, lostReplies: map[string]int{}, failures: map[string]string{}}
	f.s.workerRPC = workers.rpc
	ctx := context.Background()
	owner := f.user("link-owner", "owner@example.com")
	stagingEditor := f.user("link-staging", "staging@example.com")
	productionViewer := f.user("link-viewer", "viewer@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('other','Other')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','link-owner','owner','[]'),
 ('default','link-staging','member','[{"scope":"environment","scope_id":"e2","role":"editor"}]'),
 ('default','link-viewer','member','[{"scope":"environment","scope_id":"e1","role":"viewer"}]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES
 ('n1','One','10.0.0.1',22,'',$1,'ready','{"private_links":true}'),('n2','Two','10.0.0.2',22,'',$1,'ready','{"private_links":true}'),('old','Old','10.0.0.3',22,'',$1,'ready','{}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','Project','default'),('q','Elsewhere','default')")
	f.sql("INSERT INTO environments(id,project_id,name,team_id) VALUES('e1','p','Production','default'),('e2','p','Staging','default')")
	seal := func(raw string) []byte {
		data, err := seal(f.s.Key, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	app := seal(`{"name":"App","kind":"service","compose":"services:\n  web:\n    image: busybox:1.37\n"}`)
	database := seal(`{"name":"DB","kind":"service","compose":"services:\n  postgres:\n    image: postgres:17-alpine\n"}`)
	job := seal(`{"name":"Job","kind":"job","job_service":"task","compose":"services:\n  task:\n    image: busybox:1.37\n"}`)
	tailnet := seal(`{"name":"Tail","kind":"service","compose":"services:\n  web:\n    image: busybox:1.37\n","private":{"service":"web","hostname":"tail","port":80,"auth_key":"tskey-auth-x"}}`)
	f.sql(`INSERT INTO workloads(id,name,node_id,configuration,team_id,environment_id,project_id,kind) VALUES
 ('a','A','n1',$1,'default','e1','p','service'),('db','DB','n2',$2,'default','e1','p','service'),
 ('b','B','n1',$1,'default','e2','p','service'),('c','C','n2',$1,'default','e2','p','service'),
 ('d','D','n2',$1,'default','e1','p','service'),('job','Job','n1',$3,'default','e1','p','job'),
 ('tail','Tail','n1',$4,'default','e1','p','service'),('legacy','Legacy','old',$1,'default','e1','p','service'),
 ('ext','External','n1',$1,'default',NULL,'q','service')`, app, database, job, tailnet)
	create := func(user, source, sourceService, target, targetService string, port int, alias string, status int) string {
		t.Helper()
		body := map[string]any{"source_id": source, "source_service": sourceService, "target_id": target, "target_service": targetService, "port": port, "alias": alias}
		w := f.call(user, "default", "POST", "/api/private-connections", body, status)
		if status != 201 {
			return ""
		}
		return jsonString(t, w, "id")
	}
	create(stagingEditor, "b", "web", "db", "postgres", 5432, "db.internal", 403)
	aDB := create(owner, "a", "web", "db", "postgres", 5432, "db.internal", 201)
	bDB := create(owner, "b", "web", "db", "postgres", 5432, "db.internal", 201)
	bC := create(stagingEditor, "b", "web", "c", "web", 8080, "c.internal", 201)
	dC := create(owner, "d", "web", "c", "web", 8080, "c.internal", 201)
	create(owner, "a", "web", "db", "postgres", 5432, "other.internal", 409)
	create(owner, "a", "web", "c", "web", 5432, "db.internal", 409)
	create(owner, "a", "web", "job", "task", 80, "job.internal", 409)
	create(owner, "a", "web", "a", "web", 80, "self.internal", 400)
	create(owner, "a", "web", "db", "missing", 5432, "x.internal", 400)
	create(owner, "a", "web", "db", "postgres", 5432, "Not A Name", 400)
	create(owner, "a", "web", "db", "postgres", 0, "x.internal", 400)
	create(owner, "a", "web", "tail", "web", 80, "tail.internal", 409)
	create(owner, "a", "web", "legacy", "web", 80, "legacy.internal", 409)
	f.call("root", "other", "POST", "/api/private-connections", map[string]any{"source_id": "a", "source_service": "web", "target_id": "db", "target_service": "postgres", "port": 5432, "alias": "x.internal"}, 404)

	graph := func(user string) graphResponse {
		t.Helper()
		var g graphResponse
		path := "/api/projects/p/connections"
		if user == productionViewer {
			path += "?environment=e1"
		}
		if err := json.Unmarshal(f.call(user, "default", "GET", path, nil, 200).Body.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	find := func(g graphResponse, id string) graphConnection {
		t.Helper()
		for _, c := range g.Connections {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("connection %s missing", id)
		return graphConnection{}
	}
	if c := find(graph(owner), aDB); c.State != "pending" || !c.CrossNode || !c.CanEdit {
		t.Fatalf("new connection: %+v", c)
	}
	f.call(productionViewer, "default", "GET", "/api/projects/p/connections", nil, 403)
	f.call(productionViewer, "default", "GET", "/api/projects/p/connections?environment=e2", nil, 403)
	// A production viewer sees staging endpoints only as restricted, without names or the source's hostname.
	viewer := graph(productionViewer)
	for _, r := range viewer.Resources {
		if r.EnvironmentID == "e2" {
			t.Fatal("viewer received a staging resource", r.Name)
		}
	}
	if c := find(viewer, bDB); !c.Source.Restricted || c.Source.Name != "" || c.Alias != "" || c.CanRemove || c.Target.Name != "DB" {
		t.Fatalf("restricted source leaked: %+v", c)
	}
	if c := find(viewer, dC); !c.Target.Restricted || c.Target.Name != "" || c.Detail != "" {
		t.Fatalf("restricted target leaked: %+v", c)
	}
	for _, c := range viewer.Connections {
		if c.ID == bC {
			t.Fatal("viewer received a connection between resources it cannot view")
		}
	}

	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	// Each node holds exactly the explicit grants: nothing for C or D toward DB, and nothing transitive.
	n1, n2 := workers.applied["n1"], workers.applied["n2"]
	sources := map[string]worker.LinkSource{}
	for _, s := range n1.Sources {
		sources[s.ID] = s
	}
	if len(n1.Targets) != 0 || len(n1.Sources) != 3 || sources[aDB].Peer == nil || sources[aDB].Peer.Fingerprint != workers.fingerprints["n2"] || sources[aDB].Peer.Address != "10.0.0.2" || sources[bC].Port != 8080 {
		t.Fatalf("source node configuration: %+v", n1)
	}
	grants := map[string]worker.LinkTarget{}
	for _, target := range n2.Targets {
		grants[target.ID] = target
	}
	if len(n2.Targets) != 4 || grants[aDB].Peer != workers.fingerprints["n1"] || grants[bC].Peer != workers.fingerprints["n1"] || grants[dC].Peer != "" || len(n2.Sources) != 1 || n2.Sources[0].ID != dC || n2.Sources[0].Peer != nil {
		t.Fatalf("target node configuration: %+v", n2)
	}
	for _, target := range n2.Targets {
		if target.Workload == "db" && target.ID != aDB && target.ID != bDB {
			t.Fatal("unexpected grant to DB", target)
		}
	}
	if c := find(graph(owner), aDB); c.State != "applied" {
		t.Fatalf("applied connection: %+v", c)
	}

	// An edit is pending until both nodes confirm the new version, even when the first reply is lost.
	f.call(owner, "default", "PUT", "/api/private-connections/"+aDB, map[string]any{"source_service": "web", "target_service": "postgres", "port": 6432, "alias": "db.internal"}, 200)
	f.call(stagingEditor, "default", "PUT", "/api/private-connections/"+bC, map[string]any{"source_service": "web", "target_service": "web", "port": 8081, "alias": "c.internal"}, 200)
	f.call(owner, "default", "PUT", "/api/private-connections/"+aDB, map[string]any{"source_id": "b", "source_service": "web", "target_service": "postgres", "port": 6432, "alias": "db.internal"}, 400)
	workers.lostReplies["n2"] = 1
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if c := find(graph(owner), aDB); c.State != "pending" || !strings.Contains(c.Detail, "Retrying") {
		t.Fatalf("lost reply should stay pending: %+v", c)
	}
	f.sql("UPDATE node_links SET next_attempt_at=now()")
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if c := find(graph(owner), aDB); c.State != "applied" {
		t.Fatalf("retried connection: %+v", c)
	}
	for _, target := range workers.applied["n2"].Targets {
		if target.ID == aDB && (target.Port != 6432 || target.Version != 2) {
			t.Fatal("edited grant not applied", target)
		}
	}

	// Worker-reported errors and unreachable workers are visible and retried.
	workers.failures[dC] = "The target has no containers yet."
	f.call(owner, "default", "POST", "/api/private-connections/"+dC+"/retry", nil, 202)
	f.call(productionViewer, "default", "POST", "/api/private-connections/"+dC+"/retry", nil, 403)
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if c := find(graph(owner), dC); c.State != "error" || c.Detail != "The target has no containers yet." {
		t.Fatalf("worker error: %+v", c)
	}
	delete(workers.failures, dC)

	// A shared network is shown as broader access beside the explicit connection.
	f.sql("INSERT INTO shared_networks(id,team_id,node_id,name) VALUES('sn','default','n2','legacy-net')")
	f.sql("INSERT INTO workload_networks(workload_id,network_id,service,alias) VALUES('d','sn','web','d'),('c','sn','web','c')")
	owned := graph(owner)
	if c := find(owned, dC); len(c.Broader) != 1 || c.Broader[0] != "shared network legacy-net" || len(owned.SharedNetworks) != 1 {
		t.Fatalf("broader access missing: %+v %+v", c, owned.SharedNetworks)
	}

	// Revocation: the target's editors can remove it, and it stays visible until every node confirms.
	workers.unreachable["n1"] = true
	f.call(productionViewer, "default", "DELETE", "/api/private-connections/"+aDB, nil, 403)
	f.call(owner, "default", "DELETE", "/api/private-connections/"+aDB, nil, 202)
	f.sql("UPDATE node_links SET next_attempt_at=now(),applied_at=now()-interval '1 hour'")
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	for _, target := range workers.applied["n2"].Targets {
		if target.ID == aDB {
			t.Fatal("target node kept a revoked grant")
		}
	}
	if c := find(graph(owner), aDB); c.State != "revoking" || !strings.Contains(c.Detail, "revoked on the target") {
		t.Fatalf("revocation while source node is unreachable: %+v", c)
	}
	workers.unreachable["n1"] = false
	f.sql("UPDATE node_links SET next_attempt_at=now()")
	for range 2 {
		if err := f.s.reconcileLinks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var remaining int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM resource_connections WHERE id=$1", aDB).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("revoked connection was not removed", remaining, err)
	}
	for _, source := range workers.applied["n1"].Sources {
		if source.ID == aDB {
			t.Fatal("source node kept a revoked connection")
		}
	}

	// Deleting a workload removes its connections from every node.
	f.sql("DELETE FROM workloads WHERE id='d'")
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	for _, source := range workers.applied["n2"].Sources {
		if source.ID == dC {
			t.Fatal("deleted workload's connection remained")
		}
	}
	// A worker that cannot be probed reports no capabilities; its changes wait instead of failing.
	f.sql(`UPDATE nodes SET state='unreachable',capabilities='null' WHERE id='n2'`)
	f.call(owner, "default", "PUT", "/api/private-connections/"+bDB, map[string]any{"source_service": "web", "target_service": "postgres", "port": 5433, "alias": "db.internal"}, 200)
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if c := find(graph(owner), bDB); c.State != "pending" || !strings.Contains(c.Detail, "not ready") {
		t.Fatalf("unreachable worker: %+v", c)
	}
	f.sql(`UPDATE nodes SET state='ready',capabilities='{"private_links":true}' WHERE id='n2'`)
	f.sql("UPDATE node_links SET next_attempt_at=now()")
	f.call(owner, "default", "GET", "/api/nodes/n2/private-links", nil, 403)
	var settings struct {
		Fingerprint string `json:"fingerprint"`
		Port        int    `json:"link_port"`
	}
	if err := json.Unmarshal(f.call("root", "default", "PUT", "/api/nodes/n2/private-links", map[string]any{"link_address": "10.1.0.2", "link_port": 48000}, 200).Body.Bytes(), &settings); err != nil || settings.Port != 48000 || settings.Fingerprint != workers.fingerprints["n2"] {
		t.Fatal("node settings", settings, err)
	}
	f.call("root", "default", "PUT", "/api/nodes/n2/private-links", map[string]any{"link_address": "bad host", "link_port": 48000}, 400)
	if err := f.s.reconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	for _, source := range workers.applied["n1"].Sources {
		if source.ID == bDB && (source.Peer.Address != "10.1.0.2" || source.Peer.Port != 48000) {
			t.Fatal("peer address change not applied", source.Peer)
		}
	}
	if workers.applied["n2"].Port != 48000 {
		t.Fatal("node link port not applied")
	}
}
