package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudflareSystemTrustCopiesOnlyPublicCertificates(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "origin.example.test")
	public, err := cloudflarePublicCABundle("operator bundle\n" + string(certificate))
	if err != nil || public != string(certificate) {
		t.Fatal("public trust was not preserved", err)
	}
	for _, invalid := range []string{string(certificate) + string(key), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")})), strings.Repeat("x", 1024*1024+1)} {
		if public, err := cloudflarePublicCABundle(invalid); err == nil || public != "" {
			t.Fatal("invalid or private trust material was accepted")
		}
	}
}

type cloudflarePullTransport func(*http.Request) (*http.Response, error)

func (f cloudflarePullTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCloudflareConnectorPullIsAnonymousAndBounded(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"complete", "{\"status\":\"Pulling\"}\n{\"status\":\"Digest verified\"}\n", 200, true},
		{"daemon refusal", "registry credential detail", 403, false},
		{"pull error", "{\"error\":\"private registry detail\"}\n", 200, false},
		{"invalid progress", "not-json", 200, false},
		{"oversized progress", strings.Repeat(" ", 8*1024*1024+1), 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: cloudflarePullTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != "POST" || request.URL.Host != "docker" || request.URL.Path != "/images/create" || request.URL.Query().Get("fromImage") != DefaultCloudflaredImage || len(request.Header) != 0 || request.Body != nil {
					t.Fatal("pull changed the daemon target or included credentials")
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: http.Header{}}, nil
			})}
			err := pullCloudflareConnectorImage(context.Background(), client, DefaultCloudflaredImage)
			if (err == nil) != test.ok {
				t.Fatal("unexpected pull outcome", err)
			}
			if err != nil && strings.Contains(err.Error(), "detail") {
				t.Fatal("daemon details leaked")
			}
		})
	}
}

func TestCloudflareGenerationReversionRetiresOnlyTheFailedCandidate(t *testing.T) {
	a := cloudflareGeneration{Revision: strings.Repeat("a", 64)}
	b := cloudflareGeneration{Revision: strings.Repeat("b", 64)}
	c := cloudflareGeneration{Revision: strings.Repeat("c", 64)}
	state := cloudflarePublishingState{Active: &a, Candidate: &b}
	selectCloudflareGeneration(&state, a.Revision)
	if state.Candidate != nil || state.Active != &a || len(state.Retired) != 1 || state.Retired[0].Revision != b.Revision {
		t.Fatal("returning to active left the failed candidate outside cleanup")
	}
	state = cloudflarePublishingState{Active: &a, Candidate: &c, Retired: []cloudflareGeneration{b}}
	selectCloudflareGeneration(&state, b.Revision)
	for _, g := range state.Retired {
		if g.Revision == b.Revision || g.Revision == a.Revision {
			t.Fatal("desired or active revision would be deleted")
		}
	}
	if state.Candidate != nil || len(state.Retired) != 1 || state.Retired[0].Revision != c.Revision {
		t.Fatal("failed candidate ownership was lost")
	}
}

func TestCloudflareDeploymentStatusNeverContainsConnectorCredentials(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	state := cloudflarePublishingState{Desired: &CloudflarePublishing{ID: strings.Repeat("a", 32), TunnelID: "11111111-2222-3333-4444-555555555555", AccountID: strings.Repeat("b", 32), TunnelSecret: secret, Hostnames: []string{"app.example.test"}}, Phase: "connecting"}
	data, err := json.Marshal(cloudflareDeploymentStatus(state))
	if err != nil || bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte("tunnel_secret")) || bytes.Contains(data, []byte("account_id")) {
		t.Fatal("private provider state leaked through status", err)
	}
	if !bytes.Contains(data, []byte(DefaultCloudflaredImage)) {
		t.Fatal("connector pin is not visible")
	}
}

func TestCloudflareOriginTrustDoesNotGrowAcrossRepeatedReconciliation(t *testing.T) {
	first, _ := selfSignedCertificate(t, "app.example.test")
	second, _ := selfSignedCertificate(t, "app.example.test")
	merged := string(first)
	for range 100 {
		var err error
		merged, err = cloudflareTrustUnion(string(second), merged)
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(merged, "BEGIN CERTIFICATE") != 2 {
		t.Fatal("repeated polling expanded the origin trust set")
	}
	if _, err := cloudflareTrustUnion("invalid", merged); err == nil {
		t.Fatal("malformed trust accepted")
	}
}

func TestCloudflareRoutingInventoryKeepsTheDowngradeHoldUntilDisabled(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value := &CloudflarePublishing{ID: strings.Repeat("a", 32), TunnelID: "11111111-2222-3333-4444-555555555555", AccountID: strings.Repeat("b", 32), TunnelSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)), Hostnames: []string{"app.example.test"}}
	state := cloudflarePublishingState{Desired: value, Phase: "connecting"}
	if err := e.writeCloudflarePublishing(state); err != nil {
		t.Fatal(err)
	}
	routes := map[string][]Route{"app": {{Domain: "app.example.test", Service: "app", Port: 8080}}}
	if err := e.writeManagedIngress(routes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress.json"))
	if err != nil || !bytes.Contains(data, []byte("cove-protected-tunnel-v1")) {
		t.Fatal("older workers were not held", err)
	}
	loaded, err := e.loadIngress()
	if err != nil || loaded["app"][0].Domain != "app.example.test" || loaded["app"][0].Policy.CertificateSource != "" {
		t.Fatal("current worker cannot recover protected routes", err)
	}
	state.Desired = nil
	state.Phase = "disabled"
	if err := e.writeCloudflarePublishing(state); err != nil {
		t.Fatal(err)
	}
	if err := e.writeManagedIngress(routes); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(e.Root, "ingress.json"))
	if err != nil || bytes.Contains(data, []byte("cove-protected-tunnel-v1")) {
		t.Fatal("confirmed disable did not release the hold", err)
	}
}
