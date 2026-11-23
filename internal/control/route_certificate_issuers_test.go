package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCertificateIssuerSurvivesLostProviderResponseAndRenewsThroughRoutes(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('issuer-node','Node','localhost',22,'',$1,'ready','{"route_policies":true,"live_routes":true,"route_protocol":4}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('issuer-project','Project','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Issued", NodeID: "issuer-node", ProjectID: "issuer-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	zoneID := strings.Repeat("a", 32)
	zone := cloudflareZone{ZoneID: zoneID, Domain: "example.test", Token: strings.Repeat("fixture", 6), Enabled: true}
	protected, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,configuration) VALUES($1,$2,$3)`, zoneID, zone.Domain, protected)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['app.example.test'])`, zoneID)
	creates, lookups := 0, 0
	issued := []originCertificate{}
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+zone.Token || r.URL.Path != "/client/v4/certificates" {
			t.Fatal("wrong provider destination or credential")
		}
		var result any
		if r.Method == "POST" {
			creates++
			body, err := io.ReadAll(r.Body)
			if err != nil || bytes.Contains(body, []byte("PRIVATE KEY")) {
				t.Fatal("provider received private key material")
			}
			var request struct {
				CSR string `json:"csr"`
			}
			if json.Unmarshal(body, &request) != nil {
				t.Fatal("invalid request")
			}
			block, _ := pem.Decode([]byte(request.CSR))
			if block == nil {
				t.Fatal("missing CSR")
			}
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil || csr.CheckSignature() != nil {
				t.Fatal("invalid CSR")
			}
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(creates)), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			if creates == 1 {
				leaf.NotBefore, leaf.NotAfter = time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour)
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, csr.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			certificate := originCertificate{ID: strings.Repeat("b", 31) + string(rune('0'+creates)), Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
			issued = append(issued, certificate)
			if creates == 1 {
				return nil, errors.New("provider committed before response was lost")
			}
			result = certificate
		} else if r.Method == "GET" {
			lookups++
			result = issued
		} else {
			t.Fatal("issuer must not revoke provider certificates")
		}
		body, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})
	path := "/api/workloads/" + resource
	request := map[string]any{"name": "Origin", "provider": "cloudflare_origin", "zone_id": zoneID, "domains": []string{"app.example.test"}, "validity_days": 7, "auto_renew": true}
	id := jsonString(t, f.call("root", "default", "POST", path+"/certificate-issuers", request, 202), "id")
	viewer := f.user("issuer-viewer", "issuer-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','issuer-viewer','member','[{"scope":"project","scope_id":"issuer-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path+"/certificate-issuers", nil, 200)
	f.call(viewer, "default", "GET", path+"/certificate-zones", nil, 200)
	f.call(viewer, "default", "POST", path+"/certificate-issuers", request, 403)
	f.call(viewer, "default", "PUT", path+"/certificate-issuers/"+id, map[string]bool{"auto_renew": false}, 403)
	f.call(viewer, "default", "DELETE", path+"/certificate-issuers/"+id, nil, 403)
	f.call("root", "default", "DELETE", path, nil, 409)
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	var phase, firstID string
	if err := f.s.DB.QueryRow(ctx, "SELECT phase,state FROM route_certificate_issuers WHERE id=$1", id).Scan(&phase, &protected); err != nil || phase != "reconciling" || bytes.Contains(protected, []byte("PRIVATE KEY")) {
		t.Fatal("lost request was not durably protected", phase, err)
	}
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(ctx, "SELECT phase,certificate_id FROM route_certificate_issuers WHERE id=$1", id).Scan(&phase, &firstID); err != nil || phase != "issued" || creates != 1 || lookups != 1 {
		t.Fatal("issuance was duplicated or not recovered", phase, creates, lookups, err)
	}
	for _, suffix := range []string{"/certificate-issuers", "/certificates", "/certificate-zones"} {
		body := f.call("root", "default", "GET", path+suffix, nil, 200).Body.String()
		if strings.Contains(body, "PRIVATE KEY") || strings.Contains(body, "private_key_pem") || strings.Contains(body, zone.Token) {
			t.Fatal("private issuer state leaked", suffix)
		}
	}
	f.call("root", "default", "DELETE", path+"/certificates/"+firstID, nil, 409)
	route := worker.Route{Domain: "app.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: firstID}}
	f.call("root", "default", "PUT", path+"/routes", map[string]any{"routes": []worker.Route{route}}, 200)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('issuer-initial-deploy',$1,'deploy',$2,'succeeded')`, resource, []byte{})
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	var operation, nextID string
	if err := f.s.DB.QueryRow(ctx, "SELECT phase,certificate_id,operation_id FROM route_certificate_issuers WHERE id=$1", id).Scan(&phase, &nextID, &operation); err != nil || phase != "applying" || nextID == firstID || operation == "" || creates != 2 {
		t.Fatal("renewal did not queue a new immutable version", phase, operation, creates, err)
	}
	if err := f.s.DB.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", operation).Scan(&protected); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, protected)
	var queued worker.Request
	if err != nil || json.Unmarshal(raw, &queued) != nil || queued.Action != "routes" || queued.Routes[0].Policy.CertificateID != nextID {
		t.Fatal("renewal bypassed the route operation")
	}
	f.call("root", "default", "DELETE", path+"/certificate-issuers/"+id, nil, 409)
	f.sql(`UPDATE operations SET state='failed' WHERE id=$1`, operation)
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	f.sql(`DELETE FROM cloudflare_zone_teams WHERE zone_id=$1 AND team_id='default'`, zoneID)
	f.call("root", "default", "PUT", path+"/certificate-issuers/"+id, map[string]bool{"retry": true}, 200)
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='routes'", resource).Scan(&count); err != nil || count != 1 || creates != 2 {
		t.Fatal("revoked share permitted a route renewal", count, creates, err)
	}
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['app.example.test'])`, zoneID)
	f.call("root", "default", "PUT", path+"/certificate-issuers/"+id, map[string]bool{"retry": true}, 200)
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	var retryID string
	if err := f.s.DB.QueryRow(ctx, "SELECT certificate_id,operation_id FROM route_certificate_issuers WHERE id=$1", id).Scan(&retryID, &operation); err != nil || retryID != nextID || creates != 2 {
		t.Fatal("retry reissued instead of applying the retained version", retryID, creates, err)
	}
	if err := f.s.commitRoutes(ctx, operation, queued, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processCertificateIssuer(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "DELETE", path+"/certificate-issuers/"+id, nil, 200)
	f.call("root", "default", "GET", path+"/certificates", nil, 200)
}

func TestCertificateDomainGrantNeverTurnsWildcardPermissionIntoApexPermission(t *testing.T) {
	for _, test := range []struct {
		pattern, domain string
		allowed         bool
	}{{"*.example.test", "*.example.test", true}, {"*.example.test", "example.test", false}, {"example.test", "*.example.test", false}, {"*.example.test", "*.sub.example.test", true}, {"app.example.test", "app.example.test", true}, {"*.example.test", "*.foreign.test", false}} {
		if cloudflareDomainAllowed([]string{test.pattern}, test.domain) != test.allowed {
			t.Fatal(test)
		}
	}
}
