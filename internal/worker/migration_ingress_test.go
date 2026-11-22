package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStagedCertificatesWaitForBridge(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	if err := writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
		t.Fatal(err)
	}
	config, err := ingressConfig(map[string][]Route{"app": {{Domain: "app.example.test", Service: "web", Port: 80}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		if enabled {
			if err := writeJSON(filepath.Join(e.Root, "ingress-domains.json"), []string{"app.example.test"}); err != nil {
				t.Fatal(err)
			}
		}
		actual, err := e.migrationIngressConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		var parsed dynamicIngress
		if err := json.Unmarshal([]byte(actual), &parsed); err != nil {
			t.Fatal(err)
		}
		for _, router := range parsed.HTTP.Routers {
			if (router.TLS["certResolver"] == "cove") != enabled {
				t.Fatalf("certificate request does not match bridge readiness: %s", actual)
			}
		}
	}
}

func TestMigrationIngressBindings(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "staged"}[staged], func(t *testing.T) {
			e := &Engine{Root: t.TempDir()}
			if err := writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: staged, TLSChallenge: true}); err != nil {
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
			joined := strings.Join(create, " ")
			if slices.Contains(create, "-p") == staged {
				t.Fatalf("unexpected port bindings: %s", joined)
			}
			if !strings.Contains(joined, "acme.tlschallenge=true") || strings.Contains(joined, "httpchallenge") {
				t.Fatal(joined)
			}
		})
	}
}

func TestMigrationIngressRejectsBrokenMode(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	for _, data := range []string{`{"staged":true}`, `broken`} {
		if err := os.WriteFile(filepath.Join(e.Root, "ingress-mode.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.ingressArgs(); err == nil {
			t.Fatal("accepted unsafe migration mode")
		}
	}
}

func TestPrepareIngressRequiresQuiescence(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Run: func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("Docker was touched before the worker was idle")
		return nil
	}}
	if err := e.PrepareIngress(context.Background(), io.Discard); err == nil {
		t.Fatal("allowed undrained ingress changes")
	}
	if err := writeJSON(filepath.Join(e.Root, "draining.json"), true); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Root, "operations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.Root, "operations", "active.json"), journal{Operation: Operation{State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.PrepareIngress(context.Background(), io.Discard); err == nil {
		t.Fatal("allowed ingress changes during an operation")
	}
}
