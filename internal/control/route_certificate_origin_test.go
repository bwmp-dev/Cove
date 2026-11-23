package control

import (
	"context"
	"cove/internal/worker"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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

func signOriginFixture(t *testing.T, csrDER []byte, extraName string) string {
	t.Helper()
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "CloudFlare Origin Certificate"}, DNSNames: append([]string{}, csr.DNSNames...), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(12 * time.Hour)}
	if extraName != "" {
		template.DNSNames = append(template.DNSNames, extraName)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, csr.PublicKey, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestOriginCertificateUsesSavedCSRAndReconcilesAnAmbiguousCreate(t *testing.T) {
	domains := []string{"app.example.test", "*.preview.example.test"}
	csr, key, err := worker.NewRouteCertificateKey(domains)
	if err != nil {
		t.Fatal(err)
	}
	certificate := signOriginFixture(t, csr, "")
	unrelated, _ := certificateFixture(t, "app.example.test")
	calls := 0
	post := true
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "api.cloudflare.com" || r.URL.Path != "/client/v4/certificates" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture", 6) {
			t.Fatal("wrong certificate API destination or credential")
		}
		if post {
			if r.Method != "POST" {
				t.Fatal(r.Method)
			}
			var body struct {
				CSR       string   `json:"csr"`
				Hostnames []string `json:"hostnames"`
				Kind      string   `json:"request_type"`
				Validity  int      `json:"requested_validity"`
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil || json.Unmarshal(raw, &body) != nil {
				t.Fatal("invalid CSR request")
			}
			if strings.Contains(string(raw), "PRIVATE KEY") || body.Kind != "origin-ecc" || body.Validity != 90 || len(body.Hostnames) != 2 {
				t.Fatal("private key was sent or certificate request changed")
			}
			block, _ := pem.Decode([]byte(body.CSR))
			if block == nil || string(block.Bytes) != string(csr) {
				t.Fatal("saved CSR changed")
			}
			return nil, errors.New("fixture connection lost after provider accepted CSR")
		}
		if r.Method != "GET" || r.URL.Query().Get("zone_id") != strings.Repeat("a", 32) {
			t.Fatal("wrong reconciliation zone")
		}
		data, _ := json.Marshal(map[string]any{"success": true, "result": []originCertificate{{ID: "111", Certificate: unrelated}, {ID: "222", Certificate: certificate}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})}
	token := strings.Repeat("fixture", 6)
	if _, err := s.createOriginCertificate(context.Background(), token, csr, domains, 90); !errors.Is(err, errOriginCertificateUncertain) {
		t.Fatal("ambiguous create could be blindly retried", err)
	}
	post = false
	material := worker.RouteCertificate{ID: strings.Repeat("b", 32), PrivateKeyPEM: key}
	found, err := s.findOriginCertificate(context.Background(), token, strings.Repeat("a", 32), material, csr)
	if err != nil || found == nil || found.ID != "222" || calls != 2 {
		t.Fatal("issued CSR was not recovered without reissuing", err)
	}
	material.CertificatePEM = found.Certificate
	if _, err := validateIssuedRouteCertificate(material, csr, time.Now()); err != nil {
		t.Fatal(err)
	}
	material.CertificatePEM = signOriginFixture(t, csr, "victim.example.test")
	if _, err := validateIssuedRouteCertificate(material, csr, time.Now()); err == nil {
		t.Fatal("provider broadened requested DNS names")
	}
}

func TestOriginCertificateAPIRefusesUnexpectedPathsAndReportsPermissionFailures(t *testing.T) {
	calls := 0
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("untrusted provider body"))}, nil
	})}
	token := strings.Repeat("fixture", 6)
	for _, path := range []string{"/accounts", "/certificates?zone_id=" + strings.Repeat("a", 32) + "&zone_id=" + strings.Repeat("b", 32), "https://example.test/certificates?zone_id=" + strings.Repeat("a", 32)} {
		if err := s.originCertificateAPI(context.Background(), token, "GET", path, nil, new([]originCertificate)); err == nil {
			t.Fatal("unexpected API path accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid requests reached provider")
	}
	if err := s.originCertificateAPI(context.Background(), token, "POST", "/certificates", map[string]string{}, new(originCertificate)); err == nil || errors.Is(err, errOriginCertificateUncertain) || !strings.Contains(err.Error(), "SSL and Certificates Edit") || strings.Contains(err.Error(), "untrusted provider body") {
		t.Fatal("permission failure lost its meaning or exposed provider data", err)
	}
}

func TestOriginCertificateAmbiguousStatusRetainsRequestWithoutFollowingRedirects(t *testing.T) {
	for _, status := range []int{302, 303, 408, 500, 502, 403, 429} {
		calls := 0
		s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "api.cloudflare.com" {
				t.Fatal("provider redirect received the credential")
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://unrelated.example.test/result"}}, Body: io.NopCloser(strings.NewReader("provider details must remain private"))}, nil
		})}
		err := s.originCertificateAPI(context.Background(), strings.Repeat("fixture", 6), "POST", "/certificates", map[string]string{}, new(originCertificate))
		uncertain := status < 400 || status >= 500 || status == 408
		if err == nil || errors.Is(err, errOriginCertificateUncertain) != uncertain || calls != 1 || strings.Contains(err.Error(), "provider details") {
			t.Fatal("provider outcome was misclassified or redirect followed", status, calls, err)
		}
	}
}
