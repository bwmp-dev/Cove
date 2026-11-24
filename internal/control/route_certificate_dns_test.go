package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestCertificateTXTSeparatesRefusedCreatesFromUnknownOutcomes(t *testing.T) {
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6)}
	intent := certificateTXT{Name: "_acme-challenge.app.example.test", Content: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Comment: "cove-acme-" + resourceID()}
	for _, code := range []int{200, 302, 303, 400, 403, 408, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			posts := 0
			s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.cloudflare.com" || r.Header.Get("Authorization") != "Bearer "+zone.Token {
					t.Fatal("provider credentials escaped their fixed destination")
				}
				if r.Method == "GET" {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"success":true,"result":[]}`))}, nil
				}
				if r.Method != "POST" {
					t.Fatal("unexpected provider mutation")
				}
				posts++
				return &http.Response{StatusCode: code, Header: http.Header{"Location": {"https://unrelated.example.test/"}}, Body: io.NopCloser(strings.NewReader("sensitive-fixture " + zone.Token))}, nil
			})}
			_, err := s.presentCertificateTXT(context.Background(), zone, intent, false)
			unknown := code < 400 || code == 408 || code >= 500
			if err == nil || errors.Is(err, errCertificateTXTUncertain) != unknown || posts != 1 || strings.Contains(err.Error(), zone.Token) || strings.Contains(err.Error(), "sensitive-fixture") {
				t.Fatal("create refusal was confused with an unknown outcome, repeated or exposed", code)
			}
		})
	}
}

func TestCertificateDNSIntentBlockedRemovalQueuesConfirmedCleanup(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('dns-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('dns-project','Project','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "DNS", NodeID: "dns-node", ProjectID: "dns-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6), Enabled: true}
	protected, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, protected)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['app.example.test'])`, zone.ZoneID)
	base := "/api/workloads/" + resource
	issuer := jsonString(t, f.call("root", "default", "POST", base+"/certificate-issuers", map[string]any{"name": "DNS", "provider": "cloudflare_origin", "zone_id": zone.ZoneID, "domains": []string{"app.example.test"}, "validity_days": 7, "auto_renew": false}, 202), "id")
	intent := resourceID()
	protected, err = f.s.sealJSON(certificateDNSIntentState{Record: certificateTXT{Name: "_acme-challenge.app.example.test", Content: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Comment: "cove-acme-" + resourceID()}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO route_certificate_dns_intents(id,issuer_id,workload_id,zone_id,cycle_id,state,phase,next_check_at) VALUES($1,$2,$3,$4,'blocked',$5,'blocked',NULL)`, intent, issuer, resource, zone.ZoneID, protected)
	f.call("root", "default", "DELETE", base+"/certificate-issuers/"+issuer, nil, 200)
	var due bool
	if err := f.s.DB.QueryRow(ctx, `SELECT cleanup_requested AND phase='cleanup' AND next_check_at IS NOT NULL FROM route_certificate_dns_intents WHERE id=$1`, intent).Scan(&due); err != nil || !due {
		t.Fatal("removing a blocked issuer stranded its cleanup", due, err)
	}
	f.s.CloudflareTransport = dnsTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("a refused create must not require provider credentials to confirm no cleanup is needed")
		return nil, nil
	})
	if err := f.s.processCertificateDNSIntent(ctx, intent, false); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := f.s.DB.QueryRow(ctx, `SELECT phase FROM route_certificate_dns_intents WHERE id=$1`, intent).Scan(&phase); err != nil || phase != "cleaned" {
		t.Fatal("known refused create did not clean up", phase, err)
	}
}

func TestCertificateTXTReconcilesLostCreateAndDeletesOnlyOwnedRecord(t *testing.T) {
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6)}
	intent := certificateTXT{Name: "_acme-challenge.app.example.test", Content: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Comment: "cove-acme-" + strings.Repeat("b", 32)}
	foreign := map[string]any{"id": strings.Repeat("c", 32), "type": "TXT", "name": intent.Name, "content": "foreign", "comment": "unrelated record"}
	var stored *certificateTXT
	creates, deletes := 0, 0
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.cloudflare.com" || r.Header.Get("Authorization") != "Bearer "+zone.Token || !strings.HasPrefix(r.URL.Path, "/client/v4/zones/"+zone.ZoneID+"/dns_records") {
			t.Fatal("unexpected DNS destination or credentials")
		}
		var result any
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("type") != "TXT" || r.URL.Query().Get("name") != intent.Name {
				t.Fatal("DNS query is not scoped to the challenge")
			}
			rows := []any{foreign}
			if stored != nil {
				rows = append(rows, map[string]any{"id": stored.ID, "type": "TXT", "name": stored.Name, "content": stored.Content, "comment": stored.Comment})
			}
			result = rows
		case "POST":
			creates++
			var body struct {
				certificateTXT
				TTL  int    `json:"ttl"`
				Type string `json:"type"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.TTL != 60 || body.Type != "TXT" || body.certificateTXT != intent {
				t.Fatal("wrong challenge creation")
			}
			copy := intent
			copy.ID = strings.Repeat("d", 32)
			stored = &copy
			return nil, errors.New("connection lost after provider committed creation")
		case "DELETE":
			if stored == nil || r.URL.Path != "/client/v4/zones/"+zone.ZoneID+"/dns_records/"+stored.ID {
				t.Fatal("attempted to remove a foreign record")
			}
			deletes++
			stored = nil
			result = map[string]string{"id": strings.Repeat("d", 32)}
		default:
			t.Fatal(r.Method)
		}
		data, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
	})}
	if _, err := s.presentCertificateTXT(context.Background(), zone, intent, false); !errors.Is(err, errCertificateTXTUncertain) {
		t.Fatalf("lost response: %v", err)
	}
	owned, err := s.presentCertificateTXT(context.Background(), zone, intent, true)
	if err != nil || owned == nil || creates != 1 {
		t.Fatalf("reconciliation: %+v, %v, creates=%d", owned, err, creates)
	}
	if err := s.removeCertificateTXT(context.Background(), zone, *owned); err != nil {
		t.Fatal(err)
	}
	if err := s.removeCertificateTXT(context.Background(), zone, *owned); err != nil || deletes != 1 || foreign["content"] != "foreign" {
		t.Fatalf("idempotent owned cleanup: %v, deletes=%d", err, deletes)
	}
	if _, err := s.presentCertificateTXT(context.Background(), zone, intent, true); !errors.Is(err, errCertificateTXTUncertain) || creates != 1 {
		t.Fatalf("empty reconciliation must not duplicate an uncertain POST: %v", err)
	}
}

func TestCertificateTXTRejectsForeignChangedProofAndInvalidZone(t *testing.T) {
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6)}
	intent := certificateTXT{ID: strings.Repeat("d", 32), Name: "_acme-challenge.app.example.test", Content: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Comment: "cove-acme-" + strings.Repeat("b", 32)}
	for _, changed := range []string{"content", "id", "duplicate"} {
		t.Run(changed, func(t *testing.T) {
			s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("must not mutate a record without exact proof")
				}
				row := map[string]string{"type": "TXT", "id": intent.ID, "name": intent.Name, "content": intent.Content, "comment": intent.Comment}
				rows := []any{row}
				if changed == "duplicate" {
					rows = append(rows, row)
				} else {
					row[changed] = strings.Repeat("e", 32)
				}
				data, _ := json.Marshal(map[string]any{"success": true, "result": rows})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
			})}
			if err := s.removeCertificateTXT(context.Background(), zone, intent); err == nil {
				t.Fatal("changed ownership accepted")
			}
		})
	}
	for _, name := range []string{"_acme-challenge.foreign.test", "app.example.test", "_acme-challenge.APP.example.test"} {
		invalid := intent
		invalid.Name = name
		if validateCertificateTXT(zone, invalid) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestCertificateDNSIntentRetainsCleanupAfterIssuerAndResourceRemoval(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		lostBeforeCreate       bool
		preparationUnavailable bool
	}{{"accepted response lost", false, false}, {"unconfirmed create", true, false}, {"unavailable before create", false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			lostBeforeCreate := scenario.lostBeforeCreate
			preparationUnavailable := scenario.preparationUnavailable
			f := newAccountFixture(t)

			ctx := context.Background()
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('dns-node','Node','localhost',22,'',$1,'ready')`, []byte{})
			f.sql(`INSERT INTO projects(id,name,team_id) VALUES('dns-project','Project','default')`)
			resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "DNS", NodeID: "dns-node", ProjectID: "dns-project", Kind: "service", Compose: "services: {}"}, 201), "id")
			zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6), Enabled: true}
			protected, err := f.s.sealJSON(zone)
			if err != nil {
				t.Fatal(err)
			}
			f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, protected)
			f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['app.example.test'])`, zone.ZoneID)
			base := "/api/workloads/" + resource
			issuer := jsonString(t, f.call("root", "default", "POST", base+"/certificate-issuers", map[string]any{"name": "DNS", "provider": "cloudflare_origin", "zone_id": zone.ZoneID, "domains": []string{"app.example.test"}, "validity_days": 7, "auto_renew": false}, 202), "id")
			intent, cycle := resourceID(), resourceID()
			protected, err = f.s.sealJSON(certificateACMEIssuerState{Directory: "https://ca.example.test/directory", CycleID: cycle, IntentID: intent})
			if err != nil {
				t.Fatal(err)
			}
			f.sql(`UPDATE route_certificate_issuers SET provider='acme_dns',acme_state=$2 WHERE id=$1`, issuer, protected)
			proof := certificateTXT{Name: "_acme-challenge.app.example.test", Content: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Comment: "cove-acme-" + resourceID()}
			protected, err = f.s.sealJSON(certificateDNSIntentState{Record: proof})
			if err != nil {
				t.Fatal(err)
			}
			f.sql(`INSERT INTO route_certificate_dns_intents(id,issuer_id,workload_id,zone_id,cycle_id,state) VALUES($1,$2,$3,$4,$5,$6)`, intent, issuer, resource, zone.ZoneID, cycle, protected)
			foreign := certificateTXT{ID: strings.Repeat("f", 32), Name: proof.Name, Content: "unrelated-value", Comment: "another-owner"}
			records := map[string]certificateTXT{foreign.ID: foreign}
			creates, deletes := 0, 0
			f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer "+zone.Token || r.URL.Host != "api.cloudflare.com" {
					t.Fatal("unexpected provider destination")
				}
				var result any
				switch r.Method {
				case "GET":
					if preparationUnavailable {
						return nil, errors.New("DNS listing unavailable")
					}
					list := []map[string]string{}
					for _, record := range records {
						list = append(list, map[string]string{"id": record.ID, "type": "TXT", "name": record.Name, "content": record.Content, "comment": record.Comment})
					}
					result = list
				case "POST":
					creates++
					if !lostBeforeCreate {
						var record certificateTXT
						if json.NewDecoder(r.Body).Decode(&record) != nil || record.Comment != proof.Comment || record.Content != proof.Content {
							t.Fatal("unexpected challenge body")
						}
						record.ID = resourceID()
						records[record.ID] = record
					}
					return nil, errors.New("response unavailable")
				case "DELETE":
					id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					if id == foreign.ID || records[id].Comment != proof.Comment {
						t.Fatal("attempt to remove foreign DNS")
					}
					delete(records, id)
					deletes++
					result = map[string]string{"id": id}
				default:
					t.Fatal(r.Method)
				}
				body, _ := json.Marshal(map[string]any{"success": true, "result": result})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
			})
			process := func() {
				t.Helper()
				if err := f.s.processCertificateDNSIntent(ctx, intent, false); err != nil {
					t.Fatal(err)
				}
			}
			process()
			if preparationUnavailable {
				var initialPhase string
				if err := f.s.DB.QueryRow(ctx, `SELECT phase,state FROM route_certificate_dns_intents WHERE id=$1`, intent).Scan(&initialPhase, &protected); err != nil {
					t.Fatal(err)
				}
				raw, err := unseal(f.s.Key, protected)
				var initial certificateDNSIntentState
				if err != nil || json.Unmarshal(raw, &initial) != nil || initial.Attempted || creates != 0 || initialPhase != "pending" {
					t.Fatal("a failed preflight was recorded as an uncertain DNS create", initialPhase, creates, err)
				}
				preparationUnavailable = false
				process()
			}
			process()
			if creates != 1 {
				t.Fatal("DNS create repeated after unknown outcome", creates)
			}
			var phase string
			if err := f.s.DB.QueryRow(ctx, `SELECT phase,state FROM route_certificate_dns_intents WHERE id=$1`, intent).Scan(&phase, &protected); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(protected), proof.Content) || strings.Contains(string(protected), proof.Comment) {
				t.Fatal("DNS ownership state was not sealed")
			}
			if !lostBeforeCreate && phase != "present" {
				t.Fatal("accepted DNS create was not reconciled", phase)
			}
			viewer := f.user("dns-viewer", "dns-viewer@example.test")
			f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','dns-viewer','member','[{"scope":"project","scope_id":"dns-project","role":"viewer"}]')`)
			status := f.call(viewer, "default", "GET", base+"/certificate-challenges", nil, 200).Body.String()
			if !strings.Contains(status, proof.Name) || strings.Contains(status, proof.Content) || strings.Contains(status, proof.Comment) || strings.Contains(status, zone.Token) {
				t.Fatal("DNS status lost the hostname or leaked challenge ownership")
			}
			f.call(viewer, "default", "GET", "/api/cloudflare/"+zone.ZoneID+"/certificate-challenges", nil, 403)
			f.sql(`UPDATE cloudflare_zones SET owner_id='dns-viewer' WHERE zone_id=$1`, zone.ZoneID)
			f.call(viewer, "default", "GET", "/api/cloudflare/"+zone.ZoneID+"/certificate-challenges", nil, 200)
			f.sql(`DELETE FROM cloudflare_zone_teams WHERE zone_id=$1`, zone.ZoneID)
			process()
			if !lostBeforeCreate && deletes != 1 {
				t.Fatal("revoked hostname sharing prevented cleanup of the already-owned record")
			}
			f.call("root", "default", "DELETE", base+"/certificate-issuers/"+issuer, nil, 200)
			f.sql(`DELETE FROM workloads WHERE id=$1`, resource)
			process()
			process()
			var savedIssuer, savedResource *string
			if err := f.s.DB.QueryRow(ctx, `SELECT phase,issuer_id,workload_id FROM route_certificate_dns_intents WHERE id=$1`, intent).Scan(&phase, &savedIssuer, &savedResource); err != nil {
				t.Fatal(err)
			}
			if savedIssuer != nil || savedResource != nil || creates != 1 || records[foreign.ID] != foreign {
				t.Fatal("cleanup lost identity or changed foreign records")
			}
			status = f.call("root", "default", "GET", "/api/cloudflare/"+zone.ZoneID+"/certificate-challenges", nil, 200).Body.String()
			if strings.Contains(status, proof.Content) || strings.Contains(status, proof.Comment) || strings.Contains(status, zone.Token) || strings.Contains(status, proof.Name) != lostBeforeCreate {
				t.Fatal("zone cleanup status lost an unresolved orphan or exposed private proof")
			}
			if lostBeforeCreate {
				if phase != "cleanup" || deletes != 0 {
					t.Fatal("unknown late create ownership was discarded", phase, deletes)
				}
			} else if phase != "cleaned" || deletes != 1 || len(records) != 1 {
				t.Fatal("orphaned owned record was not cleaned", phase, deletes, len(records))
			}
			f.sql(`UPDATE route_certificate_dns_intents SET updated_at=now()-interval '8 days' WHERE id=$1`, intent)
			if err := f.s.scheduleCertificateDNSIntents(ctx); err != nil {
				t.Fatal(err)
			}
			var retained bool
			if err := f.s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM route_certificate_dns_intents WHERE id=$1)`, intent).Scan(&retained); err != nil || retained != lostBeforeCreate {
				t.Fatal("retention discarded unresolved ownership or retained obsolete confirmed cleanup", retained, err)
			}
		})
	}
}
