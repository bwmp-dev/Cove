package worker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRouteCertificateProbePinsTheSelectedLeaf(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "*.example.test")
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	address := server.Listener.Addr().String()
	if err := CheckRouteCertificateTLS(context.Background(), address, "app.example.test", string(certificate)); err != nil {
		t.Fatal("explicit leaf trust failed", err)
	}
	other, _ := selfSignedCertificate(t, "*.example.test")
	if err := CheckRouteCertificateTLS(context.Background(), address, "app.example.test", string(other)); err == nil {
		t.Fatal("another certificate was accepted")
	}
	if err := CheckRouteCertificateTLS(context.Background(), address, "unrelated.example.invalid", string(certificate)); err == nil {
		t.Fatal("unrelated probe hostname accepted")
	}
	for _, endpoint := range []string{"169.254.169.254:443", "10.0.0.1:443", "localhost:443", "127.0.0.1:0"} {
		if err := CheckRouteCertificateTLS(context.Background(), endpoint, "app.example.test", string(certificate)); err == nil {
			t.Fatal("unsafe probe endpoint accepted", endpoint)
		}
	}
}

func TestRouteCertificatePersistenceIsImmutableAndScoped(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := selfSignedCertificate(t, "app.example.test")
	bundle := RouteCertificate{ID: strings.Repeat("d", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	if err := e.saveRouteCertificate("app", bundle); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{filepath.Join(e.Root, "workloads", "app", "certificates"), 0700}, {filepath.Join(e.Root, "workloads", "app", "certificates", bundle.ID+".json"), 0600}} {
			info, err := os.Stat(item.path)
			if err != nil || info.Mode().Perm() != item.mode {
				t.Fatal("certificate permissions are not private", info, err)
			}
		}
	}
	if err := e.saveRouteCertificate("app", bundle); err != nil {
		t.Fatal("same certificate replay rejected", err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := e.readRouteCertificate("app", bundle.ID)
	if err != nil || saved != bundle {
		t.Fatal("certificate did not survive restart", err)
	}
	otherCert, otherKey := selfSignedCertificate(t, "app.example.test")
	changed := bundle
	changed.CertificatePEM = string(otherCert)
	changed.PrivateKeyPEM = string(otherKey)
	if err := e.saveRouteCertificate("app", changed); err == nil {
		t.Fatal("certificate ID was overwritten")
	}
	if _, err := e.readRouteCertificate("another-app", bundle.ID); !os.IsNotExist(err) {
		t.Fatal("certificate crossed resource boundary", err)
	}
	for _, pair := range [][2]string{{"../app", bundle.ID}, {"app", "../" + bundle.ID}} {
		if _, err := e.readRouteCertificate(pair[0], pair[1]); err == nil {
			t.Fatal("unsafe certificate path accepted")
		}
	}
	path := filepath.Join(e.Root, "workloads", "app", "certificates", bundle.ID+".json")
	if err := os.WriteFile(path, []byte(`{"certificate":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.saveRouteCertificate("app", bundle); err == nil {
		t.Fatal("unreadable certificate state was silently replaced")
	}
}

func TestRouteCertificateReplayPreservesExpiredPreviousMaterial(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"app.example.test"}, NotBefore: now.Add(-2 * time.Hour), NotAfter: now.Add(-time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := RouteCertificate{ID: strings.Repeat("e", 32), CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))}
	dir := filepath.Join(e.Root, "workloads", "app", "certificates")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, certificate.ID+".json"), storedRouteCertificate{Certificate: certificate, ValidatedAt: now.Add(-90 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := e.saveRouteCertificate("app", certificate); err != nil {
		t.Fatal("known previous certificate could not be replayed", err)
	}
	if err := e.saveRouteCertificate("new-app", certificate); err == nil {
		t.Fatal("expired certificate was newly admitted")
	}
}

func TestRouteCertificateCannotShadowOtherTLSNames(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, cn string
		names    []string
		ips      []net.IP
		owned    bool
	}{
		{"owned", "app.example.test", []string{"app.example.test"}, nil, true},
		{"unrelated SAN", "app.example.test", []string{"app.example.test", "victim.example.test"}, nil, false},
		{"unrelated CN", "victim.example.test", []string{"app.example.test"}, nil, false},
		{"CN separator", "victim.example.test,app.example.test", []string{"app.example.test"}, nil, false},
		{"IP SAN", "app.example.test", []string{"app.example.test"}, []net.IP{net.ParseIP("127.0.0.1")}, false},
		{"IP CN", "127.0.0.1", []string{"app.example.test"}, nil, false},
		{"single-label CN", "internal", []string{"app.example.test"}, nil, false},
		{"Origin CA text CN", "CloudFlare Origin Certificate", []string{"app.example.test"}, nil, true},
		{"single-label wildcard CN", "*", []string{"app.example.test"}, nil, false},
		{"malformed wildcard SAN", "app.example.test", []string{"app.example.test", "*.*"}, nil, false},
		{"unowned wildcard", "*.example.test", []string{"*.example.test"}, nil, false},
		{"top-level wildcard", "*.test", []string{"app.example.test", "*.test"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: test.cn}, DNSNames: test.names, IPAddresses: test.ips, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			certificate := RouteCertificate{CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
			if err := CertificateOwnedByRoutes(certificate, []Route{{Domain: "app.example.test"}}); (err == nil) != test.owned {
				t.Fatal(err)
			}
			if test.name == "unowned wildcard" {
				if err := CertificateOwnedByRoutes(certificate, []Route{{Domain: "*.example.test"}}); err != nil {
					t.Fatal("explicitly owned wildcard rejected", err)
				}
			}
		})
	}
}

func TestRouteCertificateValidationAndHostnameCoverage(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "*.example.test")
	bundle := RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	info, err := ParseRouteCertificate(bundle, time.Now())
	if err != nil || len(info.Fingerprint) != 64 || len(info.DNSNames) != 1 || info.DNSNames[0] != "*.example.test" || !info.ExpiresAt.After(time.Now()) {
		t.Fatal(info, err)
	}
	for _, domain := range []string{"app.example.test", "*.example.test"} {
		if err := CertificateCoversRoute(bundle, domain); err != nil {
			t.Fatal(domain, err)
		}
	}
	for _, domain := range []string{"example.test", "deep.app.example.test", "*.app.example.test", "other.example.invalid"} {
		if err := CertificateCoversRoute(bundle, domain); err == nil {
			t.Fatal("certificate covered unrelated hostname", domain)
		}
	}
	_, otherKey := selfSignedCertificate(t, "other.example.test")
	for _, change := range []func(*RouteCertificate){
		func(c *RouteCertificate) { c.PrivateKeyPEM = string(otherKey) },
		func(c *RouteCertificate) { c.CertificatePEM = strings.Repeat("x", 65537) },
		func(c *RouteCertificate) { c.PrivateKeyPEM = strings.Repeat("x", 16385) },
		func(c *RouteCertificate) { c.PrivateKeyPEM += "unparsed data" },
		func(c *RouteCertificate) { c.CertificatePEM += "unparsed data" },
		func(c *RouteCertificate) { c.ID = "not-an-id" },
	} {
		changed := bundle
		change(&changed)
		if _, err := ParseRouteCertificate(changed, time.Now()); err == nil {
			t.Fatal("invalid certificate material accepted")
		}
	}
	if _, err := ParseRouteCertificate(bundle, info.ExpiresAt); err == nil {
		t.Fatal("expired certificate accepted")
	}
	if _, err := ParseRouteCertificate(bundle, info.NotBefore.Add(-time.Second)); err == nil {
		t.Fatal("future certificate accepted")
	}
}

func TestRouteCertificateRejectsCAAndClientOnlyCertificates(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, ca := range []bool{false, true} {
		template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"app.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if ca {
			template.KeyUsage |= x509.KeyUsageCertSign
			template.ExtKeyUsage = nil
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		bundle := RouteCertificate{ID: strings.Repeat("b", 32), CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))}
		if _, err := ParseRouteCertificate(bundle, time.Now()); err == nil {
			t.Fatal("non-server certificate accepted", ca)
		}
	}
}

func TestRouteCertificateChainRequiresCorrectOrderAndValidIssuers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerTemplate := &x509.Certificate{Subject: pkix.Name{CommonName: "Cove test issuer"}, SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, issuerTemplate, &issuerKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "app.example.test"}, SerialNumber: big.NewInt(2), DNSNames: []string{"app.example.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, issuer, &leafKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))
	issuerPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuerDER}))
	bundle := RouteCertificate{ID: strings.Repeat("c", 32), CertificatePEM: leafPEM + issuerPEM, PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))}
	if _, err := ParseRouteCertificate(bundle, now); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair([]byte(bundle.CertificatePEM), []byte(bundle.PrivateKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	pair.Certificate = pair.Certificate[:1]
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	if err := CheckRouteCertificateTLS(context.Background(), server.Listener.Addr().String(), "app.example.test", bundle.CertificatePEM); err != nil {
		t.Fatal("explicit trust of a CA-signed leaf failed", err)
	}
	if _, err := ParseRouteCertificate(bundle, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired issuer accepted")
	}
	bundle.CertificatePEM = issuerPEM + leafPEM
	if _, err := ParseRouteCertificate(bundle, now); err == nil {
		t.Fatal("reversed chain accepted")
	}
}

func TestUploadedCertificateProtocolRejectsOlderMethodsAndReplaysAfterRestart(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, key := selfSignedCertificate(t, "app.example.test")
	certificate := RouteCertificate{ID: strings.Repeat("b", 32), CertificatePEM: string(cert), PrivateKeyPEM: string(key)}
	request := Request{ID: "cert-protocol", Workload: "app", Action: "deploy", Kind: "service", Compose: "services: {}", Routes: []Route{{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: certificate.ID}}}, Certificates: []RouteCertificate{certificate}}
	for _, method := range []string{"submit", "submit_routes_v2"} {
		if response := e.RPC(context.Background(), RPCRequest{Method: method, Request: &request}); response.Error == "" {
			t.Fatal("old method accepted certificate bundle", method)
		}
	}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit_routes_v3", Request: &request}); response.Error != "" || response.Operation == nil {
		t.Fatal(response.Error)
	}
	data, err := os.ReadFile(e.path(request.ID))
	if err != nil || !bytes.Contains(data, []byte(protectedRouteSource)) {
		t.Fatal("persisted certificate selection lacked downgrade guard", err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(request); err != nil {
		t.Fatal("certificate request changed digest after restart", err)
	}
	wrong := request
	wrong.Certificates = append([]RouteCertificate{}, request.Certificates...)
	wrong.Certificates[0].ID = strings.Repeat("c", 32)
	if err := ValidateRequest(wrong); err == nil {
		t.Fatal("unreferenced certificate key material accepted")
	}
}

func TestRouteCertificateProbeMountsOnlyPublicConfigurationAsWorkerUser(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, key := selfSignedCertificate(t, "app.example.test")
	material := RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(cert), PrivateKeyPEM: string(key)}
	if err := e.saveRouteCertificate("app", material); err != nil {
		t.Fatal(err)
	}
	called := false
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "image" && args[2] == "inspect" {
			fmt.Fprint(out, "sha256:fixture")
			return nil
		}
		if len(args) < 3 || args[1] != "run" {
			t.Fatal("unexpected certificate probe command", args)
		}
		called = true
		mount, user := "", ""
		for i, arg := range args {
			if arg == "--mount" {
				mount = args[i+1]
			}
			if arg == "--user" {
				user = args[i+1]
			}
		}
		expected := "type=bind,source=" + filepath.Join(e.Root, "certificate-probes.json") + ",target=/config/probes.json,readonly"
		if mount != expected || strings.Contains(strings.Join(args, " "), "type=volume") || user != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
			t.Fatal("probe gained access to private proxy volume or wrong user", args)
		}
		return nil
	}
	routes := map[string][]Route{"app": {{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: material.ID}}}}
	if err := e.confirmRouteCertificates(context.Background(), routes); err != nil || !called {
		t.Fatal("certificate was not checked", err)
	}
}
