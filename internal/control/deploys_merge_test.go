package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func mergedProtectedRoutes() []worker.Route {
	return []worker.Route{{Domain: "app.example.test", Service: "web", Port: 8080, Auth: "user:$2y$10$" + strings.Repeat("a", 53), Policy: &worker.RoutePolicy{
		PathPrefix: "/api", SecurityHeaders: true, IPAllowList: []string{"192.0.2.0/24"}, Sticky: true,
		RequestHeaders: map[string]string{"X-App": "api"}, ResponseHeaders: map[string]string{"X-Version": "v1"},
		CertificateSource: "default", Upstreams: []worker.RouteUpstream{{Service: "other", Port: 8081}},
		Maintenance: &worker.RouteMaintenance{Enabled: true, HTML: "<h1>Unavailable</h1>"}, ErrorPages: map[string]string{"503": "<h1>Retry</h1>"},
		ForwardAuth: &worker.RouteForwardAuth{URL: "https://auth.example.test/check?token=protected-url-marker", ResponseHeaders: []string{"Remote-User"}},
	}}, {Domain: "service.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{ForwardAuth: &worker.RouteForwardAuth{Service: "auth", Port: 9000, Path: "/check?token=protected-path-marker"}}}}
}

func TestDeploysMergeRouteHistoryAndPortableRoundTrip(t *testing.T) {
	routes := mergedProtectedRoutes()
	old := workloadInput{Name: "App", NodeID: "node", Kind: "service", Compose: "services: {}", Routes: routes}
	encode := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encode(old), &fields); err != nil {
		t.Fatal(err)
	}
	for _, includeSecrets := range []bool{false, true} {
		manifest := projectConfiguration{APIVersion: "cove/v1", Project: "Project", Resources: []configurationResource{{Key: "app", Configuration: old}}}
		public := publicProjectConfiguration(manifest, includeSecrets)
		data := encode(public)
		for _, marker := range []string{routes[0].Auth, "protected-url-marker", "protected-path-marker"} {
			if strings.Contains(string(data), marker) {
				t.Fatal("portable configuration disclosed route protection", includeSecrets)
			}
		}
		if err := validateProjectConfiguration(public); err != nil {
			t.Fatal("redacted manifest no longer parses", err)
		}
		var reloaded projectConfiguration
		if err := json.Unmarshal(data, &reloaded); err != nil {
			t.Fatal(err)
		}
		merged, err := mergeManifestResource(reloaded.Resources[0].Configuration, old)
		if err != nil || !worker.RoutesEqual(merged.Routes, routes) {
			t.Fatal("portable configuration lost route policies or saved protection", merged.Routes, err)
		}
	}
	changes := safeConfigurationDiffs(nil, fields)
	data := encode(changes)
	if strings.Contains(string(data), routes[0].Auth) || strings.Contains(string(data), "protected-url-marker") || strings.Contains(string(data), "protected-path-marker") {
		t.Fatal("history diff disclosed route protection")
	}
	stored := []configurationChange{{Resource: "app", Diffs: []configurationDiff{{Field: "routes", Before: string(encode(routes)), After: string(encode(routes))}}}}
	redactConfigurationChanges(stored)
	if strings.Contains(string(encode(stored)), "protected-") || strings.Contains(string(encode(stored)), "$2y$") {
		t.Fatal("previously saved draft still discloses route protection")
	}
	if routes[0].Policy.ForwardAuth.URL == "" || routes[1].Policy.ForwardAuth.Path == "" {
		t.Fatal("redaction mutated the sealed source configuration")
	}
}

func TestDeploysMergeOlderRouteWritersKeepPolicies(t *testing.T) {
	previous := mergedProtectedRoutes()[:1]
	for _, policy := range []any{nil, map[string]any{"path_prefix": "/api"}} {
		value := map[string]any{"domain": previous[0].Domain, "service": "web", "port": 8080}
		if policy != nil {
			value["policy"] = policy
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var input routeInput
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		actual, err := retainedRouteAuth([]routeInput{input}, previous)
		if err != nil || !worker.RoutesEqual(actual, previous) {
			t.Fatal("older writer dropped protection", actual, err)
		}
	}
	var removal routeInput
	if err := json.Unmarshal([]byte(`{"domain":"app.example.test","service":"web","port":8080,"policy":{"path_prefix":"/api","forward_auth":null}}`), &removal); err != nil {
		t.Fatal(err)
	}
	actual, err := retainedRouteAuth([]routeInput{removal}, previous)
	if err != nil || actual[0].Policy.ForwardAuth != nil || !reflect.DeepEqual(actual[0].Policy.IPAllowList, previous[0].Policy.IPAllowList) || actual[0].Auth != previous[0].Auth {
		t.Fatal("explicit auth removal lost other protection", actual, err)
	}
}

func TestDeploysMergeTokenPoliciesAndBuildPermissions(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "projects:read", "workloads:read", "workloads:write", "workloads:operate", "secrets:read")
	base := "/api/v1/workloads/" + id
	for _, suffix := range []string{"/autoscale-policy", "/idle-policy", "/restart-policy"} {
		publicCall(t, f, token, "default", "GET", base+suffix, nil, 200)
	}
	publicCall(t, f, token, "default", "GET", "/api/v1/environments/variable-env/suspension", nil, 200)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"build_publish":true}' WHERE id='variable-node'`)
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','build-limited','Build limited',ARRAY['view','operate'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"build-limited"}]' WHERE user_id='public-owner'`)
	input := apiOperationInput{Action: "build", Build: &worker.BuildOptions{Repository: "team/app"}, Reason: "Audited build"}
	for _, permissions := range []string{"'view','operate'", "'view','edit_configuration'", "'view','read_secrets'"} {
		f.sql(`UPDATE team_roles SET permissions=ARRAY[` + permissions + `] WHERE id='build-limited'`)
		publicCall(t, f, token, "default", "POST", base+"/operations", input, 403)
	}
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view','edit_configuration','read_secrets'] WHERE id='build-limited'`)
	publicCall(t, f, token, "default", "POST", base+"/operations", input, 409)
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	publicCall(t, f, token, "default", "GET", base+"/autoscale-policy", nil, 403)
	f.sql(`UPDATE teams SET require_second_factor=false WHERE id='default'`)
	f.sql(`UPDATE control_settings SET panel_allowlist=ARRAY['127.0.0.1/32']`)
	publicCall(t, f, token, "default", "GET", base+"/restart-policy", nil, 403)
}

func TestDeploysMergeAdmissionAndRecoveryExemptions(t *testing.T) {
	f, id, _ := publicFixture(t)
	f.sql(`INSERT INTO deployment_policies(team_id,freeze_from,freeze_until) VALUES('default',now()-interval '1 minute',now()+interval '1 hour')`)
	for _, action := range []string{"deploy", "rollout_promote", "rollout_abort", "resume", "restart_unhealthy", "build"} {
		op := resourceID()
		f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, op, id, action, []byte{})
		admitted, err := f.s.admitDeployment(t.Context(), op)
		expected := action != "deploy" && action != "rollout_promote"
		if err != nil || admitted != expected {
			t.Fatal(action, admitted, err)
		}
		f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, op)
	}
	f.sql(`UPDATE environments SET paused=true WHERE id='variable-env'`)
	for _, action := range []string{"restore", "drill", "rollout_promote"} {
		f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": action}, 409)
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"restore_copies":true}' WHERE id='variable-node'`)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/restore-copy", map[string]string{"name": "Recovery", "backup_id": "backup"}, 409)
}

func TestDeploysMergeIntentionalDownDefersChecks(t *testing.T) {
	f, id, _ := publicFixture(t)
	f.sql(`INSERT INTO route_checks(id,workload_id,team_id,domain,path,enabled,interval_seconds,timeout_seconds,expected_min,expected_max,public_label) VALUES('held-check',$1,'default','route.invalid','/',true,60,1,200,299,'Service')`, id)
	f.s.NotificationTransport = notificationRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("intentional down state triggered an uptime probe")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "container_health" {
			t.Fatalf("unexpected RPC %s", r.Method)
		}
		return worker.RPCResponse{Suspension: &worker.SuspensionStatus{Phase: "asleep"}}, nil
	}
	for _, state := range []string{"stopped", "paused", "asleep"} {
		f.sql(`UPDATE workloads SET manually_stopped=$2,idle_policy=CASE WHEN $3 THEN '{"enabled":true}'::jsonb ELSE '{"enabled":false}'::jsonb END WHERE id=$1`, id, state == "stopped", state == "asleep")
		f.sql(`UPDATE environments SET paused=$1 WHERE id='variable-env'`, state == "paused")
		f.sql(`UPDATE route_checks SET next_check_at=now()-interval '1 minute',last_up=false,consecutive_failures=9 WHERE id='held-check'`)
		down, err := f.s.resourceIntentionallyDown(t.Context(), id)
		if err != nil || !down {
			t.Fatal(state, down, err)
		}
		if err = f.s.collectUptime(t.Context()); err != nil {
			t.Fatal(err)
		}
		var unknown bool
		var count int
		if err = f.s.DB.QueryRow(t.Context(), `SELECT last_up IS NULL,consecutive_failures FROM route_checks WHERE id='held-check'`).Scan(&unknown, &count); err != nil || !unknown || count != 0 {
			t.Fatal(state, unknown, count, err)
		}
	}
}

func TestDeploysMergeOutcomeNotificationsAreDeduplicated(t *testing.T) {
	f, id, _ := publicFixture(t)
	config := connectionConfig{Name: "Lifecycle", Notification: &notificationConfig{Provider: "webhook", URL: "https://notifications.invalid/events", SigningSecret: strings.Repeat("s", 32), Enabled: true, Events: channelOutcomeEvents}}
	f.call("root", "default", "POST", "/api/notifications", config, 201)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('merged-rollout',$1,'deploy',$2)`, id, []byte{})
	for range 2 {
		f.sql(`UPDATE operations SET state='succeeded',result='{"rollout":{"phase":"waiting"}}' WHERE id='merged-rollout'`)
	}
	f.sql(`INSERT INTO audit_events(actor,action,subject,status,details) VALUES('health observer','idle state observed',$1,200,'{"state":"asleep"}')`, id)
	seen := map[string]int{}
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		for _, event := range []string{"rollout.staged", "resource.asleep"} {
			if strings.Contains(string(body), event) {
				seen[event]++
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	for range 2 {
		if err := f.s.dispatchHealthNotifications(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []string{"rollout.staged", "resource.asleep"} {
		if seen[event] != 1 {
			t.Fatal("lifecycle event not delivered once", event, seen)
		}
	}
}

func TestDeploysMergePublicRoutePoliciesAreDocumented(t *testing.T) {
	s := &Server{}
	doc := s.openAPIDocument()
	paths := doc["paths"].(map[string]any)
	for _, path := range []string{"/workloads/{id}/autoscale-policy", "/workloads/{id}/idle-policy", "/workloads/{id}/restart-policy", "/environments/{id}/suspension"} {
		methods, ok := paths[path].(map[string]any)
		if !ok || methods["get"] == nil || methods["put"] == nil {
			t.Fatal("missing real policy contract", path)
		}
	}
}

func TestDeploysMergeStoredHistoryDraftAndExportProtectRoutes(t *testing.T) {
	f, id := variablesFixture(t)
	config := workloadInput{Name: "Variables", NodeID: "variable-node", Kind: "service", Compose: "services:\n  web:\n    image: busybox:1.37\n", Routes: mergedProtectedRoutes()}
	protected, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, protected)
	assertSafe := func(body []byte) {
		t.Helper()
		for _, marker := range []string{config.Routes[0].Auth, "protected-url-marker", "protected-path-marker"} {
			if bytes.Contains(body, []byte(marker)) {
				t.Fatal("route protection disclosed", marker)
			}
		}
	}
	assertSafe(f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200).Body.Bytes())
	assertSafe(f.call("root", "default", "GET", "/api/workloads/"+id+"/configuration-history", nil, 200).Body.Bytes())
	base := "/api/projects/variable-project/configuration"
	response := f.call("root", "default", "GET", base, nil, 200)
	assertSafe(response.Body.Bytes())
	var current struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	current.Manifest.Project = "Reviewed project"
	raw, err := json.Marshal(current.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	draft := f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 201)
	assertSafe(draft.Body.Bytes())
	assertSafe(f.call("root", "default", "GET", base+"/drafts", nil, 200).Body.Bytes())
	f.call("root", "default", "POST", base+"/drafts/"+jsonString(t, draft, "id")+"/apply", nil, 200)
	var sealed []byte
	if err = f.s.DB.QueryRow(t.Context(), `SELECT configuration FROM workloads WHERE id=$1`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	var retained workloadInput
	if err = f.s.unsealJSON(sealed, &retained); err != nil || !worker.RoutesEqual(retained.Routes, config.Routes) {
		t.Fatal("draft dropped saved protection", err)
	}
	exported := f.call("root", "default", "POST", base+"/export", map[string]string{}, 200)
	archive, err := zip.NewReader(bytes.NewReader(exported.Body.Bytes()), int64(exported.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		assertSafe(body)
	}
	var details string
	if err = f.s.DB.QueryRow(t.Context(), `SELECT COALESCE(string_agg(details::text,','),'') FROM audit_events`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	assertSafe([]byte(details))
}

func TestDeploysMergeOnlyCommittedGenerationsBecomeBaselines(t *testing.T) {
	f, id := variablesFixture(t)
	for i, entry := range []struct{ id, action, phase, release string }{{"original", "deploy", "completed", "original"}, {"candidate", "deploy", "waiting", "candidate"}, {"abort", "rollout_abort", "aborted", "candidate"}, {"promote", "rollout_promote", "completed", "candidate"}} {
		request := worker.Request{ID: entry.id, Workload: id, Action: entry.action, Compose: "services: {}", Kind: "service"}
		payload, err := f.s.sealJSON(request)
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,result,updated_at) VALUES($1,$2,$3,$4,'succeeded',jsonb_build_object('rollout',jsonb_build_object('phase',$5::text,'release',$6::text)),now()+make_interval(secs=>$7))`, entry.id, id, entry.action, payload, entry.phase, entry.release, i)
		var actual string
		if err = f.s.DB.QueryRow(t.Context(), `SELECT declaration_id FROM cove_committed_deployments WHERE workload_id=$1 ORDER BY updated_at DESC,operation_id DESC LIMIT 1`, id).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		expected := "original"
		if entry.action == "rollout_promote" {
			expected = "candidate"
		}
		if actual != expected {
			t.Fatal("staged or aborted generation replaced declaration", entry, actual)
		}
	}
	f.sql(`INSERT INTO deployment_policies(team_id,approval) VALUES('default',true)`)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('approval-promotion',$1,'rollout_promote',$2)`, id, []byte{})
	var required bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT approval_required FROM operations WHERE id='approval-promotion'`).Scan(&required); err != nil || !required {
		t.Fatal("promotion bypassed initial approval", required, err)
	}
}

func TestDeploysMergePausedAndAsleepSuppressContainerThresholds(t *testing.T) {
	f, id, _ := publicFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"alert_observations":true}' WHERE id='variable-node'`)
	channel := jsonString(t, f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Thresholds", Notification: &notificationConfig{Provider: "webhook", URL: "https://notifications.invalid/events", Enabled: true, Health: true}}, 201), "id")
	for _, metric := range []string{"restarts", "oom", "crashes"} {
		f.call("root", "default", "POST", "/api/notifications/rules", thresholdRule{Name: metric, WorkloadID: id, Metric: metric, Threshold: 1, Confirmation: 60, Repeat: 300, Channels: []string{channel}}, 201)
	}
	sample := &worker.WorkloadObservations{SampledAt: time.Now(), EventsAvailable: true, Containers: []worker.ContainerObservation{{Exited: true, OOMKilled: true}}, Events: []worker.ContainerEvent{{Action: "die", Timestamp: time.Now().Add(-time.Minute)}, {Action: "oom", Timestamp: time.Now().Add(-time.Minute)}, {Action: "start", AutomaticRestart: true, Timestamp: time.Now().Add(-time.Minute)}}}
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method == "observations" {
			return worker.RPCResponse{Observations: sample}, nil
		}
		if r.Method == "container_health" {
			return worker.RPCResponse{Suspension: &worker.SuspensionStatus{Phase: "asleep", Reason: "idle"}}, nil
		}
		t.Fatal(r.Method)
		return worker.RPCResponse{}, nil
	}
	for _, asleep := range []bool{false, true} {
		f.sql(`UPDATE environments SET paused=$1 WHERE id='variable-env'`, !asleep)
		f.sql(`UPDATE workloads SET idle_policy=jsonb_build_object('enabled',$2::boolean) WHERE id=$1`, id, asleep)
		if err := f.s.evaluateResourceThresholds(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		f.sql(`UPDATE health_alerts SET first_seen=now()-interval '2 minutes'`)
		if err := f.s.evaluateResourceThresholds(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE subject_id=$1 AND event='threshold.alert'`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("intentional suspension triggered a resource incident", count, err)
	}
}

func TestDeploysMergeProviderFeedbackWaitsForTrafficDecision(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,result) VALUES('feedback-candidate',$1,'deploy',$2,'succeeded','{"rollout":{"phase":"waiting","release":"feedback-candidate"}}')`, id, []byte{})
	assertState := func(want string) {
		t.Helper()
		var actual string
		if err := f.s.DB.QueryRow(t.Context(), `SELECT state FROM cove_deployment_feedback_states WHERE id='feedback-candidate'`).Scan(&actual); err != nil || actual != want {
			t.Fatal("provider feedback traffic state", actual, want, err)
		}
	}
	assertState("running")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,result) VALUES('feedback-promotion',$1,'rollout_promote',$2,'failed','{"rollout":{"phase":"completed","release":"feedback-candidate"}}')`, id, []byte{})
	assertState("running")
	f.sql(`UPDATE operations SET state='succeeded' WHERE id='feedback-promotion'`)
	assertState("succeeded")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,result,updated_at) VALUES('feedback-abort',$1,'rollout_abort',$2,'succeeded','{"rollout":{"phase":"aborted","release":"feedback-candidate"}}',now()+interval '1 second')`, id, []byte{})
	assertState("cancelled")
	f.sql(`UPDATE operations SET result='{}' WHERE id='feedback-candidate'`)
	assertState("succeeded")
}
