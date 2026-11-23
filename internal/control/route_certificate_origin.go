package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var errOriginCertificateUncertain = errors.New("Cloudflare may have issued the certificate; reconcile the saved CSR before submitting it again")

var originCertificateValidityDays = []int{7, 30, 90, 365, 730, 1095, 5475}

type originCertificate struct {
	ID          string `json:"id"`
	Certificate string `json:"certificate"`
}

func validateIssuedRouteCertificate(certificate worker.RouteCertificate, csrDER []byte, now time.Time) (worker.RouteCertificateInfo, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil || len(csr.DNSNames) == 0 || len(csr.DNSNames) > 32 || len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 || len(csr.URIs) > 0 {
		return worker.RouteCertificateInfo{}, errors.New("saved certificate request is invalid")
	}
	info, err := worker.ParseRouteCertificate(certificate, now)
	if err != nil {
		return info, err
	}
	block, _ := pem.Decode([]byte(certificate.CertificatePEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return info, errors.New("issued certificate is invalid")
	}
	actual, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return info, err
	}
	expected, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return info, err
	}
	requested := append([]string{}, csr.DNSNames...)
	slices.Sort(requested)
	if !bytes.Equal(actual, expected) || !slices.Equal(info.DNSNames, requested) {
		return info, errors.New("issued certificate does not match the saved request's key and exact DNS names")
	}
	routes := make([]worker.Route, 0, len(requested))
	for _, domain := range requested {
		routes = append(routes, worker.Route{Domain: domain})
	}
	if err := worker.CertificateOwnedByRoutes(certificate, routes); err != nil {
		return info, err
	}
	return info, nil
}

func (s *Server) originCertificateAPI(ctx context.Context, token, method, path string, body any, result any) error {
	if !validCloudflareToken(token) {
		return errors.New("Cloudflare credentials are unavailable")
	}
	if method == "POST" {
		if path != "/certificates" {
			return errors.New("invalid certificate API request")
		}
	} else if method == "GET" {
		parsed, err := url.Parse(path)
		if err != nil || parsed.Path != "/certificates" || parsed.Fragment != "" || parsed.Scheme != "" || parsed.Host != "" || parsed.RawPath != "" || len(parsed.Query()) != 1 || len(parsed.Query()["zone_id"]) != 1 || !cloudflareID.MatchString(parsed.Query().Get("zone_id")) {
			return errors.New("invalid certificate API request")
		}
	} else {
		return errors.New("invalid certificate API request")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, Transport: s.CloudflareTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if method == "POST" {
			return errOriginCertificateUncertain
		}
		return errors.New("Cloudflare certificate API could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if method == "POST" && (response.StatusCode < 400 || response.StatusCode >= 500 || response.StatusCode == 408) {
			return errOriginCertificateUncertain
		}
		return fmt.Errorf("Cloudflare certificate request failed (HTTP %d); the connection needs SSL and Certificates Edit permission for this zone", response.StatusCode)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024))
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || !envelope.Success || json.Unmarshal(envelope.Result, result) != nil {
		if method == "POST" {
			return errOriginCertificateUncertain
		}
		return errors.New("Cloudflare returned an invalid certificate response")
	}
	return nil
}

func (s *Server) createOriginCertificate(ctx context.Context, token string, csrDER []byte, domains []string, validityDays int) (originCertificate, error) {
	var issued originCertificate
	if !slices.Contains(originCertificateValidityDays, validityDays) {
		return issued, errors.New("invalid Origin CA certificate validity")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil || len(domains) == 0 || len(domains) > 32 {
		return issued, errors.New("saved certificate request is invalid")
	}
	requested := append([]string{}, domains...)
	actual := append([]string{}, csr.DNSNames...)
	slices.Sort(requested)
	slices.Sort(actual)
	if !slices.Equal(requested, actual) {
		return issued, errors.New("saved certificate request DNS names do not match")
	}
	body := map[string]any{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), "hostnames": domains, "request_type": "origin-ecc", "requested_validity": validityDays}
	if err := s.originCertificateAPI(ctx, token, "POST", "/certificates", body, &issued); err != nil {
		return issued, err
	}
	if len(issued.ID) == 0 || len(issued.ID) > 128 || strings.ContainsAny(issued.ID, " /\\?&#\r\n") || len(issued.Certificate) == 0 || len(issued.Certificate) > 65536 {
		return originCertificate{}, errOriginCertificateUncertain
	}
	return issued, nil
}

func (s *Server) findOriginCertificate(ctx context.Context, token, zone string, request worker.RouteCertificate, csrDER []byte) (*originCertificate, error) {
	if !cloudflareID.MatchString(zone) {
		return nil, errors.New("invalid Cloudflare zone ID")
	}
	var certificates []originCertificate
	if err := s.originCertificateAPI(ctx, token, "GET", "/certificates?zone_id="+zone, nil, &certificates); err != nil {
		return nil, err
	}
	var selected *originCertificate
	var expires time.Time
	for _, certificate := range certificates {
		if len(certificate.ID) == 0 || len(certificate.ID) > 128 || strings.ContainsAny(certificate.ID, " /\\?&#\r\n") {
			return nil, errors.New("Cloudflare returned an invalid certificate identity")
		}
		material := request
		material.CertificatePEM = certificate.Certificate
		info, err := validateIssuedRouteCertificate(material, csrDER, time.Now())
		if err != nil {
			continue
		}
		if selected == nil || info.ExpiresAt.After(expires) {
			copy := certificate
			selected = &copy
			expires = info.ExpiresAt
		}
	}
	return selected, nil
}
