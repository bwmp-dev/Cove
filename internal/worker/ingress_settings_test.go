package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIngressSettingsPreserveMigratedRouting(t *testing.T) {
	e := &Engine{Root: t.TempDir(), ACMEDirectory: "https://acme.example.test/directory", ACMERoot: "fixture.pem"}
	priority := 1
	if err := writeJSON(filepath.Join(e.Root, "ingress-settings.json"), ingressSettings{CloudflareDNS: true, HTTPRedirectPriority: &priority}); err != nil {
		t.Fatal(err)
	}
	var create []string
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if slices.Contains(args, "inspect") {
			_, _ = io.WriteString(out, "ingress")
		}
		if len(args) > 1 && args[1] == "create" {
			create = args
		}
		return nil
	}
	if err := e.ensureIngress(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{
		"CF_DNS_API_TOKEN_FILE=/data/cloudflare-dns-token",
		"--entrypoints.web.http.redirections.entrypoint.priority=1",
		"--certificatesresolvers.cloudflare-dns.acme.dnschallenge.provider=cloudflare",
		"--certificatesresolvers.cloudflare-dns.acme.storage=/data/cloudflare-acme.json",
		"--certificatesresolvers.cloudflare-dns.acme.caserver=https://acme.example.test/directory",
		"--certificatesresolvers.cloudflare-dns.acme.cacertificates=/data/acme-root.pem",
		"--certificatesresolvers.cove.acme.httpchallenge.entrypoint=web",
	} {
		if !slices.Contains(create, arg) {
			t.Fatalf("missing ingress argument %q", arg)
		}
	}
	if strings.Contains(strings.Join(create, " "), "CF_DNS_API_TOKEN=") {
		t.Fatal("credential value passed through Docker command arguments")
	}
}

func TestIngressSettingsRejectInvalidConfiguration(t *testing.T) {
	for _, data := range []string{`broken`, `{"http_redirect_priority":0}`, `{"http_redirect_priority":1000001}`, `{"cloudflare_dns":"yes"}`} {
		e := &Engine{Root: t.TempDir(), Run: func(context.Context, string, []string, []string, io.Writer) error {
			t.Fatal("Docker called with invalid ingress settings")
			return nil
		}}
		if err := os.WriteFile(filepath.Join(e.Root, "ingress-settings.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.ensureIngress(context.Background(), io.Discard); err == nil {
			t.Fatalf("accepted invalid ingress settings: %s", data)
		}
	}
}
