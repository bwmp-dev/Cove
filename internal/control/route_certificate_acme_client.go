package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/acme"
)

type certificateACMETransport struct {
	Origin string
	Base   http.RoundTripper
}

func (t certificateACMETransport) CloseIdleConnections() {
	if closer, ok := t.Base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

var errCertificateACMEPending = errors.New("certificate authority validation or issuance is still pending")

func certificateACMEURL(client *acme.Client, endpoint string) bool {
	directory, err := url.Parse(client.DirectoryURL)
	if err != nil {
		return false
	}
	u, err := url.Parse(endpoint)
	return err == nil && len(endpoint) <= 2048 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && certificateAuthorityOrigin(u) == certificateAuthorityOrigin(directory)
}

func validateCertificateOrder(client *acme.Client, order *acme.Order, domains []string) error {
	if order == nil || !certificateACMEURL(client, order.URI) || len(order.Identifiers) != len(domains) || len(domains) == 0 || len(domains) > 32 || len(order.AuthzURLs) > 64 || !slices.Contains([]string{acme.StatusPending, acme.StatusReady, acme.StatusProcessing, acme.StatusValid}, order.Status) {
		return errors.New("certificate authority returned an invalid or rejected order")
	}
	if order.Status == acme.StatusValid && !certificateACMEURL(client, order.CertURL) || order.Status != acme.StatusValid && !order.Expires.IsZero() && !order.Expires.After(time.Now()) {
		return errors.New("certificate authority order is expired or its issued certificate is unavailable")
	}
	actual := make([]string, len(order.Identifiers))
	for i, identifier := range order.Identifiers {
		if identifier.Type != "dns" {
			return errors.New("certificate authority changed the requested identifier type")
		}
		actual[i] = identifier.Value
	}
	expected := append([]string{}, domains...)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) || !certificateACMEURL(client, order.FinalizeURL) || (order.CertURL != "" && !certificateACMEURL(client, order.CertURL)) {
		return errors.New("certificate authority changed the requested DNS names or endpoints")
	}
	for _, endpoint := range order.AuthzURLs {
		if !certificateACMEURL(client, endpoint) {
			return errors.New("certificate authorization endpoint is outside the configured authority")
		}
	}
	return nil
}

type certificateDNSAuthorization struct {
	URI          string `json:"uri"`
	ChallengeURI string `json:"challenge_uri"`
	Token        string `json:"token"`
	TXTName      string `json:"txt_name"`
	TXTValue     string `json:"txt_value"`
}

func readCertificateDNSAuthorization(ctx context.Context, client *acme.Client, endpoint string, domains []string) (*certificateDNSAuthorization, error) {
	if !certificateACMEURL(client, endpoint) {
		return nil, errors.New("invalid saved certificate authorization")
	}
	authorization, err := client.GetAuthorization(ctx, endpoint)
	if err != nil {
		return nil, errors.New("certificate authority authorization is unavailable")
	}
	domain := authorization.Identifier.Value
	if authorization.Wildcard {
		domain = "*." + domain
	}
	if authorization.Identifier.Type != "dns" || !slices.Contains(domains, domain) {
		return nil, errors.New("certificate authority requested an unapproved hostname")
	}
	if authorization.Status == acme.StatusValid {
		return nil, nil
	}
	if authorization.Status != acme.StatusPending {
		return nil, errors.New("certificate authority rejected or expired the hostname authorization")
	}
	for _, challenge := range authorization.Challenges {
		if challenge == nil || challenge.Type != "dns-01" {
			continue
		}
		if !certificateACMEURL(client, challenge.URI) || len(challenge.Token) == 0 || len(challenge.Token) > 512 {
			return nil, errors.New("certificate authority returned an invalid DNS challenge")
		}
		value, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return nil, errors.New("could not prepare the DNS challenge")
		}
		return &certificateDNSAuthorization{URI: endpoint, ChallengeURI: challenge.URI, Token: challenge.Token, TXTName: "_acme-challenge." + authorization.Identifier.Value, TXTValue: value}, nil
	}
	return nil, errors.New("certificate authority did not offer DNS-01 validation")
}

func finalizeCertificateOrder(ctx context.Context, client *acme.Client, endpoint string, domains []string, material worker.RouteCertificate, csrDER []byte) (worker.RouteCertificate, error) {
	if !certificateACMEURL(client, endpoint) {
		return material, errors.New("invalid saved certificate order")
	}
	order, err := client.GetOrder(ctx, endpoint)
	if err != nil {
		return material, errors.New("certificate authority order is unavailable")
	}
	if err := validateCertificateOrder(client, order, domains); err != nil {
		return material, err
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil || len(csr.IPAddresses) != 0 || len(csr.EmailAddresses) != 0 || len(csr.URIs) != 0 {
		return material, errors.New("saved certificate request is invalid")
	}
	expected := append([]string{}, domains...)
	actual := append([]string{}, csr.DNSNames...)
	slices.Sort(expected)
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		return material, errors.New("saved certificate request does not match the order's DNS names")
	}
	var chain [][]byte
	switch order.Status {
	case acme.StatusValid:
		chain, err = client.FetchCert(ctx, order.CertURL, true)
	case acme.StatusReady:
		chain, _, err = client.CreateCertFromOrder(ctx, order, csrDER, true)
	default:
		return material, errCertificateACMEPending
	}
	if err != nil {
		return material, errors.New("certificate finalization outcome is unknown; reconcile the saved order before retrying")
	}
	if len(chain) == 0 || len(chain) > 16 {
		return material, errors.New("certificate authority returned an invalid chain")
	}
	var encoded strings.Builder
	for _, der := range chain {
		encoded.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		if encoded.Len() > 65536 {
			return material, errors.New("issued certificate chain exceeds 64 KiB")
		}
	}
	material.CertificatePEM = encoded.String()
	if _, err := validateIssuedRouteCertificate(material, csrDER, time.Now()); err != nil {
		return material, errors.New("issued certificate does not match the saved request")
	}
	return material, nil
}

func certificateAuthorityOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (t certificateACMETransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Fragment != "" || certificateAuthorityOrigin(request.URL) != t.Origin {
		return nil, errors.New("ACME endpoint is outside the configured certificate authority")
	}
	response, err := t.Base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(response.Body, 2*1024*1024), response.Body}
	return response, nil
}

func newCertificateAccountKey() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})), nil
}

func newCertificateACMEClient(directory, rootPEM, accountPEM, accountURI string, wrap ...func(*http.Transport) http.RoundTripper) (*acme.Client, error) {
	u, err := url.Parse(directory)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || len(directory) > 2048 {
		return nil, errors.New("certificate authority must be an HTTPS URL without credentials, query or fragment")
	}
	if len(accountPEM) > 16384 {
		return nil, errors.New("certificate account key is invalid")
	}
	block, rest := pem.Decode([]byte(accountPEM))
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("certificate account key is invalid")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("certificate account key is invalid")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("certificate account requires an ECDSA P-256 key")
	}
	origin := certificateAuthorityOrigin(u)
	if accountURI != "" {
		account, err := url.Parse(accountURI)
		if err != nil || account.Scheme != "https" || account.User != nil || account.Fragment != "" || certificateAuthorityOrigin(account) != origin {
			return nil, errors.New("saved certificate account belongs to a different authority")
		}
	}
	var roots *x509.CertPool
	if rootPEM != "" {
		if len(rootPEM) > 256*1024 {
			return nil, errors.New("certificate authority trust bundle exceeds 256 KiB")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(rootPEM)) {
			return nil, errors.New("certificate authority trust bundle is invalid")
		}
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 * 1024, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8, ForceAttemptHTTP2: true}
	var base http.RoundTripper = transport
	if len(wrap) > 0 {
		base = wrap[0](transport)
	}
	client := &http.Client{Transport: certificateACMETransport{Origin: origin, Base: base}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &acme.Client{Key: key, KID: acme.KeyID(accountURI), DirectoryURL: directory, HTTPClient: client}, nil
}

func certificateACMETrust() (string, error) {
	path := os.Getenv("COVE_ACME_ROOT")
	if path == "" {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("configured certificate authority trust bundle is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return "", errors.New("configured certificate authority trust bundle is invalid or exceeds 256 KiB")
	}
	return string(data), nil
}

func configuredCertificateAuthority() string {
	if directory := os.Getenv("COVE_ACME_DIRECTORY"); directory != "" {
		return directory
	}
	return "https://acme-v02.api.letsencrypt.org/directory"
}

func certificateTerms(uri string) (string, error) {
	if uri == "" {
		return "", nil
	}
	for _, prefix := range []string{"data:text/plain,", "data:text/plain;charset=utf-8,"} {
		if !strings.HasPrefix(uri, prefix) {
			continue
		}
		text, err := url.PathUnescape(strings.TrimPrefix(uri, prefix))
		if err != nil || len(uri) > 16384 || !utf8.ValidString(text) || strings.TrimSpace(text) == "" || strings.ContainsFunc(text, func(r rune) bool { return r == 127 || r < 32 && r != '\n' && r != '\r' && r != '\t' }) {
			return "", errors.New("certificate authority returned invalid plain-text terms")
		}
		return text, nil
	}
	terms, err := url.Parse(uri)
	if err != nil || terms.Scheme != "https" || terms.Hostname() == "" || terms.User != nil || len(uri) > 2048 {
		return "", errors.New("certificate authority terms must be HTTPS or bounded plain text")
	}
	return "", nil
}

func (s *Server) discoverCertificateAuthority(ctx context.Context) (string, string, error) {
	directory := configuredCertificateAuthority()
	roots, err := certificateACMETrust()
	if err != nil {
		return "", "", err
	}
	key, err := newCertificateAccountKey()
	if err != nil {
		return "", "", err
	}
	client, err := newCertificateACMEClient(directory, roots, key, "", s.OutboundTransport)
	if err != nil {
		return "", "", err
	}
	defer client.HTTPClient.CloseIdleConnections()
	metadata, err := client.Discover(ctx)
	if err != nil {
		return "", "", errors.New("configured certificate authority is unavailable")
	}
	if _, err := certificateTerms(metadata.Terms); err != nil {
		return "", "", err
	}
	return directory, metadata.Terms, nil
}

func (s *Server) certificateAuthority(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	directory, terms, err := s.discoverCertificateAuthority(ctx)
	if err != nil {
		http.Error(w, "configured certificate authority is unavailable; review the panel's ACME directory and trust settings", 503)
		return
	}
	text, _ := certificateTerms(terms)
	respond(w, 200, map[string]string{"directory": directory, "terms_url": terms, "terms_text": text})
}

func (s *Server) certificateACMEAccount(ctx context.Context, directory, acceptedTerms string) (*acme.Client, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	var uri string
	err = tx.QueryRow(ctx, `SELECT private_key,account_uri FROM route_certificate_acme_accounts WHERE directory=$1 FOR UPDATE`, directory).Scan(&encrypted, &uri)
	if errors.Is(err, pgx.ErrNoRows) {
		key, err := newCertificateAccountKey()
		if err != nil {
			return nil, err
		}
		protected, err := s.sealJSON(key)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO route_certificate_acme_accounts(directory,private_key) VALUES($1,$2) ON CONFLICT DO NOTHING`, directory, protected); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		// A registration response can be lost; the committed account key recovers its identity.
		return s.certificateACMEAccount(ctx, directory, acceptedTerms)
	}
	if err != nil {
		return nil, err
	}
	data, err := unseal(s.Key, encrypted)
	var key string
	if err != nil || json.Unmarshal(data, &key) != nil {
		return nil, errors.New("stored certificate authority account is unreadable")
	}
	roots, err := certificateACMETrust()
	if err != nil {
		return nil, err
	}
	client, err := newCertificateACMEClient(directory, roots, key, uri, s.OutboundTransport)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*acme.Client, error) {
		client.HTTPClient.CloseIdleConnections()
		return nil, err
	}
	if uri == "" {
		account, err := client.GetReg(ctx, "")
		if errors.Is(err, acme.ErrNoAccount) {
			account, err = client.Register(ctx, &acme.Account{}, func(terms string) bool { return terms == acceptedTerms })
		}
		if err != nil || account == nil || account.Status != acme.StatusValid || !certificateACMEURL(client, account.URI) {
			return fail(errors.New("certificate authority account could not be confirmed; its saved key will be reconciled on retry"))
		}
		uri = account.URI
		if _, err := tx.Exec(ctx, `UPDATE route_certificate_acme_accounts SET account_uri=$2,updated_at=now() WHERE directory=$1`, directory, uri); err != nil {
			return fail(err)
		}
		client.KID = acme.KeyID(uri)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(err)
	}
	return client, nil
}
