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
	"math/big"
	"strings"
	"testing"
	"time"
)

func certificateFixture(t *testing.T, names ...string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
}

func TestRouteCertificatesAreSealedScopedAndExcludedFromPublicState(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cert-node','Node','localhost',22,'',$1,'ready','{"route_policies":true,"live_routes":true,"route_protocol":3}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cert-project','Project','default')`)
	create := func(name string) string {
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: name, NodeID: "cert-node", ProjectID: "cert-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	}
	resource, other := create("Certificates"), create("Other")
	path := "/api/workloads/" + resource
	certificate, key := certificateFixture(t, "app.example.test")
	upload := map[string]string{"name": "Private certificate", "certificate_pem": certificate, "private_key_pem": key}
	response := f.call("root", "default", "POST", path+"/certificates", upload, 201)
	id := jsonString(t, response, "id")
	if strings.Contains(response.Body.String(), "PRIVATE KEY") || strings.Contains(response.Body.String(), "certificate_pem") {
		t.Fatal("upload leaked material")
	}
	var stored []byte
	if err := f.s.DB.QueryRow(ctx, "SELECT material FROM route_certificates WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("PRIVATE KEY")) {
		t.Fatal("unsealed certificate key")
	}
	raw, err := unseal(f.s.Key, stored)
	var decoded worker.RouteCertificate
	if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded.PrivateKeyPEM != key {
		t.Fatal("sealed key missing", err)
	}
	viewer := f.user("cert-viewer", "cert-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cert-viewer','member','[{"scope":"project","scope_id":"cert-project","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", path+"/certificates", upload, 403)
	f.call(viewer, "default", "DELETE", path+"/certificates/"+id, nil, 403)
	list := f.call(viewer, "default", "GET", path+"/certificates", nil, 200).Body.String()
	if !strings.Contains(list, id) || strings.Contains(list, "PEM") || strings.Contains(list, "PRIVATE KEY") {
		t.Fatal("metadata listing incorrect", list)
	}
	route := worker.Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: id}}
	input := map[string]any{"routes": []worker.Route{route}}
	f.call("root", "default", "PUT", "/api/workloads/"+other+"/routes", input, 400)
	f.call("root", "default", "PUT", path+"/routes", input, 200)
	f.call("root", "default", "DELETE", path+"/certificates/"+id, nil, 409)
	for _, suffix := range []string{"", "/configuration-history"} {
		result := f.call(viewer, "default", "GET", path+suffix, nil, 200).Body.String()
		if strings.Contains(result, "PRIVATE KEY") || strings.Contains(result, "certificate_pem") || strings.Contains(result, "private_key_pem") {
			t.Fatal("certificate material leaked into public state", suffix)
		}
	}
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true,"live_routes":true,"route_protocol":2}' WHERE id='cert-node'`)
	if err := f.s.prepareInfrastructure(ctx, &worker.Request{Workload: resource, Action: "deploy", Routes: []worker.Route{route}}); err != errRouteProtocolUnsupported {
		t.Fatal("old worker accepted certificate", err)
	}
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true,"live_routes":true,"route_protocol":3}' WHERE id='cert-node'`)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('cert-deploy',$1,'deploy',$2,'succeeded')`, resource, []byte{})
	operation := jsonString(t, f.call("root", "default", "PUT", path+"/routes", map[string]any{"routes": []worker.Route{}}, 202), "id")
	f.call("root", "default", "DELETE", path+"/certificates/"+id, nil, 409)
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	config := workloadInput{Name: "Certificates", NodeID: "cert-node", ProjectID: "cert-project", Kind: "service", Compose: "services: {}"}
	sealed, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, resource, sealed)
	f.call("root", "default", "DELETE", "/api/workloads/"+other+"/certificates/"+id, nil, 404)
	f.call("root", "default", "DELETE", path+"/certificates/"+id, nil, 200)
}

func TestRouteCertificateBindingRejectsTLSShadowingAndFreezesSubmission(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cert-node','Node','localhost',22,'',$1,'ready','{"route_policies":true,"live_routes":true,"route_protocol":3}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cert-project','Project','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Certificates", NodeID: "cert-node", ProjectID: "cert-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	path := "/api/workloads/" + resource
	certificate, key := certificateFixture(t, "app.example.test", "victim.example.test")
	upload := map[string]string{"name": "Multi SAN", "certificate_pem": certificate, "private_key_pem": key}
	id := jsonString(t, f.call("root", "default", "POST", path+"/certificates", upload, 201), "id")
	route := worker.Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: id}}
	f.call("root", "default", "PUT", path+"/routes", map[string]any{"routes": []worker.Route{route}}, 400)
	second := route
	second.Domain = "victim.example.test"
	f.call("root", "default", "PUT", path+"/routes", map[string]any{"routes": []worker.Route{route, second}}, 200)
	request := worker.Request{ID: "freeze-cert", Workload: resource, Action: "routes", Kind: "service", Routes: []worker.Route{route, second}}
	initial, err := f.s.sealJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'routes',$3)`, request.ID, resource, initial)
	if err := f.s.prepareRouteCertificates(ctx, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Certificates) != 1 || request.Certificates[0].PrivateKeyPEM != key {
		t.Fatal("worker material missing")
	}
	var frozen []byte
	if err := f.s.DB.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", request.ID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(initial, frozen) || bytes.Contains(frozen, []byte("PRIVATE KEY")) {
		t.Fatal("payload was not sealed and frozen")
	}
	f.sql(`UPDATE route_certificates SET material=$2 WHERE id=$1`, id, []byte("unreadable replacement"))
	var retry worker.Request
	plain, err := unseal(f.s.Key, frozen)
	if err != nil || json.Unmarshal(plain, &retry) != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareRouteCertificates(ctx, &retry); err != nil || retry.Certificates[0] != request.Certificates[0] {
		t.Fatal("retry changed frozen worker request", err)
	}
	public := f.call("root", "default", "GET", "/api/operations/"+request.ID, nil, 200).Body.String()
	if strings.Contains(public, "PRIVATE KEY") || strings.Contains(public, "certificate_pem") {
		t.Fatal("public operation leaked material")
	}
}

func TestUploadedCertificateSelectionSurvivesPartialRouteEdits(t *testing.T) {
	old := worker.Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32)}}
	var input []routeInput
	if err := json.Unmarshal([]byte(`[{"domain":"app.example.test","service":"app","port":8080,"policy":{"security_headers":true}}]`), &input); err != nil {
		t.Fatal(err)
	}
	next, err := retainedRouteAuth(input, []worker.Route{old})
	if err != nil || next[0].Policy.CertificateID != old.Policy.CertificateID || next[0].Policy.CertificateSource != "uploaded" || !next[0].Policy.SecurityHeaders {
		t.Fatal("partial edit discarded certificate selection", next, err)
	}
	for _, clear := range []string{`"certificate_source":"acme"`, `"certificate_source":"default"`, `"certificate_id":null`} {
		raw := `[{"domain":"app.example.test","service":"app","port":8080,"policy":{` + clear + `}}]`
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			t.Fatal(err)
		}
		next, err = retainedRouteAuth(input, []worker.Route{old})
		if err != nil || next[0].Policy.CertificateID != "" {
			t.Fatal("explicit clear retained certificate", err)
		}
	}
}
