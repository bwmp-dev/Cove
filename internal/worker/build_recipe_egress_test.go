package worker

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func testRecipeEgress(t *testing.T) *recipeEgress {
	t.Helper()
	ca := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(ca.Close)
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate().Raw})
	p, err := newRecipeEgress(recipeEgressConfig{Roots: string(roots)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.transport.CloseIdleConnections)
	return p
}

func TestBuildRecipeEgressRejectsPrivateAndMetadata(t *testing.T) {
	p := testRecipeEgress(t)
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "172.18.0.1", "192.168.0.2", "169.254.169.254", "168.63.129.16", "100.100.100.200", "::1", "fe80::1", "fd00::1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "192.0.2.1"} {
		t.Run(address, func(t *testing.T) {
			ip := netip.MustParseAddr(address)
			p.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{ip}, nil }
			request := httptest.NewRequest("GET", "http://metadata.invalid/fixture", nil)
			response := httptest.NewRecorder()
			p.ServeHTTP(response, request)
			if response.Code != 403 {
				t.Fatalf("private target accepted: %d", response.Code)
			}
		})
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if err := p.checkTarget(context.Background(), &url.URL{Scheme: "https", Host: "mixed.invalid"}); err == nil {
		t.Fatal("mixed public/private DNS accepted")
	}
}

func TestBuildRecipeEgressPinsResolvedAddress(t *testing.T) {
	p := testRecipeEgress(t)
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	var address string
	p.dial = func(_ context.Context, _, target string) (net.Conn, error) {
		address = target
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	connection, err := p.safeDial(context.Background(), "tcp", "public.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if address != "1.1.1.1:443" {
		t.Fatal("DNS was delegated to dial", address)
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err := p.safeDial(context.Background(), "tcp", "public.invalid:443"); err == nil {
		t.Fatal("DNS rebinding accepted")
	}
}

func TestBuildRecipeEgressUsesConfiguredProxyOnly(t *testing.T) {
	p := testRecipeEgress(t)
	var received bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		if r.URL.Host != "metadata.fixture.invalid" || r.Header.Get("Proxy-Authorization") != "Basic Zml4dHVyZTpub3QtYS1jcmVkZW50aWFs" {
			t.Errorf("proxy route/auth missing")
		}
		_, _ = io.WriteString(w, "fixture metadata")
	}))
	defer upstream.Close()
	p.config.HTTPProxy = strings.Replace(upstream.URL, "http://", "http://fixture:not-a-credential@", 1)
	p.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "metadata.fixture.invalid" {
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("172.18.0.2")}, nil
	}
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !strings.HasPrefix(address, "172.18.0.2:") {
			t.Errorf("bypassed configured proxy: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://metadata.fixture.invalid/metadata", nil)
	request.Header.Set("Proxy-Authorization", "fixture-must-not-be-forwarded")
	p.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "fixture metadata" || !received {
		t.Fatalf("proxy failed: %d %q", response.Code, response.Body.String())
	}
	p.config.HTTPProxy = "http://127.0.0.1:2375"
	if _, err := newRecipeEgress(p.config); err == nil {
		t.Fatal("Docker/host proxy accepted")
	}
}

func TestBuildRecipeEgressBoundsAndNoProxy(t *testing.T) {
	p := testRecipeEgress(t)
	p.remaining = 3
	if err := p.copy(io.Discard, strings.NewReader("four")); err == nil {
		t.Fatal("transfer bound ignored")
	}
	for _, policy := range []string{"*", "example.test", ".example.test", "*.example.test", "host.example.test:443", "192.0.2.0/24"} {
		host := "host.example.test"
		if strings.Contains(policy, "/24") {
			host = "192.0.2.2"
		}
		if !recipeNoProxy(host, "443", policy) {
			t.Fatalf("NO_PROXY lost: %s", policy)
		}
	}
	if recipeNoProxy("notexample.test", "443", "example.test") || recipeNoProxy("host.example.test", "80", "host.example.test:443") {
		t.Fatal("NO_PROXY broadened")
	}
}
