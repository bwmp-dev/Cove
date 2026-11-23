package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredRunnerPreservesAndClearsProxyEnvironment(t *testing.T) {
	if os.Getenv("COVE_PROXY_ENV_HELPER") == "1" {
		_, _ = os.Stdout.WriteString(os.Getenv("HTTPS_PROXY"))
		os.Exit(0)
	}
	t.Setenv("COVE_PROXY_ENV_HELPER", "1")
	t.Setenv("HTTPS_PROXY", "http://worker-host.example:3128")
	root := t.TempDir()
	run := configuredRunner(root)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	check := func(environment []string, expected string) {
		t.Helper()
		var output bytes.Buffer
		if err := run(context.Background(), root, []string{executable, "-test.run=^TestConfiguredRunnerPreservesAndClearsProxyEnvironment$"}, environment, &output); err != nil {
			t.Fatal(err, output.String())
		}
		if strings.TrimSpace(output.String()) != expected {
			t.Fatalf("proxy environment = %q, want %q", output.String(), expected)
		}
	}
	check(nil, "http://worker-host.example:3128")
	if err := os.WriteFile(filepath.Join(root, "outbound-proxy.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	check(nil, "")
	check([]string{"HTTPS_PROXY=http://snapshot.example:3128"}, "http://snapshot.example:3128")
	if err := os.WriteFile(filepath.Join(root, "outbound-proxy.json"), []byte(`{"https_proxy":"http://instance.example:3128"}`), 0600); err != nil {
		t.Fatal(err)
	}
	check(nil, "http://instance.example:3128")
}
