package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cove/internal/outbound"
	"cove/internal/worker"
)

func TestOperationsMergeSecretPermissionsAndReadAPI(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:read", "logs:read")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','limited','Limited',ARRAY['view','edit_configuration'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"limited"}]' WHERE user_id='public-owner'`)
	base := "/api/workloads/" + id
	f.call(cookie, "default", "GET", base+"/backups/snapshot/download", nil, 403)
	f.call(cookie, "default", "POST", base+"/backups/import-s3", map[string]string{}, 403)
	f.call(cookie, "default", "PUT", base+"/backup-policy", map[string]any{"backup": worker.BackupConfig{Repository: "local", Password: "synthetic-secret", KeepLast: 2}}, 403)
	f.call(cookie, "default", "GET", base+"/files/targets", nil, 403)
	for _, permissions := range []string{"'view','read_secrets'", "'view','manage_backups'"} {
		f.sql(`UPDATE team_roles SET permissions=ARRAY[` + permissions + `] WHERE id='limited'`)
		f.call(cookie, "default", "GET", base+"/backups/snapshot/download", nil, 403)
	}
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view','read_secrets','manage_backups'] WHERE id='limited'`)
	f.call(cookie, "default", "GET", base+"/backups/snapshot/download", nil, 409)
	f.call(cookie, "default", "POST", base+"/backups/import-s3", map[string]string{}, 403)
	for _, path := range []string{"/metrics", "/log-history", "/uptime", "/sizing"} {
		publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id+path, nil, 200)
	}
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view'] WHERE id='limited'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id+"/backups/snapshot/download", nil, 404)
}

func TestOperationsMergeAdministratorTokenCaps(t *testing.T) {
	f := newAccountFixture(t)
	for _, path := range []string{"/api/settings/metrics-token", "/api/settings/panel-backup/run", "/api/nodes/fixture/cleanup-run", "/api/notifications"} {
		r := httptest.NewRequest("POST", path, nil)
		r.SetPathValue("id", "fixture")
		ctx := context.WithValue(r.Context(), identityKey{}, &identity{ID: "bootstrap", Admin: true, Team: "default"})
		ctx = context.WithValue(ctx, apiCredentialKey{}, apiCredential{Scopes: []string{"workloads:read"}})
		w := httptest.NewRecorder()
		if f.s.authorize(w, r.WithContext(ctx)) || w.Code != 403 {
			t.Fatalf("read-only administrator token authorized %s: %d", path, w.Code)
		}
	}
}

func TestOperationsMergeCanonicalAuditRetention(t *testing.T) {
	f := newAccountFixture(t)
	f.call("root", "default", "PUT", "/api/settings/audit-export", auditExportConfig{Webhook: "https://audit.invalid/events", Secret: strings.Repeat("x", 32), Retention: 30}, 200)
	f.sql(`INSERT INTO audit_events(actor,action,subject,status,created_at) VALUES('fixture','fixture','recent',200,now()-interval '2 days'),('fixture','fixture','old',200,now()-interval '40 days'),('fixture','fixture','pending',200,now()-interval '40 days')`)
	f.sql(`DELETE FROM audit_deliveries WHERE event_id IN (SELECT id FROM audit_events WHERE subject='old')`)
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	c, err := telemetryConfig(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuditDays != 30 {
		t.Fatal("retention did not use the canonical policy", c.AuditDays)
	}
	c.AuditDays = 1
	if err = pruneTelemetry(t.Context(), tx, c); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var recent, old, pending bool
	err = f.s.DB.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE subject='recent'),EXISTS(SELECT 1 FROM audit_events WHERE subject='old'),EXISTS(SELECT 1 FROM audit_events WHERE subject='pending')`).Scan(&recent, &old, &pending)
	if err != nil || !recent || old || !pending {
		t.Fatal("retention lost protected audit records", recent, old, pending, err)
	}
}

func TestOperationsMergeOutcomeRoutingAndSharedSigning(t *testing.T) {
	f, _, _ := publicFixture(t)
	secret := strings.Repeat("s", 32)
	config := connectionConfig{Name: "Outcome channel", Notification: &notificationConfig{Provider: "webhook", URL: "https://notifications.invalid/events", SigningSecret: secret, Enabled: true, Events: channelOutcomeEvents}}
	f.call("root", "default", "POST", "/api/notifications", config, 201)
	f.sql(`INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('fixture','emergency deployment override','operation',200,'default'),('deploy hook','accepted delivery','hook',202,'default'),('fixture','POST /api/projects/{id}/imports/coolify','project',201,'default'),('fixture','resource.adopt','resource',200,'default')`)
	f.sql(`INSERT INTO authentication_failures(key,locked_until) VALUES('account:public-owner',now()+interval '1 minute')`)
	seen := map[string]bool{}
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(r.Header.Get("X-Cove-Timestamp") + "."))
		mac.Write(body)
		if r.Header.Get("X-Cove-Timestamp") == "" || r.Header.Get("X-Cove-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("inconsistent webhook signing")
		}
		for _, event := range channelOutcomeEvents {
			if strings.Contains(string(body), event) {
				seen[event] = true
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	for range 3 {
		if err := f.s.dispatchHealthNotifications(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []string{"security.lockout", "deployment.freeze_override", "deploy_hook.accepted", "import.reviewed", "adoption.succeeded"} {
		if !seen[event] {
			t.Fatal("event not routed", event, seen)
		}
	}
	if notificationMatches(&notificationConfig{Enabled: true, Failure: true}, "security.lockout") {
		t.Fatal("new events require explicit channel opt-in")
	}
	if err := f.s.deliverAudit(t.Context(), auditExportConfig{Webhook: "https://audit.invalid", Secret: secret}, "webhook", 42, []byte(`{"safe":true}`)); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsMergeProxyForChecksLogsAndStreams(t *testing.T) {
	targets := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets <- r.Host
		http.Error(w, "fixture refuses before contacting destination", 502)
	}))
	defer proxy.Close()
	s := &Server{outboundState: &outboundProxyState{}}
	c := outbound.Config{HTTPS: proxy.URL}
	s.outboundState.value.Store(&c)
	s.probeUptime(t.Context(), uptimeCheck{Domain: "8.8.8.8", Path: "/", Timeout: 1, ExpectedMin: 200, ExpectedMax: 299})
	if err := s.sendLogBatch(t.Context(), logForwarder{Name: "Fixture", Kind: "http", Endpoint: "https://8.8.8.8/logs", Enabled: true}, []forwardedLog{{Timestamp: time.Now(), Message: "redacted"}}); err == nil {
		t.Fatal("refused proxy succeeded")
	}
	if err := s.sendLogBatch(t.Context(), logForwarder{Name: "Syslog", Kind: "syslog", Endpoint: "tls://8.8.8.8:6514", Enabled: true}, []forwardedLog{{Timestamp: time.Now(), Message: "redacted"}}); err == nil {
		t.Fatal("refused syslog proxy succeeded")
	}
	if err := s.deliverAudit(t.Context(), auditExportConfig{Syslog: "8.8.8.8:6514", TLS: true}, "syslog", 1, []byte(`{}`)); err == nil {
		t.Fatal("refused audit proxy succeeded")
	}
	for range 4 {
		select {
		case address := <-targets:
			if address != "8.8.8.8:443" && address != "8.8.8.8:6514" {
				t.Fatal(address)
			}
		case <-time.After(time.Second):
			t.Fatal("configured proxy was bypassed")
		}
	}
	s.outboundState.saved.Store(true)
	s.workerStream = func(_ context.Context, _ string, r worker.RPCRequest, _ io.Reader, _ func(worker.StreamResponse, io.Reader) error) error {
		if r.Proxy == nil || *r.Proxy != c || r.Stream.Request.OutboundProxy == nil || *r.Stream.Request.OutboundProxy != c {
			t.Fatal("stream did not carry selected proxy")
		}
		return nil
	}
	if err := s.nodeStream(t.Context(), "fixture", worker.RPCRequest{Method: "stream", Stream: &worker.StreamRequest{Request: &worker.Request{}}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsMergeSMTPUsesSelectedProxy(t *testing.T) {
	f := newAccountFixture(t)
	target := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target <- r.Host
		http.Error(w, "fixture refuses before contacting destination", 502)
	}))
	defer proxy.Close()
	f.s.SMTP = SMTPConfig{Host: "8.8.8.8", Port: "465", From: "cove@example.com"}
	config := outbound.Config{HTTPS: proxy.URL}
	f.s.outboundState.value.Store(&config)
	mailer, err := f.s.mailer(t.Context())
	if err != nil || mailer == nil {
		t.Fatal("mailer unavailable", err)
	}
	if err = mailer(t.Context(), "fixture@example.com", "Fixture", "Synthetic content"); err == nil {
		t.Fatal("refused SMTP proxy succeeded")
	}
	select {
	case address := <-target:
		if address != "8.8.8.8:465" {
			t.Fatal(address)
		}
	default:
		t.Fatal("SMTP bypassed the selected proxy")
	}
}

func TestOperationsMergeMetricsCapabilityRevalidation(t *testing.T) {
	f := newAccountFixture(t)
	created := f.call("root", "default", "POST", "/api/settings/metrics-token", nil, 200)
	token := jsonString(t, created, "token")
	publicCall(t, f, token, "", "GET", "/metrics", nil, 200)
	f.sql(`UPDATE control_settings SET require_second_factor=true`)
	publicCall(t, f, token, "", "GET", "/metrics", nil, 403)
	f.sql(`UPDATE control_settings SET require_second_factor=false`)
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	publicCall(t, f, token, "", "GET", "/metrics", nil, 403)
	f.sql(`UPDATE teams SET require_second_factor=false`)
	f.sql(`UPDATE users SET disabled=true WHERE id=(SELECT metrics_token_creator_id FROM control_settings)`)
	publicCall(t, f, token, "", "GET", "/metrics", nil, 401)
}

func TestOperationsMergePublicStatusPrefixAndIsolation(t *testing.T) {
	f, id, _ := publicFixture(t)
	slug := strings.Repeat("e", 64)
	f.sql(`INSERT INTO project_status_pages(project_id,enabled,title,slug) VALUES('variable-project',true,'Public availability',$1)`, slug)
	f.sql(`INSERT INTO route_checks(id,workload_id,team_id,domain,path,enabled,interval_seconds,timeout_seconds,expected_min,expected_max,public_label) VALUES('private-check',$1,'default','private-host.invalid','/private-path',true,60,2,200,299,'Public API')`, id)
	f.sql(`UPDATE control_settings SET panel_allowlist=ARRAY['127.0.0.1/32']`)
	f.s.PublicURL = "https://cove.example/control/cove"
	f.handler = f.s.Handler()
	request := func(address string, status int) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/control/cove/api/status/"+slug, nil)
		r.RemoteAddr = address
		r.Header.Set("Authorization", "Bearer expired-private-token")
		r.Header.Set("X-Cove-Team", "other-team")
		r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: "expired-session"})
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
		return w
	}
	request("192.0.2.2:1234", 403)
	body := request("127.0.0.1:1234", 200).Body.String()
	if !strings.Contains(body, "Public API") {
		t.Fatal(body)
	}
	for _, private := range []string{id, "private-check", "private-host.invalid", "private-path", "variable-project"} {
		if strings.Contains(body, private) {
			t.Fatal("public status exposed internal data", private, body)
		}
	}
	f.sql(`UPDATE project_status_pages SET enabled=false`)
	request("127.0.0.1:1234", 404)
}

func TestOperationsMergeRetentionKeepsDraftAndGovernanceReferences(t *testing.T) {
	f, id, _ := publicFixture(t)
	sealed, err := f.s.sealJSON(worker.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"draft-op", "old-op", "governance-op"} {
		action := "restart"
		age := "400 days"
		if op == "governance-op" {
			action = "deploy"
			age = "100 days"
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) VALUES($1,$2,$3,'failed',$4,now()-$5::interval)`, op, id, action, sealed, age)
	}
	f.sql(`INSERT INTO configuration_drafts(id,project_id,team_id,creator_id,payload,base_version,changes,operation_ids) VALUES('saved-draft','variable-project','default','public-owner',$1,'version','[]','["draft-op"]')`, sealed)
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "prune_journals" || r.JournalPrune == nil {
			t.Fatal("unexpected RPC", r.Method)
		}
		pruned := []string{}
		for _, candidate := range r.JournalPrune.Candidates {
			if candidate.ID == "draft-op" || candidate.ID == "governance-op" {
				t.Fatal("protected operation offered to pruning", candidate.ID)
			}
			pruned = append(pruned, candidate.ID)
		}
		return worker.RPCResponse{JournalPrune: &worker.JournalPruneResult{Pruned: pruned}}, nil
	}
	if err = f.s.pruneNodeOperationHistory(t.Context(), "variable-node", 1, 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE id IN ('draft-op','governance-op')`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if err = f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE id='old-op'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestOperationsMergeMaintenanceExemptionsAndConfigSecrecy(t *testing.T) {
	f, id, _ := publicFixture(t)
	f.sql(`INSERT INTO deployment_policies(team_id,freeze_from,freeze_until) VALUES('default',now()-interval '1 hour',now()+interval '1 hour')`)
	for _, action := range []string{"backup", "restore", "drill", "stop", "restart"} {
		op := "merge-" + action
		sealed, err := f.s.sealJSON(worker.Request{ID: op, Workload: id, Action: action})
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,$3,'queued',$4)`, op, id, action, sealed)
		admitted, err := f.s.admitDeployment(t.Context(), op)
		if err != nil || !admitted {
			t.Fatal("maintenance was frozen", action, err)
		}
		f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, op)
	}
	secret := "operational-backup-secret"
	manifest := projectConfiguration{Resources: []configurationResource{{Configuration: workloadInput{Backup: &worker.BackupConfig{Password: secret, Remote: &worker.StorageLocation{Password: secret, SecretKey: secret}}}}}}
	for _, include := range []bool{false, true} {
		raw, _ := json.Marshal(publicProjectConfiguration(manifest, include))
		if strings.Contains(string(raw), secret) {
			t.Fatal("configuration export exposed operational credentials")
		}
	}
	raw, _ := json.Marshal(compareConfiguration(worker.Request{Backup: &worker.BackupConfig{Password: secret}}, worker.Request{}))
	if strings.Contains(string(raw), secret) {
		t.Fatal("release comparison exposed credentials")
	}
}

func TestOperationsMergeAuditFailureDoesNotRecursivelyExport(t *testing.T) {
	f, _, _ := publicFixture(t)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Audit failure", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.invalid/events", Enabled: true, Events: []string{"audit_export.failed"}}}, 201)
	f.call("root", "default", "PUT", "/api/settings/audit-export", auditExportConfig{Webhook: "https://audit.invalid/events", Secret: strings.Repeat("x", 32), Retention: 30}, 200)
	var before, after int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader("fixture failure"))}, nil
	})
	if err := f.s.dispatchAuditExport(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&after); err != nil || before != after {
		t.Fatal("audit failure recursively exported itself", before, after, err)
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE event='audit_export.failed'`).Scan(&after); err != nil || after != 1 {
		t.Fatal("failure did not enter the channel queue", after, err)
	}
}
