package worker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var certificateIdentifier = regexp.MustCompile(`^[a-f0-9]{32}$`)

type RouteCertificate struct {
	ID             string `json:"id"`
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

type RouteCertificateInfo struct {
	Fingerprint string    `json:"fingerprint"`
	CommonName  string    `json:"common_name,omitempty"`
	DNSNames    []string  `json:"dns_names"`
	IPAddresses []string  `json:"ip_addresses,omitempty"`
	NotBefore   time.Time `json:"not_before"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type storedRouteCertificate struct {
	Certificate RouteCertificate `json:"certificate"`
	ValidatedAt time.Time        `json:"validated_at"`
}

func CheckRouteCertificateTLS(ctx context.Context, address, hostname, certificatePEM string) error {
	return checkRouteCertificateTLS(ctx, address, hostname, certificatePEM, time.Now())
}

func checkRouteCertificateTLS(ctx context.Context, address, hostname, certificatePEM string, verifiedAt time.Time) error {
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("certificate probes require a literal loopback endpoint")
	}
	if strings.ContainsRune(hostname, '*') || ValidateRoutes([]Route{{Domain: hostname, Service: "certificate", Port: 443}}, "service") != nil {
		return errors.New("certificate probe requires an exact lowercase DNS hostname")
	}
	if len(certificatePEM) > 65536 {
		return errors.New("certificate probe exceeds its certificate size limit")
	}
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("certificate probe requires a PEM server certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || leaf.VerifyHostname(hostname) != nil {
		return errors.New("certificate probe hostname is not covered by its certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	expected := sha256.Sum256(leaf.Raw)
	config := &tls.Config{Time: func() time.Time { return verifiedAt }, RootCAs: roots, ServerName: hostname, MinVersion: tls.VersionTLS12, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || sha256.Sum256(state.PeerCertificates[0].Raw) != expected {
			return errors.New("proxy certificate does not match the selected certificate")
		}
		return nil
	}}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := (&tls.Dialer{Config: config}).DialContext(probe, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("managed proxy is not serving the selected route certificate")
	}
	defer connection.Close()
	return nil
}

func (e *Engine) readRouteCertificate(workload, id string) (RouteCertificate, error) {
	var stored storedRouteCertificate
	if !identifier.MatchString(workload) || len(id) != 32 {
		return RouteCertificate{}, errors.New("invalid resource or certificate ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return RouteCertificate{}, errors.New("invalid certificate ID")
	}
	f, err := os.Open(filepath.Join(e.Root, "workloads", workload, "certificates", id+".json"))
	if err != nil {
		return RouteCertificate{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return RouteCertificate{}, err
	}
	if info.Size() > 160*1024 {
		return RouteCertificate{}, errors.New("stored route certificate exceeds its size limit")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 160*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return RouteCertificate{}, errors.New("stored route certificate is unreadable")
	}
	if err := decoder.Decode(new(any)); err != io.EOF || stored.Certificate.ID != id || stored.ValidatedAt.IsZero() {
		return RouteCertificate{}, errors.New("stored route certificate identity is invalid")
	}
	// Recovery must be able to restore the previous certificate even after it has expired.
	if _, err := ParseRouteCertificate(stored.Certificate, stored.ValidatedAt); err != nil {
		return RouteCertificate{}, errors.New("stored route certificate material is invalid")
	}
	return stored.Certificate, nil
}

func (e *Engine) saveRouteCertificate(workload string, certificate RouteCertificate) error {
	if !identifier.MatchString(workload) {
		return errors.New("invalid resource ID")
	}
	existing, err := e.readRouteCertificate(workload, certificate.ID)
	if err == nil {
		if existing != certificate {
			return errors.New("certificate IDs are immutable; upload a new certificate version")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := time.Now().UTC()
	if _, err := ParseRouteCertificate(certificate, now); err != nil {
		return err
	}
	dir := filepath.Join(e.Root, "workloads", workload, "certificates")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, certificate.ID+".json"), storedRouteCertificate{Certificate: certificate, ValidatedAt: now})
}

func ParseRouteCertificate(c RouteCertificate, now time.Time) (RouteCertificateInfo, error) {
	var info RouteCertificateInfo
	if len(c.ID) != 32 {
		return info, errors.New("invalid route certificate ID")
	}
	if _, err := hex.DecodeString(c.ID); err != nil {
		return info, errors.New("invalid route certificate ID")
	}
	return ParseTLSCertificate(c.CertificatePEM, c.PrivateKeyPEM, now, false)
}

// Native database clients can explicitly trust a self-signed server CA; the shared HTTP SNI store cannot.
func ParseTLSCertificate(certificatePEM, privateKeyPEM string, now time.Time, allowSelfSignedCA bool) (RouteCertificateInfo, error) {
	var info RouteCertificateInfo
	if len(certificatePEM) == 0 || len(certificatePEM) > 65536 || len(privateKeyPEM) == 0 || len(privateKeyPEM) > 16384 {
		return info, errors.New("certificate chain must be at most 64 KiB and private key at most 16 KiB")
	}
	remaining := []byte(certificatePEM)
	var chain []*x509.Certificate
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(chain) >= 16 {
			return info, errors.New("certificate chain must contain at most 16 PEM certificates")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return info, errors.New("certificate chain contains an invalid X.509 certificate")
		}
		if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
			return info, errors.New("certificate chain is not currently valid")
		}
		chain = append(chain, certificate)
		remaining = rest
	}
	if len(chain) == 0 {
		return info, errors.New("a PEM certificate chain is required")
	}
	for i := 1; i < len(chain); i++ {
		if err := chain[i-1].CheckSignatureFrom(chain[i]); err != nil {
			return info, errors.New("certificate chain must be ordered from server certificate to its issuers")
		}
	}
	block, rest := pem.Decode([]byte(privateKeyPEM))
	if block == nil || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || (block.Type != "PRIVATE KEY" && block.Type != "RSA PRIVATE KEY" && block.Type != "EC PRIVATE KEY") {
		return info, errors.New("an unencrypted PEM private key is required")
	}
	pair, err := tls.X509KeyPair([]byte(certificatePEM), []byte(privateKeyPEM))
	if err != nil {
		return info, errors.New("certificate and private key do not form a valid TLS key pair")
	}
	switch key := pair.PrivateKey.(type) {
	case *rsa.PrivateKey:
		if key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
			return info, errors.New("RSA certificate keys must contain between 2048 and 8192 bits")
		}
	case *ecdsa.PrivateKey:
		if key.Curve.Params().BitSize < 256 {
			return info, errors.New("ECDSA certificate keys must contain at least 256 bits")
		}
	case ed25519.PrivateKey:
	default:
		return info, errors.New("unsupported TLS private key type")
	}
	leaf := chain[0]
	if leaf.IsCA && (!allowSelfSignedCA || leaf.CheckSignatureFrom(leaf) != nil) || len(leaf.DNSNames) == 0 {
		return info, errors.New("a server certificate with DNS subject alternative names is required")
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return info, errors.New("certificate does not permit TLS signatures")
	}
	if len(leaf.ExtKeyUsage)+len(leaf.UnknownExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return info, errors.New("certificate does not permit TLS server authentication")
	}
	sum := sha256.Sum256(leaf.Raw)
	info = RouteCertificateInfo{Fingerprint: hex.EncodeToString(sum[:]), CommonName: leaf.Subject.CommonName, DNSNames: append([]string{}, leaf.DNSNames...), NotBefore: leaf.NotBefore, ExpiresAt: leaf.NotAfter}
	for _, ip := range leaf.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	slices.Sort(info.DNSNames)
	slices.Sort(info.IPAddresses)
	return info, nil
}

func CertificateOwnedByRoutes(c RouteCertificate, routes []Route) error {
	block, _ := pem.Decode([]byte(c.CertificatePEM))
	if block == nil {
		return errors.New("route certificate is unavailable")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("route certificate is invalid")
	}
	if len(leaf.IPAddresses) > 0 {
		return errors.New("managed resource certificates cannot register IP addresses in the shared TLS store")
	}
	names := append([]string{}, leaf.DNSNames...)
	if leaf.Subject.CommonName != "" {
		names = append(names, leaf.Subject.CommonName)
	}
	for _, raw := range names {
		if strings.ContainsRune(raw, ',') {
			return errors.New("certificate names cannot contain shared TLS store separators")
		}
		name := strings.TrimRight(strings.ToLower(raw), ".")
		if net.ParseIP(name) != nil {
			return errors.New("managed resource certificates cannot register IP addresses in the shared TLS store")
		}
		base := strings.TrimPrefix(name, "*.")
		dnsName := base != ""
		for _, label := range strings.Split(base, ".") {
			dnsName = dnsName && domainLabel.MatchString(label)
		}
		if !dnsName {
			if strings.ContainsRune(name, '*') {
				return errors.New("certificate names cannot contain malformed wildcards")
			}
			continue
		}
		owned := false
		for _, route := range routes {
			if route.Domain == name {
				owned = true
				break
			}
			if strings.HasPrefix(route.Domain, "*.") && !strings.HasPrefix(name, "*.") && strings.HasSuffix(name, route.Domain[1:]) {
				prefix := strings.TrimSuffix(name, route.Domain[1:])
				if prefix != "" && !strings.ContainsRune(prefix, '.') {
					owned = true
					break
				}
			}
		}
		if !owned {
			return errors.New("every certificate DNS name and DNS common name must belong to this resource's routes")
		}
	}
	return nil
}

func CertificateCoversRoute(c RouteCertificate, domain string) error {
	block, _ := pem.Decode([]byte(c.CertificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("route certificate is unavailable")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("route certificate is invalid")
	}
	if strings.HasPrefix(domain, "*.") {
		for _, name := range leaf.DNSNames {
			if strings.EqualFold(name, domain) {
				return nil
			}
		}
	} else if leaf.VerifyHostname(domain) == nil {
		return nil
	}
	return errors.New("certificate subject alternative names do not cover the route hostname")
}
