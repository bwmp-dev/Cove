package worker

import (
	"context"
	"cove/internal/outbound"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestImageScansMergeOnlyInfrastructureProxyMayAccompanyMetadata(t *testing.T) {
	proxy := &outbound.Config{HTTPS: "http://fixture:opaque@proxy.example.invalid:8080"}
	r := Request{ID: "scan", Workload: "app", Action: "image_scan", Kind: "service", OutboundProxy: proxy}
	if err := ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Environment = map[string]string{"TOKEN": "application-secret"} },
		func(r *Request) {
			r.Registry = &Registry{Server: "registry.example.invalid", Username: "fixture", Password: "application-secret"}
		},
		func(r *Request) { r.ManagedDatabase = true },
		func(r *Request) { r.Compose = "services: {}" },
	} {
		copy := r
		mutate(&copy)
		if ValidateRequest(copy) == nil {
			t.Fatal("scan accepted application deployment inputs")
		}
	}
}

func TestImageScansMergeDatabaseRefreshUsesProxyEnvironmentOnly(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy := &outbound.Config{HTTP: "http://fixture:opaque@proxy.example.invalid:8080", HTTPS: "http://fixture:opaque@proxy.example.invalid:8080", NoProxy: "localhost"}
	started := false
	e.Run = func(_ context.Context, _ string, args, env []string, _ io.Writer) error {
		if args[1] == "ps" {
			return nil
		}
		if args[1] != "run" {
			t.Fatal("unexpected Docker request", args[1])
		}
		started = true
		if strings.Contains(strings.Join(args, " "), "opaque") || !slices.Equal(env, proxy.Env()) {
			t.Fatal("proxy credential was exposed in arguments or policy was lost")
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
			found := false
			for i := 0; i+1 < len(args); i++ {
				found = found || args[i] == "--env" && args[i+1] == key
			}
			if !found {
				t.Fatal("missing proxy propagation", key)
			}
		}
		return errors.New("fixture stopped before refresh")
	}
	if _, err := e.refreshScanDatabase(context.Background(), "owner", "operation", t.TempDir(), ScannerImage, proxy); err == nil || !started {
		t.Fatal("fixture did not exercise the refresh boundary", err)
	}
}

func TestImageScansMergeDistinctPlatformIdentitiesAndLegacyUnknown(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	first, second := "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	if scanImageIdentity(id, first) == scanImageIdentity(id, second) {
		t.Fatal("platforms with one Docker index were coalesced")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] != "inspect" || args[3] != "{{json .ImageManifestDescriptor}}" {
			t.Fatal("platform probe changed application state")
		}
		_, err := io.WriteString(out, `{"Digest":"`+first+`"}`)
		return err
	}
	if e.scanPlatform(context.Background(), "container") != first {
		t.Fatal("deployed platform descriptor lost")
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		return errors.New("unsupported descriptor")
	}
	if e.scanPlatform(context.Background(), "container") != "" {
		t.Fatal("unsupported inspection fabricated platform proof")
	}
}
