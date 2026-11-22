package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"slices"
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
