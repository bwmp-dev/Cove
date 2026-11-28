package control

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cove/internal/outbound"
	"cove/internal/worker"
)

func TestTunnelRevokedOwnerStopsWorkerWithoutProviderAccess(t *testing.T) {
	for _, revoke := range []string{
		"proxy policy changed at restart",
		`UPDATE users SET disabled=true WHERE id='bootstrap'`,
		`UPDATE users SET is_admin=false WHERE id='bootstrap'`,
		`UPDATE control_settings SET require_second_factor=true WHERE id=1`,
	} {
		t.Run(revoke, func(t *testing.T) {
			f := newAccountFixture(t)
			ctx := context.Background()
			zoneID, connection := strings.Repeat("a", 32), strings.Repeat("b", 32)
			credentials, err := f.s.sealJSON(cloudflareCredentials{Token: strings.Repeat("fixture-only-", 3)})
			if err != nil {
				t.Fatal(err)
			}
			zone, err := f.s.sealJSON(cloudflareZone{ZoneID: zoneID, Domain: "example.test", ConnectionID: connection, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.s.sealJSON(nodeTunnelPrivate{Identity: cloudflareTunnelIdentity{Name: "cove-" + strings.Repeat("c", 32), Secret: base64.StdEncoding.EncodeToString(make([]byte, 32))}})
			if err != nil {
				t.Fatal(err)
			}
			f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,'bootstrap','Fixture','token',$2)`, connection, credentials)
			f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zoneID, zone)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('tunnel-node','Node','localhost',22,'',$1)`, []byte{})
			f.sql(`INSERT INTO node_cloudflare_tunnels(node_id,id,zone_id,connection_id,owner_id,hostnames,image,state) VALUES('tunnel-node',$1,$2,$3,'bootstrap',ARRAY['app.example.test'],$4,$5)`, strings.Repeat("c", 32), zoneID, connection, worker.DefaultCloudflaredImage, state)
			if revoke == "proxy policy changed at restart" {
				proxy := outbound.Config{HTTPS: "http://127.0.0.1:9"}
				f.s.outboundState.value.Store(&proxy)
			} else {
				f.sql(revoke)
			}
			stops := 0
			f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
				if r.Method != "cloudflare_publishing_disable" {
					t.Fatalf("revoked owner used worker %s", r.Method)
				}
				stops++
				return worker.RPCResponse{Publishing: &worker.CloudflarePublishingStatus{Phase: "disabled"}}, nil
			}
			f.s.CloudflareTransport = dnsTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("revoked owner reached provider")
				return nil, nil
			})
			if err := f.s.processCloudflareTunnel(ctx, "tunnel-node", false); err != nil {
				t.Fatal(err)
			}
			var phase string
			if err := f.s.DB.QueryRow(ctx, `SELECT phase FROM node_cloudflare_tunnels WHERE node_id='tunnel-node'`).Scan(&phase); err != nil {
				t.Fatal(err)
			}
			if stops != 1 || phase != "blocked" {
				t.Fatalf("stops=%d phase=%s", stops, phase)
			}
		})
	}
}

func TestTunnelReservationsBlockOnlyMovesOfPublishedHosts(t *testing.T) {
	f, _ := moveFixture(t, "queued")
	zoneID, connection := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,'bootstrap','Fixture','token',$2)`, connection, []byte{})
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zoneID, []byte{})
	state, err := f.s.sealJSON(nodeTunnelPrivate{Records: map[string]*tunnelDNSState{"retiring.example.test": {Cleanup: true}}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO node_cloudflare_tunnels(node_id,id,zone_id,connection_id,owner_id,hostnames,image,state) VALUES('source',$1,$2,$3,'bootstrap',ARRAY['app.example.test'],$4,$5)`, strings.Repeat("c", 32), zoneID, connection, worker.DefaultCloudflaredImage, state)
	for _, test := range []struct {
		host    string
		blocked bool
	}{{"app.example.test", true}, {"retiring.example.test", true}, {"other.example.test", false}} {
		err := f.s.checkMoveTunnelReservations(t.Context(), f.s.DB, "source", "target", []worker.Route{{Domain: test.host}})
		if (err != nil) != test.blocked {
			t.Fatalf("host %s blocked=%v err=%v", test.host, test.blocked, err)
		}
	}
}

func TestTunnelProviderUsesConfiguredOutboundProxy(t *testing.T) {
	hits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != "CONNECT" || r.Host != "api.cloudflare.com:443" {
			t.Errorf("unexpected proxy target %s %s", r.Method, r.Host)
		}
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	s := &Server{outboundState: &outboundProxyState{}}
	config := outbound.Config{HTTPS: proxy.URL}
	s.outboundState.value.Store(&config)
	s.CloudflareTransport = s.OutboundTransport(&http.Transport{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := s.cloudflareTunnelRequest(ctx, strings.Repeat("fixture", 6), strings.Repeat("a", 32), "GET", "/cfd_tunnel", nil, nil)
	if err == nil || hits != 1 {
		t.Fatalf("tunnel request bypassed selected proxy: hits=%d err=%v", hits, err)
	}
}

func TestTunnelScopedAdministrationRechecksCreatorPolicy(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.call(cookie, "default", "GET", "/api/nodes/variable-node/cloudflare-tunnel", nil, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	_, token := mintPublic(t, f, cookie, "user", "nodes:manage")
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Publishing: &worker.CloudflarePublishingStatus{Phase: "disabled"}}, nil
	}
	path := "/api/v1/nodes/variable-node/cloudflare-tunnel"
	publicCall(t, f, token, "default", "GET", path, nil, 200)
	f.sql(`UPDATE users SET is_admin=false WHERE id='public-owner'`)
	publicCall(t, f, token, "default", "GET", path, nil, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.sql(`UPDATE control_settings SET require_second_factor=true WHERE id=1`)
	publicCall(t, f, token, "default", "GET", path, nil, 403)
}

func TestTunnelFailureNotificationsContainNoProviderOrHostnameData(t *testing.T) {
	f := newAccountFixture(t)
	zone, connection := strings.Repeat("a", 32), strings.Repeat("b", 32)
	f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,'bootstrap','Fixture','token',$2)`, connection, []byte{})
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zone, []byte{})
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('tunnel-node','Node','localhost',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO node_cloudflare_tunnels(node_id,id,zone_id,connection_id,owner_id,hostnames,image,state) VALUES('tunnel-node',$1,$2,$3,'bootstrap',ARRAY['sensitive.example.test'],$4,$5)`, strings.Repeat("c", 32), zone, connection, worker.DefaultCloudflaredImage, []byte{})
	f.user("ordinary", "ordinary@example.test")
	f.sql(`INSERT INTO teams(id,name) VALUES('ordinary-team','Ordinary')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('ordinary-team','ordinary','owner')`)
	for _, team := range []string{"default", "ordinary-team"} {
		f.call("root", team, "POST", "/api/notifications", connectionConfig{Name: "Tunnel notifications", Notification: &notificationConfig{Provider: "email", Email: "alerts@example.test", Enabled: true, Events: []string{"tunnel.failed"}}}, 201)
	}
	f.sql(`UPDATE node_cloudflare_tunnels SET phase='blocked',last_error='sensitive.example.test fixture-secret' WHERE node_id='tunnel-node'`)
	var team, message string
	rows, err := f.s.DB.Query(t.Context(), `SELECT team_id,message FROM health_deliveries WHERE event='tunnel.failed'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err := rows.Scan(&team, &message); err != nil {
			t.Fatal(err)
		}
		count++
		if team != "default" || strings.Contains(message, "sensitive") || strings.Contains(message, "fixture-secret") {
			t.Fatalf("unsafe tunnel notification for %s", team)
		}
	}
	if rows.Err() != nil || count != 1 {
		t.Fatalf("administrator-team notifications=%d err=%v", count, rows.Err())
	}
}
