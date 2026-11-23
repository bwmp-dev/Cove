package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestRoutePolicyPersistenceAndCapability(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('policy-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('policy-project','Project','default')")
	config := workloadInput{Name: "Policy", NodeID: "policy-node", ProjectID: "policy-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	routes := []worker.Route{{Domain: "app.example.com", Service: "app", Port: 80, Policy: &worker.RoutePolicy{PathPrefix: "/api", SecurityHeaders: true, ResponseHeaders: map[string]string{"X-Version": "1"}, IPAllowList: []string{"fd00::/8"}}}}
	request := map[string]any{"routes": routes}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/routes", request, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true}' WHERE id='policy-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/routes", request, 200)
	response := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	var saved workloadInput
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || !worker.RoutesEqual(saved.Routes, routes) {
		t.Fatal(response.Body.String(), err)
	}
	response = f.call("root", "default", "PUT", "/api/workloads/"+id+"/routes", request, 200)
	if jsonString(t, response, "state") != "unchanged" {
		t.Fatal(response.Body.String())
	}
	req := worker.Request{Workload: id, Action: "deploy", Routes: routes}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='policy-node'`)
	if err := f.s.prepareInfrastructure(context.Background(), &req); err == nil {
		t.Fatal("old worker accepted route policies")
	}
}

func TestRoutePolicyRetainsHiddenAuthentication(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('auth-node','Node','localhost',22,'',$1,'ready','{"route_policies":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('auth-project','Project','default')")
	auth := "user:$2y$10$" + strings.Repeat("a", 53)
	config := workloadInput{Name: "Protected", NodeID: "auth-node", ProjectID: "auth-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Routes: []worker.Route{{Domain: "protected.example.com", Service: "app", Port: 80, Auth: auth}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	response := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	if strings.Contains(response.Body.String(), auth) {
		t.Fatal("route password hash exposed")
	}
	var visible workloadInput
	if err := json.Unmarshal(response.Body.Bytes(), &visible); err != nil {
		t.Fatal(err)
	}
	visible.Routes[0].Policy = &worker.RoutePolicy{PathPrefix: "/api", SecurityHeaders: true}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/routes", map[string]any{"routes": visible.Routes}, 200)
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var saved workloadInput
	if err = json.Unmarshal(data, &saved); err != nil || saved.Routes[0].Auth != auth {
		t.Fatal("route edit dropped hidden authentication", saved.Routes, err)
	}
	visible.Name = "Protected renamed"
	visible.ProjectID, visible.EnvironmentID = "", ""
	f.call("root", "default", "PUT", "/api/workloads/"+id, visible, 200)
	if err = f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	data, err = unseal(f.s.Key, encrypted)
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Routes[0].Auth != auth {
		t.Fatal("configuration save dropped hidden authentication", err)
	}
	route := map[string]any{"domain": "protected.example.com", "service": "app", "port": 80, "policy": visible.Routes[0].Policy, "auth": ""}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/routes", map[string]any{"routes": []any{route}}, 200)
	if err = f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	data, err = unseal(f.s.Key, encrypted)
	saved = workloadInput{}
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Routes[0].Auth != "" {
		t.Fatal("explicit authentication removal was not applied", err)
	}
}

func TestRouteAuthenticationRejectsAmbiguousPathChange(t *testing.T) {
	previous := []worker.Route{{Domain: "app.example.com", Auth: "first", Policy: &worker.RoutePolicy{PathPrefix: "/one"}}, {Domain: "app.example.com", Auth: "second", Policy: &worker.RoutePolicy{PathPrefix: "/two"}}}
	input := []routeInput{{Route: worker.Route{Domain: "app.example.com", Policy: &worker.RoutePolicy{PathPrefix: "/three"}}}}
	if _, err := retainedRouteAuth(input, previous); err == nil {
		t.Fatal("ambiguous credentials discarded")
	}
	input[0].Policy.PathPrefix = "/two"
	got, err := retainedRouteAuth(input, previous)
	if err != nil || got[0].Auth != "second" {
		t.Fatal(got, err)
	}
}

func TestRoutePolicyPreviewRedirectSafety(t *testing.T) {
	s := &Server{}
	src := &previewSource{ID: "app", Name: "App", Kind: "service", Config: workloadInput{Routes: []worker.Route{{Domain: "app.example.com", Service: "web", Port: 80, Policy: &worker.RoutePolicy{RedirectHost: "www.example.com"}}}}}
	recipe := previewRecipe{DomainTemplate: "{resource}-{preview}.preview.example.com"}
	plan := s.planPreviewResource(recipe, src, &previewTopology{}, map[string]bool{}, map[int64]bool{})
	if !slices.ContainsFunc(plan.Blockers, func(b previewBlocker) bool { return b.Code == "route_redirect" }) {
		t.Fatal("preview would redirect to production", plan)
	}
	recipe.DomainTemplate = ""
	plan = s.planPreviewResource(recipe, src, &previewTopology{}, map[string]bool{}, map[int64]bool{})
	if slices.ContainsFunc(plan.Blockers, func(b previewBlocker) bool { return b.Code == "route_redirect" }) {
		t.Fatal("unpublished redirect blocked a preview", plan)
	}
}
