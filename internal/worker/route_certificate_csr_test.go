package worker

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

func TestRouteCertificateKeyBindsRequestedSANs(t *testing.T) {
	names := []string{"app.example.test", "*.preview.example.test"}
	der, keyPEM, err := NewRouteCertificateKey(names)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil || strings.Join(csr.DNSNames, ",") != strings.Join(names, ",") {
		t.Fatal(csr, err)
	}
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		t.Fatal("private key unavailable")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	private, ok := key.(*ecdsa.PrivateKey)
	if !ok || private.Curve.Params().BitSize != 256 {
		t.Fatal("unexpected signing key")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil || !bytes.Equal(publicDER, csr.RawSubjectPublicKeyInfo) {
		t.Fatal("CSR does not bind generated key", err)
	}
	for _, invalid := range [][]string{nil, {"*.com"}, {"foo.*.example.test"}, {"UPPER.example.test"}, {"app.example.test", "app.example.test"}, {"https://app.example.test"}} {
		if _, _, err := NewRouteCertificateKey(invalid); err == nil {
			t.Fatal("invalid CSR hostname accepted", invalid)
		}
	}
}
