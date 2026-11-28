package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCloudflareTunnelProviderDoesNotRepeatAnUncertainCreate(t *testing.T) {
	account := strings.Repeat("a", 32)
	intent := cloudflareTunnelIdentity{Name: "cove-" + strings.Repeat("b", 32), Secret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}
	posts := 0
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.cloudflare.com" || r.URL.Path != "/client/v4/accounts/"+account+"/cfd_tunnel" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture", 6) {
			t.Fatal("unexpected provider boundary")
		}
		posts++
		return nil, errors.New("synthetic lost response")
	})}
	if _, err := s.createCloudflareTunnel(context.Background(), strings.Repeat("fixture", 6), account, intent); !errors.Is(err, errTunnelOutcomeUnknown) {
		t.Fatal("lost outcome not retained", err)
	}
	intent.Attempted = true
	if _, err := s.createCloudflareTunnel(context.Background(), strings.Repeat("fixture", 6), account, intent); !errors.Is(err, errTunnelOutcomeUnknown) || posts != 1 {
		t.Fatal("uncertain create repeated", err, posts)
	}
	if err := s.removeCloudflareTunnel(context.Background(), strings.Repeat("fixture", 6), account, intent); !errors.Is(err, errTunnelOutcomeUnknown) {
		t.Fatal("unowned tunnel was eligible for deletion")
	}
}

func TestCloudflareTunnelLostResponsesRequireAuthenticatedOwnershipAndExactDNSCleanup(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	zoneID, account, connection := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	token := strings.Repeat("fixture-only-", 3)
	credential, err := f.s.sealJSON(cloudflareCredentials{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,'bootstrap','Fixture','token',$2)`, connection, credential)
	zone := cloudflareZone{ZoneID: zoneID, Domain: "example.test", ConnectionID: connection, Enabled: true}
	raw, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zoneID, raw)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['*.example.test'])`, zoneID)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('tunnel-node','Node','localhost',22,'',$1,'ready','{"cloudflare_publishing":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('tunnel-project','Tunnel','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Tunnel", NodeID: "tunnel-node", ProjectID: "tunnel-project", Kind: "service", Compose: "services: {}", Routes: []worker.Route{{Domain: "app.example.test", Service: "app", Port: 8080}, {Domain: "other.example.test", Service: "app", Port: 8080}}}, 201), "id")
	_ = resource
	remote := cloudflareTunnelRemote{ID: "11111111-2222-3333-4444-555555555555", Account: account, ConfigSource: "local"}
	var record *tunnelDNSProof
	creates, dnsCreates, deletes := 0, 0, 0
	conflict := true
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.cloudflare.com" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("real provider/credential boundary crossed")
		}
		var result any
		switch {
		case r.Method == "GET" && r.URL.Path == "/client/v4/zones/"+zoneID:
			result = map[string]any{"id": zoneID, "name": "example.test", "account": map[string]string{"id": account}}
		case r.URL.Path == "/client/v4/accounts/"+account+"/cfd_tunnel" && r.Method == "POST":
			creates++
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["config_src"] != "local" {
				t.Fatal("invalid tunnel create")
			}
			remote.Name = body["name"]
			return nil, errors.New("fixture committed before losing create response")
		case r.URL.Path == "/client/v4/accounts/"+account+"/cfd_tunnel" && r.Method == "GET":
			result = []cloudflareTunnelRemote{remote}
		case r.URL.Path == "/client/v4/accounts/"+account+"/cfd_tunnel/"+remote.ID && r.Method == "GET":
			result = remote
		case r.URL.Path == "/client/v4/accounts/"+account+"/cfd_tunnel/"+remote.ID && r.Method == "DELETE":
			deletes++
			result = map[string]any{}
		case r.URL.Path == "/client/v4/zones/"+zoneID+"/dns_records" && r.Method == "GET":
			list := []map[string]any{}
			if conflict {
				list = append(list, map[string]any{"id": strings.Repeat("e", 32), "type": "A", "name": r.URL.Query().Get("name"), "content": "192.0.2.1"})
			}
			if record != nil && record.Name == r.URL.Query().Get("name") {
				list = append(list, map[string]any{"id": record.ID, "type": "CNAME", "name": record.Name, "content": record.Content, "comment": record.Comment, "proxied": true})
			}
			result = list
		case r.URL.Path == "/client/v4/zones/"+zoneID+"/dns_records" && r.Method == "POST":
			dnsCreates++
			var body struct {
				tunnelDNSProof
				Type    string `json:"type"`
				Proxied bool   `json:"proxied"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != "CNAME" || !body.Proxied {
				t.Fatal("invalid tunnel DNS")
			}
			body.ID = strings.Repeat("d", 32)
			record = &body.tunnelDNSProof
			return nil, errors.New("fixture committed before losing DNS create response")
		case record != nil && r.URL.Path == "/client/v4/zones/"+zoneID+"/dns_records/"+record.ID && r.Method == "GET":
			result = map[string]any{"id": record.ID, "type": "CNAME", "name": record.Name, "content": record.Content, "comment": record.Comment, "proxied": true}
		case record != nil && r.URL.Path == "/client/v4/zones/"+zoneID+"/dns_records/"+record.ID && r.Method == "DELETE":
			record = nil
			deletes++
			result = map[string]any{}
		default:
			t.Fatalf("unexpected fake provider request %s %s", r.Method, r.URL.Path)
		}
		body, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	authenticated := false
	wrongWorkerIdentity := false
	proveCalls := 0
	disabled := false
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "tunnel-node" {
			t.Fatal("wrong worker")
		}
		status := &worker.CloudflarePublishingStatus{Phase: "active", Connected: authenticated, Running: !disabled, Withheld: map[string]string{}}
		if r.Method == "cloudflare_publishing_apply" {
			if r.Publishing == nil || r.Publishing.TunnelSecret == "" {
				t.Fatal("missing sealed credential")
			}
			status.ID, status.TunnelID = r.Publishing.ID, r.Publishing.TunnelID
			if wrongWorkerIdentity {
				status.ID = strings.Repeat("f", 32)
			}
			if r.Publishing.ProveOnly {
				proveCalls++
			}
		}
		if r.Method == "cloudflare_publishing_disable" {
			disabled = true
			status.Phase = "disabled"
			status.Running = false
			status.Connected = false
		}
		return worker.RPCResponse{Publishing: status}, nil
	}
	path := "/api/nodes/tunnel-node/cloudflare-tunnel"
	f.call("root", "default", "PUT", path, nodeTunnelConfiguration{ZoneID: zoneID, Hostnames: []string{"app.example.test"}}, 202)
	step := func() {
		t.Helper()
		if err := f.s.processCloudflareTunnel(ctx, "tunnel-node", false); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		step()
	}
	if creates != 1 || dnsCreates != 0 || proveCalls == 0 {
		t.Fatal("unproven create was duplicated or published", creates, dnsCreates, proveCalls)
	}
	authenticated = true
	wrongWorkerIdentity = true
	step()
	if dnsCreates != 0 {
		t.Fatal("mismatched worker identity was adopted")
	}
	wrongWorkerIdentity = false
	for range 6 {
		step()
	}
	if dnsCreates != 0 {
		t.Fatal("preflight conflict sent a create")
	}
	var cipher []byte
	if err := f.s.DB.QueryRow(ctx, "SELECT state FROM node_cloudflare_tunnels WHERE node_id='tunnel-node'").Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	decoded, err := unseal(f.s.Key, cipher)
	var pending nodeTunnelPrivate
	if err != nil || json.Unmarshal(decoded, &pending) != nil || pending.Records["app.example.test"].Attempted {
		t.Fatal("confirmed preflight refusal was retained as an uncertain mutation", err)
	}
	conflict = false
	f.call("root", "default", "POST", path+"/retry", nil, 202)
	for range 5 {
		step()
	}
	if creates != 1 || dnsCreates != 1 || record == nil {
		t.Fatal("ownership did not recover exactly once", creates, dnsCreates)
	}
	response := f.call("root", "default", "GET", path, nil, 200)
	for _, marker := range []string{token, "tunnel_secret", "\"secret\"", record.Comment} {
		if strings.Contains(response.Body.String(), marker) {
			t.Fatal("provider secret/proof exposed")
		}
	}
	f.sql(`UPDATE nodes SET capabilities='{"live_routes":true,"route_protocol":6,"route_policies":true}' WHERE id='tunnel-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": []worker.Route{{Domain: "app.example.test", Service: "app", Port: 8081}}}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"cloudflare_publishing":true}' WHERE id='tunnel-node'`)
	f.call("root", "default", "DELETE", "/api/nodes/tunnel-node", nil, 409)
	conflict = true
	f.call("root", "default", "PUT", path, nodeTunnelConfiguration{ZoneID: zoneID, Hostnames: []string{"app.example.test", "other.example.test"}}, 202)
	for range 5 {
		step()
	}
	if dnsCreates != 1 {
		t.Fatal("second preflight conflict mutated DNS")
	}
	f.sql(`DELETE FROM cloudflare_zone_teams WHERE zone_id=$1`, zoneID)
	for range 3 {
		step()
	}
	if !disabled || record != nil || dnsCreates != 1 {
		t.Fatal("revoked zone share kept publishing or created another record")
	}
	f.call("root", "default", "DELETE", path, nil, 202)
	for range 6 {
		step()
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM node_cloudflare_tunnels WHERE node_id='tunnel-node'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || !disabled || record != nil || deletes != 2 {
		t.Fatal("owned cleanup incomplete", count, disabled, deletes)
	}
}
