package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerUsageValues(t *testing.T) {
	m := &ContainerMetrics{CPU: "125.5%", Memory: "1.5GiB / 2GiB", Network: "10kB / 2MB", DiskIO: "0B / 1KiB"}
	u := containerUsage(m)
	if u == nil || u.CPUPercent != 125.5 || u.MemoryUsed != 1.5*(1<<30) || u.NetworkRX != 10000 || u.NetworkTX != 2000000 || u.BlockWritten != 1024 {
		t.Fatal(u)
	}
	for _, v := range []string{"", "-1B", "NaNB", "1unknown", "999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999B"} {
		if _, ok := metricBytes(v); ok {
			t.Fatalf("invalid metric accepted: %s", v)
		}
	}
	m.Memory = "unknown"
	if containerUsage(m) != nil {
		t.Fatal("unknown metric became a zero sample")
	}
}

func TestTelemetryDockerCollection(t *testing.T) {
	if os.Getenv("COVE_TEST_TELEMETRY_DOCKER") != "1" {
		t.Skip("COVE_TEST_TELEMETRY_DOCKER=1 required; creates a disposable Docker container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := fmt.Sprintf("telemetry-%d", time.Now().UnixNano())
	name := "cove-test-" + id
	command := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-"+id, "alpine:3.21", "sh", "-c", "echo synthetic-telemetry-secret; touch /tmp/telemetry-ready; sleep 45")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("container cleanup failed: %v %s", err, out)
		}
	})
	if out, err := exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c", "until test -f /tmp/telemetry-ready; do sleep 0.1; done").CombinedOutput(); err != nil {
		t.Fatal("container readiness failed", err, string(out))
	}
	root := t.TempDir()
	dir := filepath.Join(root, "workloads", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{Environment: map[string]string{"SECRET": "synthetic-telemetry-secret"}}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Run: RunCommand}
	result, err := e.CollectTelemetry(ctx, id, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Containers) != 1 || result.Containers[0].Metrics == nil || result.Containers[0].Metrics.Usage == nil {
		t.Fatal("missing Docker metrics", result.Containers)
	}
	if len(result.Logs) != 1 || strings.Contains(result.Logs[0].Message, "synthetic-telemetry-secret") || result.Logs[0].Timestamp.IsZero() {
		t.Fatal("missing or unredacted log", result.Logs)
	}
}
