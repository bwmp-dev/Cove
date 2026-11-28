package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

func TestCertificateACMEConnectedProviderTLS(t *testing.T) {
	providerURL := os.Getenv("COVE_TEST_CERTIFICATE_PROVIDER")
	if providerURL == "" {
		t.Skip("isolated verified-TLS DNS provider, validating Pebble and PostgreSQL required")
	}
	endpoint, err := url.Parse(providerURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() != "localhost" || endpoint.Port() == "" {
		t.Fatal("isolated loopback TLS provider required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, os.Getenv("COVE_TEST_CERTIFICATE_DNS_ADDRESS"))
	}}
	t.Cleanup(func() { net.DefaultResolver = previousResolver })
	t.Setenv("COVE_ACME_ROOT", os.Getenv("COVE_TEST_CERTIFICATE_ACME_ROOT"))
	t.Setenv("COVE_ACME_DIRECTORY", os.Getenv("COVE_TEST_CERTIFICATE_ACME_DIRECTORY"))
	root, err := os.ReadFile(os.Getenv("COVE_TEST_CERTIFICATE_ACME_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		t.Fatal("fixture TLS root required")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api.cloudflare.com:443" {
			return nil, errors.New("unexpected provider destination")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, endpoint.Host)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	f := newAccountFixture(t)
	f.s.CloudflareTransport = transport
	token := os.Getenv("COVE_TEST_CERTIFICATE_PROVIDER_TOKEN")
	connection := jsonString(t, f.call("root", "default", "POST", "/api/cloudflare/connections", map[string]string{"name": "Fixture DNS", "token": token}, 200), "id")
	zone := strings.Repeat("a", 32)
	f.call("root", "default", "POST", "/api/cloudflare/connections/"+connection+"/zones", map[string]any{"zones": []string{zone}}, 200)
	f.call("root", "default", "PUT", "/api/cloudflare/"+zone+"/shares", map[string]any{"shares": []map[string]any{{"team_id": "default", "domains": []string{"*.example.test"}}}}, 200)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('tls-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('tls-project','Project','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "DNS TLS", NodeID: "tls-node", ProjectID: "tls-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	base := "/api/workloads/" + resource
	var authority struct {
		Terms string `json:"terms_url"`
	}
	if json.Unmarshal(f.call("root", "default", "GET", base+"/certificate-authority", nil, 200).Body.Bytes(), &authority) != nil {
		t.Fatal("could not read authority consent metadata")
	}
	issuer := jsonString(t, f.call("root", "default", "POST", base+"/certificate-issuers", map[string]any{"name": "TLS DNS", "provider": "acme_dns", "zone_id": zone, "domains": []string{"*.full.example.test"}, "validity_days": 0, "auto_renew": false, "accept_terms": true, "terms_url": authority.Terms}, 202), "id")
	var phase string
	for deadline := time.Now().Add(40 * time.Second); time.Now().Before(deadline); {
		if err := f.s.processCertificateIssuer(ctx, issuer, false); err != nil {
			t.Fatal(err)
		}
		f.sql(`UPDATE route_certificate_dns_intents SET next_check_at=now() WHERE phase NOT IN ('cleaned','blocked')`)
		if err := f.s.scheduleCertificateDNSIntents(ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.s.DB.QueryRow(ctx, `SELECT phase FROM route_certificate_issuers WHERE id=$1`, issuer).Scan(&phase); err != nil {
			t.Fatal(err)
		}
		if phase == "issued" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if phase != "issued" {
		t.Fatal("connected TLS provider did not issue", phase)
	}
	f.call("root", "default", "DELETE", base+"/certificate-issuers/"+issuer, nil, 200)
	if err := f.s.scheduleCertificateDNSIntents(ctx); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, "GET", providerURL+"/fixture/state", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("could not inspect isolated provider cleanup", err)
	}
	defer response.Body.Close()
	var result struct {
		Result struct {
			Records int `json:"records"`
			Creates int `json:"creates"`
			Deletes int `json:"deletes"`
		} `json:"result"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&result) != nil || result.Result.Records != 0 || result.Result.Creates != 1 || result.Result.Deletes != 1 {
		t.Fatal("verified-TLS connected provider did not preserve exact create/cleanup ownership")
	}
}

func TestCertificateACMEClientPinsAuthorityAndBoundsProviderBodies(t *testing.T) {
	key, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"https://ca.example.test/directory?token=secret", "https://ca.example.test/directory?"} {
		if _, err := newCertificateACMEClient(directory, "", key, ""); err == nil {
			t.Fatal("authority metadata accepted a query")
		}
	}
	client, err := newCertificateACMEClient("https://ca.example.test/directory", "", key, "https://ca.example.test/account/1")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.HTTPClient.Transport.(certificateACMETransport)
	if !ok {
		t.Fatal("authority transport missing")
	}
	calls := 0
	transport.Base = dnsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 3*1024*1024)))}, nil
	})
	for _, target := range []string{"http://ca.example.test/nonce", "https://127.0.0.1/nonce", "https://ca.example.test:8443/nonce", "https://user:pass@ca.example.test/nonce", "https://ca.example.test/nonce#fragment"} {
		request, _ := http.NewRequestWithContext(context.Background(), "GET", target, nil)
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("provider escaped its configured authority", target)
		}
	}
	if calls != 0 {
		t.Fatal("untrusted provider URL reached transport")
	}
	request, _ := http.NewRequestWithContext(context.Background(), "GET", "https://ca.example.test:443/nonce", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || len(body) != 2*1024*1024 || calls != 1 {
		t.Fatal("provider response exceeded bound", len(body), err)
	}
	base := client.HTTPClient.Transport.(certificateACMETransport).Base.(*http.Transport)
	if base.TLSClientConfig == nil || base.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("certificate authority TLS verification disabled")
	}
	base.CloseIdleConnections()
}

func TestCertificateTermsAreSafeLinksOrPlainText(t *testing.T) {
	if text, err := certificateTerms("data:text/plain,Terms%20%3Cscript%3Enot%20markup%3C/script%3E"); err != nil || text != "Terms <script>not markup</script>" {
		t.Fatal("plain terms must stay text", text, err)
	}
	if text, err := certificateTerms("https://ca.example.test/terms.pdf"); err != nil || text != "" {
		t.Fatal("HTTPS terms link rejected", err)
	}
	for _, uri := range []string{"javascript:alert(1)", "data:text/html,<script>bad</script>", "data:text/plain,%00", "data:text/plain,", "http://ca.example.test/terms", "https://user:pass@ca.example.test/terms", "data:text/plain," + strings.Repeat("x", 16385)} {
		if _, err := certificateTerms(uri); err == nil {
			t.Fatal("unsafe terms accepted", uri[:min(60, len(uri))])
		}
	}
}

func TestCertificateACMEDNS01Pebble(t *testing.T) {
	directory := os.Getenv("COVE_TEST_CERTIFICATE_ACME_DIRECTORY")
	if directory == "" {
		t.Skip("isolated Pebble DNS-01 fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rootPEM, err := os.ReadFile(os.Getenv("COVE_TEST_CERTIFICATE_ACME_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	accountKey, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newCertificateACMEClient(directory, string(rootPEM), accountKey, "")
	if err != nil {
		t.Fatal(err)
	}
	account, err := registerCertificateAccount(ctx, client, true)
	if err != nil {
		t.Fatal(err)
	}
	client, err = newCertificateACMEClient(directory, string(rootPEM), accountKey, "")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := registerCertificateAccount(ctx, client, false)
	if err != nil || recovered != account {
		t.Fatal("saved account key was not recovered", err)
	}
	domains := []string{"*.wild.example.test"}
	csr, key, err := worker.NewRouteCertificateKey(domains)
	if err != nil {
		t.Fatal(err)
	}
	order, err := client.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: domains[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCertificateOrder(client, order, domains); err != nil {
		t.Fatal(err)
	}
	management := &http.Client{Timeout: 5 * time.Second}
	postDNS := func(path string, value any) {
		t.Helper()
		body, _ := json.Marshal(value)
		request, _ := http.NewRequestWithContext(ctx, "POST", os.Getenv("COVE_TEST_CERTIFICATE_DNS_MANAGEMENT")+path, bytes.NewReader(body))
		response, err := management.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("test DNS management rejected the fixture", response.StatusCode)
		}
	}
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6)}
	foreignID := strings.Repeat("f", 32)
	records := map[string]certificateTXT{foreignID: {ID: foreignID, Name: "_acme-challenge.wild.example.test", Content: "foreign-value", Comment: "not owned by Cove"}}
	updateDNS := func(name string) {
		postDNS("/clear-txt", map[string]string{"host": name + "."})
		for _, record := range records {
			if record.Name == name {
				postDNS("/set-txt", map[string]string{"host": name + ".", "value": record.Content})
			}
		}
	}
	updateDNS(records[foreignID].Name)
	creates := 0
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.cloudflare.com" || !strings.HasPrefix(r.URL.Path, "/client/v4/zones/"+zone.ZoneID+"/dns_records") {
			t.Fatal("unexpected Cloudflare destination")
		}
		var result any
		switch r.Method {
		case "GET":
			list := []map[string]string{}
			for _, record := range records {
				if record.Name == r.URL.Query().Get("name") {
					list = append(list, map[string]string{"id": record.ID, "type": "TXT", "name": record.Name, "content": record.Content, "comment": record.Comment})
				}
			}
			result = list
		case "POST":
			var record certificateTXT
			if json.NewDecoder(r.Body).Decode(&record) != nil || validateCertificateTXT(zone, record) != nil {
				t.Fatal("invalid DNS challenge sent to provider")
			}
			creates++
			record.ID = resourceID()
			records[record.ID] = record
			updateDNS(record.Name)
			return nil, errors.New("provider accepted before the response was lost")
		case "DELETE":
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			record, ok := records[id]
			if !ok || id == foreignID || !strings.HasPrefix(record.Comment, "cove-acme-") {
				t.Fatal("attempt to remove foreign DNS state")
			}
			delete(records, id)
			updateDNS(record.Name)
			result = map[string]string{"id": id}
		default:
			t.Fatal(r.Method)
		}
		body, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, os.Getenv("COVE_TEST_CERTIFICATE_DNS_ADDRESS"))
	}}
	for _, endpoint := range order.AuthzURLs {
		authorization, err := readCertificateDNSAuthorization(ctx, client, endpoint, domains)
		if err != nil || authorization == nil {
			t.Fatal("expected a real pending DNS-01 authorization", err)
		}
		intent := certificateTXT{Name: authorization.TXTName, Content: authorization.TXTValue, Comment: "cove-acme-" + resourceID()}
		if _, err := s.presentCertificateTXT(ctx, zone, intent, false); !errors.Is(err, errCertificateTXTUncertain) {
			t.Fatal("lost DNS response was not recorded as uncertain", err)
		}
		proof, err := s.presentCertificateTXT(ctx, zone, intent, true)
		if err != nil || proof == nil || creates != 1 {
			t.Fatal("DNS reconciliation duplicated creation", err)
		}
		values, err := resolver.LookupTXT(ctx, intent.Name)
		if err != nil || !slices.Contains(values, intent.Content) || !slices.Contains(values, "foreign-value") {
			t.Fatal("real DNS did not contain both owned and foreign values", err)
		}
		if _, err := client.Accept(ctx, &acme.Challenge{Type: "dns-01", URI: authorization.ChallengeURI, Token: authorization.Token}); err != nil {
			t.Fatal(err)
		}
		if validated, err := client.WaitAuthorization(ctx, endpoint); err != nil || validated.Status != acme.StatusValid {
			t.Fatal("Pebble rejected the actual DNS challenge", err)
		}
		if err := s.removeCertificateTXT(ctx, zone, *proof); err != nil {
			t.Fatal(err)
		}
		values, err = resolver.LookupTXT(ctx, intent.Name)
		if err != nil || !slices.Equal(values, []string{"foreign-value"}) {
			t.Fatal("cleanup changed foreign DNS state", err)
		}
	}
	if _, err := client.WaitOrder(ctx, order.URI); err != nil {
		t.Fatal(err)
	}
	transport := client.HTTPClient.Transport.(certificateACMETransport)
	base := transport.Base
	finalizations := 0
	nonceRefused := false
	upstream := dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.String() == order.FinalizeURL && !nonceRefused {
			nonceRefused = true
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/problem+json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"urn:ietf:params:acme:error:badNonce","status":400,"detail":"fixture nonce refused"}`))}, nil
		}
		return base.RoundTrip(r)
	})
	transport.Base = dnsTransport(func(r *http.Request) (*http.Response, error) {
		response, err := upstream.RoundTrip(r)
		if err != nil {
			t.Log("fixture authority transport failure", r.Method, r.URL.Path, err)
		} else if response.StatusCode >= 400 {
			t.Log("fixture authority status", r.Method, r.URL.Path, response.StatusCode)
		}
		if r.Method == "POST" && r.URL.String() == order.FinalizeURL && err == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			finalizations++
			response.Body.Close()
			return nil, errors.New("finalization response was lost")
		}
		return response, err
	})
	client.HTTPClient.Transport = transport
	material := worker.RouteCertificate{ID: resourceID(), PrivateKeyPEM: key}
	if _, err := finalizeCertificateOrder(ctx, client, order.URI, domains, material, csr); err == nil || finalizations != 1 || !nonceRefused {
		t.Fatal("expected one accepted finalization after the nonce refusal, with an unknown response", finalizations, err)
	}
	var issued worker.RouteCertificate
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		issued, err = finalizeCertificateOrder(ctx, client, order.URI, domains, material, csr)
		if err == nil {
			break
		}
		if !errors.Is(err, errCertificateACMEPending) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || finalizations != 1 {
		t.Fatal("order recovery repeated finalization", finalizations, err)
	}
	caRoots := x509.NewCertPool()
	caRoots.AppendCertsFromPEM(rootPEM)
	rootClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caRoots, MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	response, err := rootClient.Get(os.Getenv("COVE_TEST_CERTIFICATE_ISSUANCE_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	issuanceRoot, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal("could not fetch the fixture's public issuance root", err)
	}
	trust := x509.NewCertPool()
	if !trust.AppendCertsFromPEM(issuanceRoot) {
		t.Fatal("fixture issuance root is invalid")
	}
	pair, err := tls.X509KeyPair([]byte(issued.CertificatePEM), []byte(issued.PrivateKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("issued-wildcard")) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	application := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust, ServerName: "a.wild.example.test", MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	response, err = application.Get(server.URL)
	if err != nil {
		t.Fatal("issued wildcard failed real TLS chain/SNI verification", err)
	}
	response.Body.Close()
	if path := os.Getenv("COVE_TEST_CERTIFICATE_ACME_MATERIAL"); path != "" {
		encoded, err := json.Marshal(issued)
		if err != nil || os.WriteFile(path, encoded, 0600) != nil {
			t.Fatal("could not retain the isolated fixture's certificate material")
		}
	}
}

func TestCertificateACMEOrderKeepsWildcardScopeAndAuthority(t *testing.T) {
	client := &acme.Client{DirectoryURL: "https://ca.example.test/directory"}
	base := acme.Order{URI: "https://ca.example.test/order/1", Status: acme.StatusPending, Identifiers: []acme.AuthzID{{Type: "dns", Value: "*.example.test"}}, FinalizeURL: "https://ca.example.test/finalize/1", AuthzURLs: []string{"https://ca.example.test/authz/1"}}
	if err := validateCertificateOrder(client, &base, []string{"*.example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*acme.Order){
		func(o *acme.Order) { o.Identifiers = []acme.AuthzID{{Type: "dns", Value: "example.test"}} },
		func(o *acme.Order) {
			o.Identifiers = append(append([]acme.AuthzID{}, o.Identifiers...), acme.AuthzID{Type: "dns", Value: "example.test"})
		},
		func(o *acme.Order) { o.Identifiers = []acme.AuthzID{{Type: "ip", Value: "127.0.0.1"}} },
		func(o *acme.Order) { o.FinalizeURL = "https://169.254.169.254/finalize" },
		func(o *acme.Order) { o.CertURL = "https://other.example.test/certificate" },
		func(o *acme.Order) { o.AuthzURLs = []string{"https://user:pass@ca.example.test/authz"} },
		func(o *acme.Order) { o.Status = acme.StatusInvalid },
		func(o *acme.Order) { o.Status = acme.StatusValid },
		func(o *acme.Order) { o.Expires = time.Now().Add(-time.Second) },
	} {
		order := base
		change(&order)
		if validateCertificateOrder(client, &order, []string{"*.example.test"}) == nil {
			t.Fatal("authority or requested wildcard scope changed")
		}
	}
}

func TestCertificateACMEClientRejectsInvalidAccountAndTrustState(t *testing.T) {
	key, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ directory, root, key, account string }{
		{"http://ca.example.test/dir", "", key, ""},
		{"https://user:password@ca.example.test/dir", "", key, ""},
		{"https://ca.example.test/dir", "not a trust bundle", key, ""},
		{"https://ca.example.test/dir", "", key, "https://169.254.169.254/account"},
		{"https://ca.example.test/dir", "", "private-fixture-secret", ""},
	} {
		if _, err := newCertificateACMEClient(test.directory, test.root, test.key, test.account); err == nil || strings.Contains(err.Error(), "private-fixture-secret") {
			t.Fatal("invalid account state accepted or secret included in error", err)
		}
	}
}

func TestCertificateACMEAccountRecoversCommittedKeyPebble(t *testing.T) {
	directory := os.Getenv("COVE_TEST_CERTIFICATE_ACME_DIRECTORY")
	if directory == "" {
		t.Skip("isolated Pebble and PostgreSQL fixture required")
	}
	f := newAccountFixture(t)

	t.Setenv("COVE_ACME_ROOT", os.Getenv("COVE_TEST_CERTIFICATE_ACME_ROOT"))
	roots, err := certificateACMETrust()
	if err != nil {
		t.Fatal(err)
	}
	key, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newCertificateACMEClient(directory, roots, key, "")
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := client.Discover(context.Background())
	client.HTTPClient.CloseIdleConnections()
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.s.certificateACMEAccount(context.Background(), directory, discovery.Terms)
	if err != nil {
		t.Fatal(err)
	}
	identity := first.KID
	first.HTTPClient.CloseIdleConnections()
	if identity == "" {
		t.Fatal("registered account identity missing")
	}
	var protected []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT private_key FROM route_certificate_acme_accounts WHERE directory=$1`, directory).Scan(&protected); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(protected), "PRIVATE KEY") {
		t.Fatal("account key stored in plaintext")
	}
	f.sql(`UPDATE route_certificate_acme_accounts SET account_uri='' WHERE directory=$1`, directory)
	recovered, err := f.s.certificateACMEAccount(context.Background(), directory, discovery.Terms)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.HTTPClient.CloseIdleConnections()
	if recovered.KID != identity {
		t.Fatal("lost registration response replaced the committed account key")
	}
}

func registerCertificateAccount(ctx context.Context, client *acme.Client, acceptedTerms bool) (string, error) {
	account, err := client.GetReg(ctx, "")
	if errors.Is(err, acme.ErrNoAccount) {
		if !acceptedTerms {
			return "", errors.New("accept the certificate authority's terms before creating an account")
		}
		account, err = client.Register(ctx, &acme.Account{}, func(string) bool { return true })
	}
	if err != nil {
		return "", errors.New("certificate authority account is unavailable")
	}
	if account == nil || account.Status != acme.StatusValid || !certificateACMEURL(client, account.URI) {
		return "", errors.New("certificate authority returned an invalid account")
	}
	return account.URI, nil
}

func TestCertificateACMEControllerPublishesValidatesAndCancelsPebble(t *testing.T) {
	directory := os.Getenv("COVE_TEST_CERTIFICATE_ACME_DIRECTORY")
	if directory == "" {
		t.Skip("isolated validating Pebble and PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, os.Getenv("COVE_TEST_CERTIFICATE_DNS_ADDRESS"))
	}}
	t.Cleanup(func() { net.DefaultResolver = previousResolver })
	f := newAccountFixture(t)

	t.Setenv("COVE_ACME_ROOT", os.Getenv("COVE_TEST_CERTIFICATE_ACME_ROOT"))
	t.Setenv("COVE_ACME_DIRECTORY", directory)

	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('acme-node','Node','localhost',22,'',$1,'ready','{"route_protocol":4}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('acme-project','Project','default')`)
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "ACME", NodeID: "acme-node", ProjectID: "acme-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", Token: strings.Repeat("fixture", 6), Enabled: true}
	protected, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, protected)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['*.wild.example.test'])`, zone.ZoneID)
	roots, err := certificateACMETrust()
	if err != nil {
		t.Fatal(err)
	}
	key, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	metadataClient, err := newCertificateACMEClient(directory, roots, key, "")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := metadataClient.Discover(ctx)
	metadataClient.HTTPClient.CloseIdleConnections()
	if err != nil {
		t.Fatal(err)
	}
	foreign := certificateTXT{ID: strings.Repeat("f", 32), Name: "_acme-challenge.wild.example.test", Content: "foreign-controller-value", Comment: "unrelated-owner"}
	records := map[string]certificateTXT{foreign.ID: foreign}
	var mu sync.Mutex
	creates, deletes := 0, 0
	propagated := false
	refusals := 0
	updateDNS := func() {
		t.Helper()
		client := &http.Client{Timeout: 5 * time.Second}
		post := func(path string, body any) {
			t.Helper()
			raw, _ := json.Marshal(body)
			request, _ := http.NewRequestWithContext(ctx, "POST", os.Getenv("COVE_TEST_CERTIFICATE_DNS_MANAGEMENT")+path, bytes.NewReader(raw))
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("test DNS update refused", response.StatusCode)
			}
		}
		post("/clear-txt", map[string]string{"host": foreign.Name + "."})
		for _, record := range records {
			post("/set-txt", map[string]string{"host": record.Name + ".", "value": record.Content})
		}
	}
	updateDNS()
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host != "api.cloudflare.com" || !strings.HasPrefix(r.URL.Path, "/client/v4/zones/"+zone.ZoneID+"/dns_records") || r.Header.Get("Authorization") != "Bearer "+zone.Token {
			t.Fatal("unexpected DNS provider destination")
		}
		var result any
		switch r.Method {
		case "GET":
			list := []map[string]string{}
			for _, record := range records {
				list = append(list, map[string]string{"id": record.ID, "type": "TXT", "name": record.Name, "content": record.Content, "comment": record.Comment})
			}
			result = list
		case "POST":
			if refusals == 0 {
				refusals++
				return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("provider-sensitive-fixture " + zone.Token))}, nil
			}
			var record certificateTXT
			if json.NewDecoder(r.Body).Decode(&record) != nil || validateCertificateTXT(zone, record) != nil {
				t.Fatal("invalid owned challenge")
			}
			creates++
			record.ID = resourceID()
			records[record.ID] = record
			if propagated {
				updateDNS()
			}
			if creates == 1 {
				return nil, errors.New("DNS response lost after acceptance")
			}
			result = map[string]string{"id": record.ID, "type": "TXT", "name": record.Name, "content": record.Content, "comment": record.Comment}
		case "DELETE":
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if id == foreign.ID || !strings.HasPrefix(records[id].Comment, "cove-acme-") {
				t.Fatal("foreign DNS record deletion")
			}
			delete(records, id)
			deletes++
			updateDNS()
			result = map[string]string{"id": id}
		default:
			t.Fatal(r.Method)
		}
		raw, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	base := "/api/workloads/" + resource
	request := func() map[string]any {
		return map[string]any{"name": "DNS controller", "provider": "acme_dns", "zone_id": zone.ZoneID, "domains": []string{"*.wild.example.test"}, "validity_days": 0, "auto_renew": true, "accept_terms": true, "terms_url": authority.Terms}
	}
	invalid := request()
	invalid["accept_terms"] = false
	f.call("root", "default", "POST", base+"/certificate-issuers", invalid, 400)
	invalid = request()
	invalid["terms_url"] = "https://unrelated.example.test/terms"
	f.call("root", "default", "POST", base+"/certificate-issuers", invalid, 409)
	viewer := f.user("acme-viewer", "acme-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','acme-viewer','member','[{"scope":"project","scope_id":"acme-project","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", base+"/certificate-issuers", request(), 403)
	info := f.call(viewer, "default", "GET", base+"/certificate-authority", nil, 200).Body.String()
	if !strings.Contains(info, directory) || !strings.Contains(info, authority.Terms) {
		t.Fatal("authority metadata differs from accepted terms")
	}
	create := func() string {
		t.Helper()
		return jsonString(t, f.call("root", "default", "POST", base+"/certificate-issuers", request(), 202), "id")
	}

	controller := f.s
	step := func(id string) {
		t.Helper()
		if err := controller.processCertificateIssuer(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}

	dns := func() {
		t.Helper()
		f.sql(`UPDATE route_certificate_dns_intents SET next_check_at=now() WHERE phase NOT IN ('cleaned','blocked')`)
		if err := f.s.scheduleCertificateDNSIntents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	first := create()
	public := f.call(viewer, "default", "GET", base+"/certificate-issuers", nil, 200).Body.String()
	if !strings.Contains(public, directory) || !strings.Contains(public, "terms_accepted_at") || strings.Contains(public, "PRIVATE KEY") || strings.Contains(public, "order_uri") || strings.Contains(public, "private_key_pem") || strings.Contains(public, zone.Token) {
		t.Fatal("authority metadata leaked private request state")
	}
	step(first)
	var phase, certificate string
	retried := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		step(first)
		dns()
		if err := f.s.DB.QueryRow(ctx, `SELECT phase,COALESCE(certificate_id,'') FROM route_certificate_issuers WHERE id=$1`, first).Scan(&phase, &certificate); err != nil {
			t.Fatal(err)
		}
		if phase == "failed" && !retried {
			var blocked bool
			if err := f.s.DB.QueryRow(ctx, `SELECT next_check_at IS NULL AND EXISTS(SELECT 1 FROM route_certificate_dns_intents WHERE issuer_id=$1 AND phase='blocked' AND next_check_at IS NULL) FROM route_certificate_issuers WHERE id=$1`, first).Scan(&blocked); err != nil || !blocked || creates != 0 {
				t.Fatal("provider refusal did not pause before explicit retry", blocked, creates, err)
			}
			dns()
			f.call("root", "default", "PUT", base+"/certificate-issuers/"+first, map[string]bool{"retry": true}, 200)
			retried = true
		}
		if !propagated {
			var message string
			if err := f.s.DB.QueryRow(ctx, `SELECT last_error FROM route_certificate_issuers WHERE id=$1`, first).Scan(&message); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(message, "DNS propagation") {
				if phase != "requesting" || certificate != "" || creates != 1 {
					t.Fatal("validation did not wait for actual DNS visibility", phase, creates)
				}
				propagated = true
				updateDNS()
			}
		}
		if phase == "issued" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if phase != "issued" || certificate == "" || creates != 1 || !propagated || !retried || refusals != 1 {
		t.Fatal("durable DNS controller did not issue without duplicating the lost create", phase, creates)
	}
	dns()
	material, _, err := f.s.routeCertificate(ctx, f.s.DB, resource, certificate)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.CertificateCoversRoute(material, "a.wild.example.test"); err != nil {
		t.Fatal(err)
	}
	list := f.call("root", "default", "GET", base+"/certificates", nil, 200).Body.String()
	if !strings.Contains(list, "acme_dns") || strings.Contains(list, "PRIVATE KEY") || strings.Contains(list, zone.Token) {
		t.Fatal("issued metadata leaked or lost provider identity")
	}
	deactivateAuthorization := func() {
		t.Helper()
		var encrypted []byte
		if err := f.s.DB.QueryRow(ctx, `SELECT acme_state FROM route_certificate_issuers WHERE id=$1`, first).Scan(&encrypted); err != nil {
			t.Fatal(err)
		}
		raw, err := unseal(f.s.Key, encrypted)
		var pending certificateACMEIssuerState
		if err != nil || json.Unmarshal(raw, &pending) != nil {
			t.Fatal("could not read fixture order identity")
		}
		client, err := f.s.certificateACMEAccount(ctx, pending.Directory, pending.TermsURL)
		if err != nil {
			t.Fatal(err)
		}
		defer client.HTTPClient.CloseIdleConnections()
		order, err := client.GetOrder(ctx, pending.OrderURI)
		if err != nil || len(order.AuthzURLs) == 0 {
			t.Fatal("fixture order has no authorizations", err)
		}
		// Revoke only fixture authorizations so CA reuse cannot skip the DNS behavior under test.
		for _, endpoint := range order.AuthzURLs {
			if err := client.RevokeAuthorization(ctx, endpoint); err != nil {
				t.Fatal(err)
			}
		}
	}
	issuedCount := 1
	if os.Getenv("COVE_TEST_CERTIFICATE_ACME_RENEWAL") == "1" {
		deactivateAuthorization()
		restoreCertificateControllerDatabase(t, f)
		info, err := worker.ParseRouteCertificate(material, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		wait := time.Until(certificateRenewalAt(info))
		if wait > 30*time.Second {
			t.Fatal("renewal fixture requires a short-lived certificate profile", wait)
		}
		if wait > 0 {
			time.Sleep(wait + 50*time.Millisecond)
		}
		controller = &Server{DB: f.s.DB, Key: f.s.Key, AdminToken: f.s.AdminToken, PublicURL: f.s.PublicURL, CloudflareTransport: f.s.CloudflareTransport}
		if err := controller.Init(ctx); err != nil {
			t.Fatal(err)
		}
		previous := certificate
		step(first)
		var checkpoint time.Time
		if err := f.s.DB.QueryRow(ctx, `SELECT next_check_at FROM route_certificate_issuers WHERE id=$1`, first).Scan(&checkpoint); err != nil || !checkpoint.Before(info.ExpiresAt) {
			t.Fatal("normal renewal checkpoint outlasts the currently served certificate", err)
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			step(first)
			dns()
			if err := f.s.DB.QueryRow(ctx, `SELECT phase,COALESCE(certificate_id,'') FROM route_certificate_issuers WHERE id=$1`, first).Scan(&phase, &certificate); err != nil {
				t.Fatal(err)
			}
			if phase == "issued" && certificate != previous {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if phase != "issued" || certificate == previous || creates != 2 {
			t.Fatal("a restarted controller did not renew through a new owned challenge", phase, creates)
		}
		renewed, _, err := f.s.routeCertificate(ctx, f.s.DB, resource, certificate)
		if err != nil || renewed.PrivateKeyPEM == material.PrivateKeyPEM || worker.CertificateCoversRoute(renewed, "a.wild.example.test") != nil {
			t.Fatal("renewal did not rotate the key and retain wildcard coverage", err)
		}
		retained, _, err := f.s.routeCertificate(ctx, f.s.DB, resource, previous)
		if err != nil || retained != material {
			t.Fatal("renewal overwrote the previous immutable certificate", err)
		}
		if os.Getenv("COVE_TEST_CERTIFICATE_TRAEFIK") == "1" {
			testCertificateACMETraefik(t, renewed)
		}
		issuedCount++
		dns()
	}
	deactivateAuthorization()
	f.call("root", "default", "DELETE", base+"/certificate-issuers/"+first, nil, 200)
	second := create()
	for i := 0; i < certificateDNSIntentLimit; i++ {
		f.sql(`INSERT INTO route_certificate_dns_intents(id,zone_id,cycle_id,state,phase) VALUES($1,$2,'limit-fixture',$3,'cleanup')`, resourceID(), zone.ZoneID, []byte{})
	}
	step(second)
	step(second)
	if err := f.s.DB.QueryRow(ctx, `SELECT phase FROM route_certificate_issuers WHERE id=$1`, second).Scan(&phase); err != nil || phase != "failed" || creates != issuedCount {
		t.Fatal("unresolved challenge limit admitted another provider mutation", phase, creates, err)
	}
	f.sql(`DELETE FROM route_certificate_dns_intents WHERE cycle_id='limit-fixture'`)
	f.call("root", "default", "PUT", base+"/certificate-issuers/"+second, map[string]bool{"retry": true}, 200)
	step(second)
	dns()
	if creates != issuedCount+1 {
		t.Fatal("second issuer did not create its own challenge", creates)
	}
	f.call("root", "default", "DELETE", base+"/certificate-issuers/"+second, nil, 200)
	dns()
	dns()
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM route_certificates WHERE workload_id=$1`, resource).Scan(&count); err != nil || count != issuedCount {
		t.Fatal("cancelled request issued a certificate", count, err)
	}
	if len(records) != 1 || records[foreign.ID] != foreign || deletes != issuedCount+1 {
		t.Fatal("owned DNS cleanup did not preserve foreign TXT", len(records), deletes)
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, os.Getenv("COVE_TEST_CERTIFICATE_DNS_ADDRESS"))
	}}
	values, err := resolver.LookupTXT(ctx, foreign.Name)
	if err != nil || !slices.Equal(values, []string{foreign.Content}) {
		t.Fatal("real DNS retained an owned challenge or lost the foreign value", err)
	}
}

func testCertificateACMETraefik(t *testing.T, material worker.RouteCertificate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	directory := t.TempDir()
	for name, value := range map[string]string{"certificate.pem": material.CertificatePEM, "key.pem": material.PrivateKeyPEM} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
			t.Fatal("could not write private fixture certificate")
		}
	}
	config := `{"tls":{"certificates":[{"certFile":"/fixture/certificate.pem","keyFile":"/fixture/key.pem"}]},"http":{"routers":{"issued":{"rule":"Host(` + "`a.wild.example.test`" + `)","entryPoints":["websecure"],"tls":{},"service":"noop@internal"}}}}`
	if err := os.WriteFile(filepath.Join(directory, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	owner := resourceID()
	name := "cove-acme-traefik-" + owner
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		label, err := exec.CommandContext(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.test.owner"}}`, name).Output()
		if err != nil || strings.TrimSpace(string(label)) != owner {
			t.Error("could not confirm ownership for certificate fixture cleanup")
			return
		}
		if err := exec.CommandContext(cleanup, "docker", "rm", "-f", "-v", name).Run(); err != nil {
			t.Error("could not remove owned certificate fixture")
		}
	})
	if err := exec.CommandContext(ctx, "docker", "run", "-d", "--pull=never", "--name", name, "--label", "cove.test=true", "--label", "cove.test.owner="+owner, "--label", "cove.browser-fixture="+os.Getenv("COVE_TEST_CERTIFICATE_FIXTURE"), "-p", "127.0.0.1::8443", "--mount", "type=bind,source="+directory+",target=/fixture,readonly", "traefik:v3.7.19", "--entrypoints.websecure.address=:8443", "--providers.file.filename=/fixture/config.yaml", "--log.level=ERROR").Run(); err != nil {
		t.Fatal("could not start the isolated pinned Traefik fixture", err)
	}
	port, err := exec.CommandContext(ctx, "docker", "port", name, "8443/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, err := certificateACMETrust()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(rootPEM)) {
		t.Fatal("invalid fixture management root")
	}
	management := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	response, err := management.Get(os.Getenv("COVE_TEST_CERTIFICATE_ISSUANCE_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	response.Body.Close()
	management.CloseIdleConnections()
	trust := x509.NewCertPool()
	if err != nil || response.StatusCode != 200 || !trust.AppendCertsFromPEM(root) {
		t.Fatal("could not retrieve the fixture issuance root")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust, ServerName: "a.wild.example.test", MinVersion: tls.VersionTLS12}}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	address := "https://" + strings.TrimSpace(string(port)) + "/"
	for ctx.Err() == nil {
		request, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
		request.Host = "a.wild.example.test"
		response, err = client.Do(request)
		if err == nil {
			response.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || response == nil || response.StatusCode != 418 || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		t.Fatal("renewed wildcard did not serve through verified TLS on pinned Traefik", err)
	}
	pair, err := tls.X509KeyPair([]byte(material.CertificatePEM), []byte(material.PrivateKeyPEM))
	if err != nil || !bytes.Equal(response.TLS.PeerCertificates[0].Raw, pair.Certificate[0]) {
		t.Fatal("proxy did not serve the renewed immutable certificate")
	}
}

func restoreCertificateControllerDatabase(t *testing.T, f *accountFixture) {
	t.Helper()
	container := os.Getenv("COVE_TEST_DATABASE_CONTAINER")
	if !regexp.MustCompile(`^cove-certificates-[a-f0-9]+-db$`).MatchString(container) {
		t.Fatal("owned certificate PostgreSQL container required for restore acceptance")
	}
	original := f.s.DB
	config := original.Config().Copy()
	schema := config.ConnConfig.RuntimeParams["search_path"]
	if !regexp.MustCompile(`^cove_test_[a-f0-9]{32}$`).MatchString(schema) {
		t.Fatal("unexpected owned schema identity")
	}
	fingerprints := func(pool *pgxpool.Pool) []string {
		t.Helper()
		values := []string{}
		for _, table := range []string{"route_certificates", "route_certificate_issuers", "route_certificate_acme_accounts", "route_certificate_dns_intents"} {
			var digest string
			if err := pool.QueryRow(t.Context(), "SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY row_to_json(t)::text),'')) FROM "+pgx.Identifier{table}.Sanitize()+" t").Scan(&digest); err != nil {
				t.Fatal(err)
			}
			values = append(values, digest)
		}
		return values
	}
	expected := fingerprints(original)
	dump, err := exec.CommandContext(t.Context(), "docker", "exec", container, "pg_dump", "-U", "postgres", "-d", config.ConnConfig.Database, "-n", schema, "-Fc").Output()
	if err != nil {
		t.Fatal("owned certificate schema backup failed", err)
	}
	database := "cove_cert_restore_" + resourceID()
	if _, err = original.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	var restored *pgxpool.Pool
	t.Cleanup(func() {
		if restored != nil {
			restored.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := original.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
			t.Error("owned certificate restore database cleanup failed", err)
		}
	})
	command := exec.CommandContext(t.Context(), "docker", "exec", "-i", container, "pg_restore", "-U", "postgres", "-d", database, "--exit-on-error")
	command.Stdin = bytes.NewReader(dump)
	if err = command.Run(); err != nil {
		t.Fatal("owned certificate schema restore failed", err)
	}
	config.ConnConfig.Database = database
	restored, err = pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(expected, fingerprints(restored)) {
		t.Fatal("certificate, issuer, CA account or DNS ownership state changed during database restore")
	}
	f.s.DB = restored
	if err = f.s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(expected, fingerprints(restored)) {
		t.Fatal("schema upgrade changed restored certificate ownership state")
	}
	t.Log("restored sealed certificate, CA account and DNS intent tables before real renewal")
}
