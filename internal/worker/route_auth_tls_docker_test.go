package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

func verifyPublicAuthTLSInFixture(t *testing.T, ctx context.Context) {
	t.Helper()
	// This alias exists only inside the disposable privileged fixture; traffic never leaves it.
	address := "8.0.0.2"
	if output, err := exec.CommandContext(ctx, "ip", "addr", "add", address+"/32", "dev", "lo").CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	defer exec.Command("ip", "addr", "del", address+"/32", "dev", "lo").Run()
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-auth"}, IPAddresses: []net.IP{net.ParseIP(address)}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.Header.Get("X-Cove-Auth-Hop") != "1" {
			t.Error("missing TLS or auth-hop guard")
		}
		w.Header().Set("Remote-User", "tls-user")
		w.WriteHeader(200)
	}))
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := publicAuthTransport(roots)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Cove-Auth-Hop", "1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("verified public-auth TLS request failed", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Remote-User") != "tls-user" {
		t.Fatal(response.StatusCode, response.Header)
	}
	untrusted := publicAuthTransport(x509.NewCertPool())
	defer untrusted.CloseIdleConnections()
	if response, err := (&http.Client{Transport: untrusted, Timeout: 5 * time.Second}).Do(request); err == nil {
		response.Body.Close()
		t.Fatal("public forward-auth accepted an untrusted TLS certificate")
	}
}
