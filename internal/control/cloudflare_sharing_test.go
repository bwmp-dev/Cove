package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCloudflareDomainGrants(t *testing.T) {
	for _, test := range []struct {
		pattern, domain string
		allowed         bool
	}{
		{"app.example.com", "app.example.com", true}, {"app.example.com", "sub.app.example.com", false},
		{"*.stage.example.com", "stage.example.com", false}, {"*.stage.example.com", "app.stage.example.com", true},
		{"*.stage.example.com", "deep.app.stage.example.com", true}, {"*.stage.example.com", "evil-stage.example.com", false},
		{"*.example.com", "app.example.com.attacker.test", false}, {"*.example.com", "app..example.com", false},
		{"*.example.com", "-app.example.com", false}, {"*.example.com", "APP.example.com", false},
	} {
		if got := cloudflareDomainAllowed([]string{test.pattern}, test.domain); got != test.allowed {
			t.Errorf("%s / %s: %v", test.pattern, test.domain, got)
		}
	}
}

func TestCloudflarePersonalSharingIsolation(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("cf-owner", "owner@example.test")
	recipient := f.user("cf-recipient", "recipient@example.test")
	outsider := f.user("cf-outsider", "outsider@example.test")
	f.sql(`INSERT INTO teams(id,name) VALUES('second','Second'),('hidden','Hidden')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cf-owner','owner','[]'),('second','cf-owner','owner','[]'),('second','cf-recipient','owner','[]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','host',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO team_nodes VALUES('default','n'),('second','n')`)
	var writes []string
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		var result any = map[string]string{"name": "example.com"}
		if strings.Contains(r.URL.Path, "dns_records") {
			result = []cloudflareRecord{}
		}
		if r.Method != "GET" {
			var record cloudflareRecord
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
				t.Fatal(err)
			}
			writes = append(writes, record.Name)
			result = map[string]any{}
		}
		raw, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	zone := cloudflareZone{ZoneID: strings.Repeat("d", 32), Domain: "example.com", Token: strings.Repeat("private", 8), Enabled: true, Nodes: map[string]string{"n": "8.8.8.8"}}
	f.call(owner, "default", "PUT", "/api/cloudflare", zone, 200)
	if got := f.call(recipient, "second", "GET", "/api/cloudflare", nil, 200).Body.String(); strings.Contains(got, "example.com") {
		t.Fatal("private connection visible")
	}
	share := func(user string, shares []cloudflareShare, status int) {
		f.call(user, "", "PUT", "/api/cloudflare/"+zone.ZoneID+"/shares", map[string]any{"shares": shares}, status)
	}
	share(owner, []cloudflareShare{{Team: "hidden", Domains: []string{"example.com"}}}, 403)
	for _, pattern := range []string{"evil-example.com", "*.com", "*example.com", "example.com.", "APP.example.com", "a..example.com", "-app.example.com"} {
		share(owner, []cloudflareShare{{Team: "second", Domains: []string{pattern}}}, 400)
	}
	share(owner, []cloudflareShare{{Team: "default", Domains: []string{"app.example.com"}}, {Team: "second", Domains: []string{"*.stage.example.com"}}}, 200)
	share(recipient, []cloudflareShare{{Team: "second", Domains: []string{"*.example.com"}}}, 403)
	f.call(recipient, "second", "PUT", "/api/cloudflare", zone, 403)
	info := f.call(recipient, "second", "GET", "/api/cloudflare", nil, 200).Body.String()
	if strings.Contains(info, "private") || strings.Contains(info, "8.8.8.8") || strings.Contains(info, `"can_manage":true`) || strings.Contains(info, "app.example.com") {
		t.Fatal("credential or foreign share disclosed", info)
	}
	if got := f.call(outsider, "", "GET", "/api/cloudflare", nil, 200).Body.String(); strings.Contains(got, "example.com") {
		t.Fatal("connection disclosed to outsider")
	}
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('one','One','default'),('two','Two','second')`)
	for _, item := range []struct {
		id, team, project string
		domains           []string
	}{
		{"a", "default", "one", []string{"app.example.com", "unshared.example.com", "x.stage.example.com"}},
		{"b", "second", "two", []string{"app.example.com", "stage.example.com", "x.stage.example.com", "deep.x.stage.example.com", "example.com.attacker.test"}},
	} {
		routes := []map[string]any{}
		for _, domain := range item.domains {
			routes = append(routes, map[string]any{"domain": domain, "service": "app", "port": 80})
		}
		raw, _ := json.Marshal(map[string]any{"name": "App", "node_id": "n", "routes": routes})
		encrypted, err := seal(f.s.Key, raw)
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO workloads(id,name,node_id,team_id,project_id,configuration) VALUES($1,'App','n',$2,$3,$4)`, item.id, item.team, item.project, encrypted)
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)`, item.id, item.id, []byte{})
	}
	if err := f.s.reconcileDNS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 3 {
		t.Fatalf("wrong DNS writes: %v", writes)
	}
	for _, name := range writes {
		if name != "app.example.com" && name != "x.stage.example.com" && name != "deep.x.stage.example.com" {
			t.Fatal("out of scope write", name)
		}
	}
	var stale []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM cloudflare_zones WHERE zone_id=$1`, zone.ZoneID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	transport := f.s.CloudflareTransport
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "dns_records") {
			blockOnce.Do(func() { close(started); <-release })
		}
		return transport.RoundTrip(r)
	})
	dnsDone := make(chan error, 1)
	go func() { dnsDone <- f.s.reconcileResourceDNS(context.Background(), zone.ZoneID, "second", stale, "b") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS write did not start")
	}
	revoked := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("PUT", "/api/cloudflare/"+zone.ZoneID+"/shares", strings.NewReader(`{"shares":[]}`))
		req.Header.Set("Origin", f.s.PublicURL)
		req.AddCookie(&http.Cookie{Name: "__Host-cove", Value: owner})
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		revoked <- response.Code
	}()
	select {
	case <-revoked:
		t.Fatal("revocation raced an in-flight DNS write")
	case <-time.After(150 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-dnsDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DNS writer did not finish")
	}
	select {
	case status := <-revoked:
		if status != 200 {
			t.Fatal("revocation failed", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not finish")
	}
	writtenBeforeRevocation := len(writes)

	if err := f.s.reconcileResourceDNS(context.Background(), zone.ZoneID, "second", stale, "b"); err != nil {
		t.Fatal(err)
	}
	if len(writes) != writtenBeforeRevocation {
		t.Fatal("revoked grant still writes DNS")
	}
	// Ownership survives losing team membership; sharing still requires team rights.
	f.sql(`DELETE FROM team_members WHERE user_id='cf-owner'`)
	zone.Enabled = false
	zone.Token = ""
	f.call(owner, "", "PUT", "/api/cloudflare", zone, 200)
	share(owner, []cloudflareShare{{Team: "second", Domains: []string{"app.example.com"}}}, 403)
}

func TestCloudflareSharingMigration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO cloudflare_zones(zone_id,team_id,domain,configuration) VALUES('legacy','default','example.com',$1)`, []byte{})
	f.sql(`DELETE FROM schema_migrations WHERE id='cloudflare-user-sharing-v1'`)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var owner string
	var domains []string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT z.owner_id,t.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams t USING(zone_id) WHERE z.zone_id='legacy'`).Scan(&owner, &domains); err != nil || owner != "bootstrap" || len(domains) != 2 {
		t.Fatal("legacy access not preserved", err)
	}
	var legacyTeam *string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT team_id FROM cloudflare_zones WHERE zone_id='legacy'`).Scan(&legacyTeam); err != nil || legacyTeam != nil {
		t.Fatal("legacy team still owns the connection", err)
	}
	f.sql(`DELETE FROM cloudflare_zone_teams WHERE zone_id='legacy'`)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM cloudflare_zone_teams WHERE zone_id='legacy'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked shares recreated", err)
	}
}
