package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type providerTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (p providerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = p.target.Scheme, p.target.Host
	copy.URL = &u
	return p.base.RoundTrip(copy)
}

func TestCloudProvidersCreateEnrollAndResumeWithoutDuplicate(t *testing.T) {
	for _, kind := range []string{"hetzner", "digitalocean", "vultr"} {
		t.Run(kind, func(t *testing.T) {
			f := newAccountFixture(t)
			var mu sync.Mutex
			posts := 0
			var instance map[string]any
			var enrollment string
			token := "test-provider-credential-never-returned"
			fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("provider authentication missing")
					w.WriteHeader(401)
					return
				}
				path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1"), "/v2")
				var result any
				switch path {
				case "/locations":
					result = map[string]any{"locations": []any{map[string]any{"id": 1, "name": "region"}}}
				case "/server_types":
					result = map[string]any{"server_types": []any{map[string]any{"id": 2, "name": "plan", "cores": 2, "memory": 4, "architecture": "x86"}}}
				case "/regions":
					result = map[string]any{"regions": []any{map[string]any{"id": "region", "slug": "region", "name": "Region", "available": true}}, "meta": map[string]any{}}
				case "/sizes":
					result = map[string]any{"sizes": []any{map[string]any{"slug": "plan", "vcpus": 2, "memory": 4096, "price_monthly": 6, "regions": []string{"region"}, "available": true}}}
				case "/plans":
					result = map[string]any{"plans": []any{map[string]any{"id": "plan", "vcpu_count": 2, "ram": 4096, "monthly_cost": 6, "locations": []string{"region"}}}, "meta": map[string]any{}}
				case "/images":
					result = map[string]any{"images": []any{map[string]any{"id": 3, "name": "Debian 13", "distribution": "Debian"}}}
				case "/snapshots":
					result = map[string]any{"snapshots": []any{map[string]any{"id": "prepared-fixture", "os_id": 3, "description": "Trusted verifier", "status": "complete"}}, "meta": map[string]any{}}
				case "/os":
					result = map[string]any{"os": []any{map[string]any{"id": 3, "name": "Debian 13"}}, "meta": map[string]any{}}
				default:
					collection, singular := "servers", "server"
					if kind == "digitalocean" {
						collection, singular = "droplets", "droplet"
					} else if kind == "vultr" {
						collection, singular = "instances", "instance"
					}
					if r.Method == "POST" {
						posts++
						if path != "/"+collection {
							t.Error("wrong creation path")
						}
						var body map[string]any
						if json.NewDecoder(io.LimitReader(r.Body, 65537)).Decode(&body) != nil {
							t.Error("invalid creation body")
						}
						data, _ := body["user_data"].(string)
						if kind == "vultr" {
							decoded, err := base64.StdEncoding.DecodeString(data)
							if err != nil {
								t.Error(err)
							}
							data = string(decoded)
						}
						if !strings.HasPrefix(data, "#cloud-config\n") || strings.Contains(data, token) || strings.Contains(data, "PRIVATE KEY") || strings.Contains(data, "sudo sh") {
							t.Error("unsafe cloud-init configuration")
						}
						var cloud struct {
							Run [][]string `json:"runcmd"`
						}
						if json.Unmarshal([]byte(strings.TrimPrefix(data, "#cloud-config\n")), &cloud) != nil || len(cloud.Run) != 1 {
							t.Error("missing enrollment command")
						}
						command := cloud.Run[0][2]
						at := strings.Index(command, "Authorization: Bearer ")
						if at < 0 {
							t.Error("missing enrollment token")
						} else {
							enrollment = strings.Split(command[at+len("Authorization: Bearer "):], "'")[0]
						}
						instance = map[string]any{"id": 123, "name": body["name"], "label": body["label"], "status": "running", "default_password": "provider-default-secret", "tags": body["tags"], "labels": body["labels"], "main_ip": "198.51.100.20", "public_net": map[string]any{"ipv4": map[string]string{"ip": "198.51.100.20"}}, "networks": map[string]any{"v4": []any{map[string]string{"ip_address": "198.51.100.20", "type": "public"}}}}
						// Simulate a paid creation with a lost acknowledgement. Recovery must use GET only.
						w.WriteHeader(503)
						return
					}
					if r.Method != "GET" {
						t.Errorf("unexpected provider mutation %s", r.Method)
					}
					if strings.HasSuffix(path, "/123") {
						result = map[string]any{singular: instance}
					} else {
						items := []any{}
						if instance != nil {
							items = append(items, instance)
						}
						result = map[string]any{collection: items, "meta": map[string]any{}}
					}
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer fake.Close()
			target, _ := url.Parse(fake.URL)
			f.s.ProviderTransport = providerTestTransport{target, fake.Client().Transport}
			user := f.user("cloud-editor", "cloud@example.com")
			f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','cloud-editor','owner')`)
			f.call(user, "default", "GET", "/api/node-providers", nil, 403)
			created := f.call("root", "default", "POST", "/api/node-providers", map[string]any{"kind": kind, "name": "Cloud", "token": token}, 200)
			provider := jsonString(t, created, "id")
			for _, path := range []string{"/api/node-providers", "/api/node-providers/" + provider + "/catalog"} {
				response := f.call("root", "default", "GET", path, nil, 200)
				if strings.Contains(response.Body.String(), token) || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("provider token leaked or response cached")
				}
			}
			region, plan := "region", "plan"
			if kind == "hetzner" {
				region, plan = "1", "2"
			}
			input := map[string]any{"provider_id": provider, "name": "New node", "region": region, "plan": plan, "image": "3", "confirm": false}
			f.call("root", "default", "POST", "/api/node-provisions", input, 400)
			input["confirm"] = true
			f.call("root", "default", "POST", "/api/node-provisions", input, 400)
			input["confirm_verifier"] = true
			if kind == "vultr" {
				input["image"] = "snapshot-prepared-fixture"
			}
			jobID := jsonString(t, f.call("root", "default", "POST", "/api/node-provisions", input, 202), "id")
			for i := 0; i < 3; i++ {
				if err := f.s.processNodeProvision(context.Background(), jobID); err != nil {
					t.Fatal(err)
				}
			}
			if posts != 1 {
				t.Fatalf("creation repeated after lost acknowledgement: %d", posts)
			}
			var node, phase, address string
			var encrypted []byte
			if err := f.s.DB.QueryRow(context.Background(), `SELECT p.node_id,p.phase,p.configuration,n.address FROM node_provisions p JOIN nodes n ON n.id=p.node_id WHERE p.id=$1`, jobID).Scan(&node, &phase, &encrypted, &address); err != nil {
				t.Fatal(err)
			}
			if phase != "enrolling" || address != "198.51.100.20" || enrollment == "" {
				t.Fatalf("enrollment did not progress: %s %s", phase, address)
			}
			if strings.Contains(string(encrypted), enrollment) {
				t.Fatal("enrollment persisted in plaintext")
			}
			f.call("root", "default", "DELETE", "/api/node-providers/"+provider, nil, 409)
			host, _, err := keyPair()
			if err != nil {
				t.Fatal(err)
			}
			f.s.workerRPC = func(_ context.Context, got string, req worker.RPCRequest) (worker.RPCResponse, error) {
				if got != node || req.Method != "probe" {
					t.Fatal("enrollment verified wrong node")
				}
				return worker.RPCResponse{Probe: &worker.Probe{Protocol: 1, Ready: true}}, nil
			}
			body, _ := json.Marshal(map[string]string{"host_key": host})
			registration := httptest.NewRequest("POST", "/bootstrap/register", bytes.NewReader(body))
			registration.Header.Set("Authorization", "Bearer "+enrollment)
			registered := httptest.NewRecorder()
			f.handler.ServeHTTP(registered, registration)
			if registered.Code != 200 {
				t.Fatalf("automatic enrollment failed: %d %s", registered.Code, registered.Body.String())
			}
			replay := httptest.NewRecorder()
			registration = httptest.NewRequest("POST", "/bootstrap/register", bytes.NewReader(body))
			registration.Header.Set("Authorization", "Bearer "+enrollment)
			f.handler.ServeHTTP(replay, registration)
			if replay.Code != 401 {
				t.Fatal("enrollment token reusable")
			}

			if err := f.s.processNodeProvision(context.Background(), jobID); err != nil {
				t.Fatal(err)
			}
			if err := f.s.DB.QueryRow(context.Background(), `SELECT phase,configuration FROM node_provisions WHERE id=$1`, jobID).Scan(&phase, &encrypted); err != nil {
				t.Fatal(err)
			}
			if phase != "succeeded" || len(encrypted) != 0 {
				t.Fatal("terminal enrollment secret not erased")
			}
			for _, path := range []string{"/api/node-provisions", "/api/nodes/" + node + "/cloud"} {
				response := f.call("root", "default", "GET", path, nil, 200)
				for _, secret := range []string{token, enrollment, "provider-default-secret"} {
					if strings.Contains(response.Body.String(), secret) {
						t.Fatal("secret exposed in cloud status")
					}
				}
			}
			f.call("root", "default", "DELETE", "/api/node-providers/"+provider, nil, 200)
			if posts != 1 {
				t.Fatal("disconnect created a cloud server")
			}
		})
	}
}

func TestProviderReconciliationRefusesForeignServersAndAbandonRevokesEnrollment(t *testing.T) {
	f := newAccountFixture(t)
	secret, _ := seal(f.s.Key, []byte("test-provider-token"))
	payload, _ := seal(f.s.Key, []byte(`{"name":"Pending","token":"enrollment-must-not-survive"}`))
	f.sql(`INSERT INTO node_providers(id,kind,name,secret) VALUES('provider','hetzner','Provider',$1)`, secret)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('pending','Pending','pending.invalid',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,'pending',now()+interval '15 minutes')`, tokenHash("enrollment-must-not-survive"))
	f.sql(`INSERT INTO node_provisions(id,provider_id,provider_kind,provider_name,team_id,node_id,cloud_name,phase,configuration) VALUES('creation','provider','hetzner','Provider','default','pending','cove-creation','uncertain',$1)`, payload)
	owned := false
	mutations := 0
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
			w.WriteHeader(500)
			return
		}
		marker := "foreign"
		if owned {
			marker = "creation"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"id": 123, "name": "cove-creation", "labels": map[string]string{"cove-provision": marker}, "public_net": map[string]any{"ipv4": map[string]string{"ip": "198.51.100.21"}}}})
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	f.s.ProviderTransport = providerTestTransport{target, fake.Client().Transport}
	f.call("root", "default", "POST", "/api/node-provisions/creation/reconcile", map[string]any{"confirm": true, "server_id": "123"}, 409)
	owned = true
	f.call("root", "default", "POST", "/api/node-provisions/creation/reconcile", map[string]any{"confirm": false, "server_id": "123"}, 400)
	f.call("root", "default", "POST", "/api/node-provisions/creation/reconcile", map[string]any{"confirm": true, "server_id": "123"}, 202)
	if err := f.s.processNodeProvision(context.Background(), "creation"); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "DELETE", "/api/node-provisions/creation", map[string]bool{"confirm": true}, 200)
	var cleared, revoked bool
	if err := f.s.DB.QueryRow(context.Background(), `SELECT phase='cancelled' AND octet_length(configuration)=0 FROM node_provisions WHERE id='creation'`).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT consumed_at IS NOT NULL FROM enrollments WHERE node_id='pending'`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !cleared || !revoked || mutations != 0 {
		t.Fatal("abandonment changed a cloud server or retained an enrollment secret")
	}
}

func TestProviderCreationClaimSurvivesConcurrentDispatchAndRestart(t *testing.T) {
	f := newAccountFixture(t)
	secret, _ := seal(f.s.Key, []byte("test-provider-token"))
	payload, _ := seal(f.s.Key, []byte(`{"name":"Node","region":"1","plan":"2","image":"3","confirm_verifier":true}`))
	f.sql(`INSERT INTO node_providers(id,kind,name,secret) VALUES('provider','hetzner','Provider',$1)`, secret)
	f.sql(`INSERT INTO node_provisions(id,provider_id,provider_kind,provider_name,team_id,cloud_name,configuration,creator_id) VALUES('creation','provider','hetzner','Provider','default','cove-creation',$1,'bootstrap')`, payload)
	var mu sync.Mutex
	posts := 0
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": []any{}})
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	f.s.ProviderTransport = providerTestTransport{target, fake.Client().Transport}
	var group sync.WaitGroup
	failures := make(chan error, 3)
	for i := 0; i < 3; i++ {
		group.Go(func() { failures <- f.s.processNodeProvision(context.Background(), "creation") })
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.processNodeProvision(context.Background(), "creation"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("concurrent/restarted dispatch sent %d paid creations", posts)
	}
	var nodes int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM nodes WHERE cloud_server_name='cove-creation'`).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if nodes != 1 {
		t.Fatal("creation allocated duplicate pending nodes")
	}
	f.sql(`UPDATE node_provisions SET deadline=now()-interval '1 second' WHERE id='creation'`)
	if err := f.s.processNodeProvision(context.Background(), "creation"); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT phase FROM node_provisions WHERE id='creation'`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "uncertain" || posts != 1 {
		t.Fatal("unconfirmed paid server was retried or presented as a definite failure")
	}
}

func TestProviderHTTPRefusesRedirectAndBoundsResponse(t *testing.T) {
	for _, test := range []string{"redirect", "oversize", "private-address"} {
		t.Run(test, func(t *testing.T) {
			calls := 0
			fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if test == "redirect" {
					http.Redirect(w, r, "https://outside.example/steal", 302)
					return
				}
				_, _ = io.WriteString(w, strings.Repeat(" ", (2<<20)+1))
			}))
			defer fake.Close()
			target, _ := url.Parse(fake.URL)
			s := Server{ProviderTransport: providerTestTransport{target, fake.Client().Transport}}
			provider := nodeProvider{Kind: "hetzner", Token: "synthetic-token"}
			if test == "private-address" {
				data, _ := json.Marshal(map[string]any{"id": 1, "name": "server", "public_net": map[string]any{"ipv4": map[string]string{"ip": "127.0.0.1"}}})
				var row map[string]json.RawMessage
				_ = json.Unmarshal(data, &row)
				if _, err := decodeProviderInstance(provider, row, "marker"); err == nil {
					t.Fatal("private cloud address accepted")
				}
				return
			}
			if _, err := s.providerJSON(context.Background(), provider, "GET", "/servers", nil); err == nil {
				t.Fatal("invalid cloud response accepted")
			}
			if calls != 1 {
				t.Fatal("provider redirected credentials or repeated a request")
			}
		})
	}
}
