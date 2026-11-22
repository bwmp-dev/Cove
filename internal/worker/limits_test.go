package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLimitsValidationAndCompose(t *testing.T) {
	for _, limits := range []map[string]ServiceLimits{{"bad name": {CPUs: 1}}, {"app": {CPUs: -1}}, {"app": {MemoryMB: 5}}, {"app": {CPUs: 0.001}}} {
		if ValidateLimits(limits) == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(`{"services":{"app":{"cpus":2,"mem_limit":1073741824,"environment":{"SECRET":"$literal"},"deploy":{"resources":{"limits":{"cpus":2,"memory":1073741824}}}}}}`), &config); err != nil {
		t.Fatal(err)
	}
	if err := applyLimits(config, map[string]ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 128}}); err != nil {
		t.Fatal(err)
	}
	service := config["services"].(map[string]any)["app"].(map[string]any)
	if service["cpus"] != 0.5 || service["mem_limit"] != int64(134217728) {
		t.Fatal(service)
	}
	if err := applyLimits(config, map[string]ServiceLimits{"absent": {CPUs: 1}}); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func TestLimitsRealDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_LIMITS_DOCKER") != "1" {
		t.Skip("set COVE_TEST_LIMITS_DOCKER=1")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	project := "cove-limit-" + owner
	path := filepath.Join(e.Root, "compose.yaml")
	if err = os.WriteFile(path, []byte("services:\n  app:\n    image: busybox:1.37\n    command: [sleep, '120']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"docker", "compose", "-p", project, "-f", path}
	configured, err := e.prepareLimits(context.Background(), Request{ID: "limits", Limits: map[string]ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 64}}}, e.Root, args, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	args = []string{"docker", "compose", "-p", project, "-f", configured}
	t.Cleanup(func() {
		if err := e.Run(context.Background(), e.Root, append(append([]string{}, args...), "down"), nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	if err = e.Run(context.Background(), e.Root, append(append([]string{}, args...), "up", "-d", "--wait"), nil, os.Stderr); err != nil {
		t.Fatal(err)
	}
	id, err := e.output(context.Background(), "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		t.Fatal(err)
	}
	samples := []Container{{ID: strings.TrimSpace(id)}}
	if err := e.containerMetrics(context.Background(), samples); err != nil || samples[0].Metrics == nil || samples[0].Metrics.Memory == "" {
		t.Fatal("container metrics unavailable", err)
	}
	values, err := e.output(context.Background(), "docker", "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", strings.TrimSpace(id))
	if err != nil || strings.TrimSpace(values) != "500000000 67108864" {
		t.Fatal(values, err)
	}
}
