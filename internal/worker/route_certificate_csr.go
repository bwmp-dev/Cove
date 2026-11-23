package worker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
)

func NewRouteCertificateKey(domains []string) ([]byte, string, error) {
	if len(domains) == 0 || len(domains) > 32 {
		return nil, "", errors.New("certificate requires between one and 32 DNS hostnames")
	}
	seen := map[string]bool{}
	for _, domain := range domains {
		base := strings.TrimPrefix(domain, "*.")
		if seen[domain] || ValidateRoutes([]Route{{Domain: base, Service: "certificate", Port: 443}}, "service") != nil {
			return nil, "", errors.New("certificate hostnames must be unique lowercase DNS names, optionally beginning with *.")
		}
		seen[domain] = true
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: domains}, key)
	if err != nil {
		return nil, "", err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, "", err
	}
	return csr, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})), nil
}
