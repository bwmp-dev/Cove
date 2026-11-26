package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type dnsTransport func(*http.Request) (*http.Response, error)

func (f dnsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCloudflareReconciliation(t *testing.T) {
	for _, test := range []struct {
		name    string
		records []cloudflareRecord
		method  string
		fail    bool
	}{
		{name: "create", method: "POST"},
		{name: "correct wrong worker", records: []cloudflareRecord{{ID: strings.Repeat("b", 32), Name: "app.example.com", Type: "A", Content: "8.8.4.4"}}, method: "PATCH"},
		{name: "unchanged", records: []cloudflareRecord{{Name: "app.example.com", Type: "A", Content: "8.8.8.8"}}},
		{name: "cname conflict", records: []cloudflareRecord{{Name: "app.example.com", Type: "CNAME"}}, fail: true},
		{name: "other family", records: []cloudflareRecord{{Name: "app.example.com", Type: "AAAA"}}, fail: true},
		{name: "multiple addresses", records: []cloudflareRecord{{Name: "app.example.com", Type: "A"}, {Name: "app.example.com", Type: "A"}}, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutations := 0
			s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.cloudflare.com" || r.Header.Get("Authorization") != "Bearer private-token" {
					t.Fatal("wrong API destination or auth")
				}
				result := any(test.records)
				if r.Method != "GET" {
					mutations++
					if r.Method != test.method {
						t.Fatalf("unexpected mutation %s", r.Method)
					}
					var body cloudflareRecord
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Content != "8.8.8.8" || body.Name != "app.example.com" {
						t.Fatal("incorrect record")
					}
					result = map[string]any{}
				}
				b, _ := json.Marshal(map[string]any{"success": true, "result": result})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
			})}
			err := s.reconcileDNSRecord(context.Background(), cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.com", Token: "private-token"}, "app.example.com", "8.8.8.8")
			if (err != nil) != test.fail {
				t.Fatalf("error: %v", err)
			}
			want := 0
			if test.method != "" {
				want = 1
			}
			if mutations != want {
				t.Fatalf("mutations=%d", mutations)
			}
		})
	}
}

func TestCloudflareRejectsOutsideZoneAndRedactsProviderErrors(t *testing.T) {
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("secret-provider-body"))}, nil
	})}
	c := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.com", Token: "secret"}
	if s.reconcileDNSRecord(context.Background(), c, "evil-example.com", "8.8.8.8") == nil {
		t.Fatal("accepted outside zone")
	}
	err := s.reconcileDNSRecord(context.Background(), c, "app.example.com", "8.8.8.8")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestCloudflarePreservesExistingProxySettings(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "8.8.4.4"} {
		writes := 0
		s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
			var result any = []cloudflareRecord{{ID: strings.Repeat("b", 32), Name: "app.example.com", Type: "A", Content: address, Proxied: true}}
			if r.Method != "GET" {
				writes++
				var record cloudflareRecord
				if json.NewDecoder(r.Body).Decode(&record) != nil || !record.Proxied || record.Content != "8.8.8.8" {
					t.Fatal("DNS repair changed proxy state")
				}
				result = map[string]any{}
			}
			raw, _ := json.Marshal(map[string]any{"success": true, "result": result})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw))}, nil
		})}
		err := s.reconcileDNSRecord(context.Background(), cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.com", Token: "private-token", PreserveProxy: true}, "app.example.com", "8.8.8.8")
		if err != nil || address == "8.8.8.8" && writes != 0 || address != "8.8.8.8" && writes != 1 {
			t.Fatal("unexpected reconciliation", err, writes)
		}
	}
}

func TestCloudflareTeamReconciliationAndCredentialMasking(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("editor", "editor@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','A'),('b','B')")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','editor','owner','[]')")
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','N','127.0.0.1',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','P','a'),('other','Other','b')")
	config := []byte(`{"name":"App","node_id":"n","routes":[{"domain":"app.example.com","service":"app","port":80}]}`)
	encrypted, err := seal(f.s.Key, config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) VALUES('w','n','App',$1,'a','p'),('foreign','n','Foreign',$1,'b','other')", encrypted)
	f.sql("INSERT INTO operations(id,workload_id,action,state,payload) VALUES('done','w','deploy','succeeded',$1),('foreign-done','foreign','deploy','succeeded',$1)", []byte{})
	writes := 0
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		result := any(map[string]string{"name": "example.com"})
		if strings.Contains(r.URL.Path, "dns_records") {
			result = []cloudflareRecord{}
		}
		if r.Method != "GET" {
			writes++
			result = map[string]any{}
		}
		data, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	zone := cloudflareZone{ZoneID: strings.Repeat("c", 32), Domain: "example.com", Token: strings.Repeat("secret", 8), Enabled: true, Nodes: map[string]string{"n": "8.8.8.8"}}
	f.call(editor, "a", "PUT", "/api/cloudflare", zone, 400)
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('a','n')")
	f.call(editor, "a", "PUT", "/api/cloudflare", zone, 200)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"team","role":"editor"}]' WHERE user_id='editor'`)
	f.call(editor, "a", "PUT", "/api/cloudflare", zone, 200)
	f.sql(`UPDATE team_members SET grants='[{"scope":"project","scope_id":"p","role":"editor"}]' WHERE user_id='editor'`)
	f.call(editor, "a", "PUT", "/api/cloudflare", zone, 200)
	f.sql(`UPDATE team_members SET role='owner',grants='[]' WHERE user_id='editor'`)
	f.call(editor, "a", "PUT", "/api/cloudflare", zone, 200)
	f.call(editor, "a", "PUT", "/api/cloudflare/"+zone.ZoneID+"/shares", map[string]any{"shares": []cloudflareShare{{Team: "a", Domains: []string{"app.example.com"}}}}, 200)
	response := f.call("root", "a", "GET", "/api/cloudflare", nil, 200)
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatal("token leaked")
	}
	if err = f.s.reconcileDNS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("wrong team reconciliation: %d", writes)
	}
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('active','w','routes',$1)", []byte{})
	f.sql("UPDATE cloudflare_zones SET checked_at=NULL")
	if err = f.s.reconcileDNS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatal("background reconciliation updated a changing route")
	}
	request := worker.Request{ID: "active", Workload: "w", Action: "routes", Routes: []worker.Route{{Domain: "app.example.com", Service: "app", Port: 80}}}
	if ready, err := f.s.prepareRouteDNS(context.Background(), "n", request, time.Now()); err != nil || !ready {
		t.Fatal("admitted route change did not publish DNS before submission", ready, err)
	}
	if writes != 2 {
		t.Fatal("active route publication was not performed", writes)
	}
	f.sql("UPDATE operations SET state='cancelled' WHERE id='active'")
	zone.Token = ""
	zone.Enabled = false
	f.call("root", "a", "PUT", "/api/cloudflare", zone, 200)
	if err = f.s.reconcileDNS(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatal("updated disabled zone")
	}
	zone.Token = strings.Repeat("other", 8)
	other := f.user("other-owner", "other@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('b','other-owner','owner','[]')`)
	f.call(other, "b", "PUT", "/api/cloudflare", zone, 403)
}
