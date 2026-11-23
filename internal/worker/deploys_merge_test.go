package worker

import (
	"context"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cove/internal/outbound"
)

func TestDeploysMergeForwardAuthUsesPinnedProxy(t *testing.T) {
	target := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target <- r.Host
		http.Error(w, "fixture refuses before contacting destination", 502)
	}))
	defer proxy.Close()
	transport := publicAuthTransport(x509.NewCertPool(), outbound.Config{HTTPS: proxy.URL})
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), "GET", "https://8.8.8.8/check", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := transport.RoundTrip(request); err == nil {
		response.Body.Close()
		t.Fatal("refused proxy succeeded")
	}
	select {
	case address := <-target:
		if address != "8.8.8.8:443" {
			t.Fatal("proxy did not receive the validated address", address)
		}
	default:
		t.Fatal("forward auth bypassed the selected proxy")
	}
}

func TestDeploysMergeAdoptedReplicaSelectorsAndLogicalService(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.adoptedProjects["app"] = adoptedProject{Project: "existing-project"}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "label=com.docker.compose.project=existing-project") {
			t.Fatal("replica selector ignored adopted ownership", args)
		}
		if strings.Contains(joined, "--format {{json .}}") {
			_, err := io.WriteString(out, `{"ID":"aaaaaaaaaaaa","Labels":"com.docker.compose.service=coveg-old-web,dev.cove.service=web","State":"running"}`)
			return err
		}
		_, err := io.WriteString(out, "aaaaaaaaaaaa")
		return err
	}
	if ids, err := e.trafficContainers(t.Context(), "app", "web", true); err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	containers, err := e.Containers(t.Context(), "app")
	if err != nil || len(containers) != 1 || containers[0].Service != "web" {
		t.Fatal("telemetry lost logical service identity", containers, err)
	}
}

func TestDeploysMergeSuspensionDefersDriftWithoutInspectingHelpers(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := workloadSuspension{Operation: "pause", Created: time.Now().UTC(), Phase: "paused", Options: SuspensionOptions{Reason: "idle", Scope: "app"}}
	if err = writeJSON(filepath.Join(dir, "suspension.json"), state); err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("suspended drift inspected runtime or auxiliary containers")
		return nil
	}
	report, err := e.Drift(t.Context(), "app")
	if err != nil || !report.Busy || report.Deferred != "suspended" || len(report.Changes) != 0 {
		t.Fatal(report, err)
	}
}

func TestDeploysMergeCapacityCountsRequestedReplicas(t *testing.T) {
	config := map[string]any{"services": map[string]any{"web": map[string]any{"cpus": float64(1), "mem_limit": float64(256 * 1024 * 1024)}}}
	sample := &NodeMetrics{CPUs: 8, MemoryTotal: 1 << 30, MemoryAvailable: 512 << 20}
	before := capacityChecks(config, sample)
	applyCapacityReplicas(config, &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"web": 3}})
	after := capacityChecks(config, sample)
	if len(before) == 0 || len(after) == 0 {
		t.Fatal(before, after)
	}
	warnings := func(checks []PreflightCheck) int {
		n := 0
		for _, check := range checks {
			if check.State == "warning" {
				n++
			}
		}
		return n
	}
	if warnings(after) <= warnings(before) {
		t.Fatal("capacity advice ignored the three-replica memory requirement", before, after)
	}
}

func TestDeploysMergeLiveLogsRedactRetainedBundleSecrets(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{AdditionalSecrets: []string{"bundle-token"}, Routes: []Route{{Policy: &RoutePolicy{ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.com/check?token=route-secret"}}}}}); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) > 1 && args[1] == "ps" {
			_, err := io.WriteString(out, `{"ID":"aaaaaaaaaaaa","Labels":"com.docker.compose.service=coveg-web,dev.cove.service=web","State":"running"}`)
			return err
		}
		_, err := io.WriteString(out, time.Now().UTC().Format(time.RFC3339Nano)+" bundle-token route-secret\n")
		return err
	}
	result, err := e.CollectLiveLogs(t.Context(), "app", time.Now().Add(-time.Minute))
	if err != nil || len(result.Logs) != 1 || strings.Contains(result.Logs[0].Message, "bundle-token") || strings.Contains(result.Logs[0].Message, "route-secret") {
		t.Fatal("structured logs disclosed retained bundle data", result, err)
	}
}

func TestDeploysMergeProxyChangeRefreshesForwardAuthOrRollsBack(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := &outbound.Config{HTTPS: "http://127.0.0.1:3128"}
	if err = e.configureOutbound(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	routes := map[string][]Route{"app": {{Domain: "app.example.com", Service: "web", Port: 80, Policy: &RoutePolicy{ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.com/check"}}}}}
	if err = writeJSON(filepath.Join(e.Root, "ingress.json"), routes); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		calls++
		return errors.New("fixture denies ingress changes")
	}
	if err = e.configureOutbound(t.Context(), old); err != nil || calls != 0 {
		t.Fatal("unchanged proxy refreshed ingress", calls, err)
	}
	if err = e.configureOutbound(t.Context(), &outbound.Config{HTTPS: "http://127.0.0.1:3129"}); err == nil || calls == 0 {
		t.Fatal("changed public forward-auth kept stale proxy policy", calls, err)
	}
	saved, err := readOutboundConfig(e.Root)
	if err != nil || saved == nil || *saved != *old {
		t.Fatal("failed helper change committed a mismatched proxy", saved, err)
	}
}

func TestDeploysMergeImagePublicationRequiresMatchingDaemonProxy(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selected := &outbound.Config{HTTP: "http://proxy.example:3128", HTTPS: "http://proxy.example:3128", NoProxy: "localhost,127.0.0.1"}
	matches := false
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) < 2 || args[1] != "info" {
			t.Fatal("unexpected command", args)
		}
		report := map[string]string{"http": "", "https": "", "no_proxy": ""}
		if matches {
			report = map[string]string{"http": selected.HTTP, "https": selected.HTTPS, "no_proxy": selected.NoProxy}
		}
		return json.NewEncoder(out).Encode(report)
	}
	if err = e.requireDockerProxy(t.Context(), selected); err == nil {
		t.Fatal("image publication silently bypassed selected proxy")
	}
	matches = true
	if err = e.requireDockerProxy(t.Context(), selected); err != nil {
		t.Fatal(err)
	}
}

func TestDeploysMergePrivateBuilderTracksProxyChanges(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	creates, removals := 0, 0
	expected := ""
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		command := strings.Join(args, " ")
		if strings.Contains(command, "config --format json") {
			_, err := io.WriteString(out, `{"services":{"web":{"build":{"context":"."}}}}`)
			return err
		}
		if len(args) > 2 && args[1] == "buildx" && args[2] == "create" {
			creates++
			foundProxy, foundBypass := false, false
			for i := 1; i < len(args); i++ {
				if args[i-1] != "--driver-opt" {
					continue
				}
				fields, err := csv.NewReader(strings.NewReader(args[i])).Read()
				if err != nil || len(fields) != 1 {
					t.Fatal(args[i], err)
				}
				foundProxy = foundProxy || fields[0] == "env.HTTPS_PROXY="+expected
				foundBypass = foundBypass || fields[0] == "env.NO_PROXY=localhost,127.0.0.1"
			}
			if !foundProxy || !foundBypass {
				t.Fatal("private builder lacks selected proxy or CSV-safe bypass list", args)
			}
		}
		if len(args) > 2 && args[1] == "buildx" && args[2] == "rm" {
			removals++
		}
		return nil
	}
	for _, port := range []string{"3128", "3128", "3129"} {
		expected = "http://proxy.example:" + port
		policy := &outbound.Config{HTTPS: expected, NoProxy: "localhost,127.0.0.1"}
		if err = e.configureOutbound(t.Context(), policy); err != nil {
			t.Fatal(err)
		}
		if err = e.buildImages(t.Context(), e.Root, []string{"docker", "compose", "-f", "compose.yaml"}, policy.Env(), io.Discard, false); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 2 || removals != 1 {
		t.Fatal("builder policy drift or unexpected cache replacement", creates, removals)
	}
}

func TestDeploysMergeCompletedGenerationInitIsNotACrash(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "generation.compose.json")
	if err = os.WriteFile(compose, []byte(`{"services":{"coveg-release-init":{"image":"busybox:1.37","labels":{"dev.cove.service":"init"}},"coveg-release-web":{"image":"busybox:1.37","depends_on":{"coveg-release-init":{"condition":"service_completed_successfully"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: compose}); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, `{"ID":"aaaaaaaaaaaa","State":"exited","Labels":"com.docker.compose.service=coveg-release-init,dev.cove.service=init"}`)
			return err
		}
		_, err := io.WriteString(out, `{"id":"aaaaaaaaaaaa","service":"init","state":{"Status":"exited","ExitCode":0}}`)
		return err
	}
	containers, err := e.HealthContainers(t.Context(), "app")
	if err != nil || len(containers) != 1 || !containers[0].ExpectedCompleted {
		t.Fatal("successful rollout init classified as failed container", containers, err)
	}
}

func TestDeploysMergeGitBuildUsesCheckedOutCommitWithoutOverwritingExplicitValue(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "explicit"}[override], func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			revision := strings.Repeat("a", 40)
			environment := map[string]string{}
			expected := revision
			if override {
				expected = "operator-value"
				environment[CoveCommitSHA] = expected
			}
			checked := false
			e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
				command := strings.Join(args, " ")
				if args[0] == "git" && strings.Contains(command, " clone ") {
					dir := args[len(args)-1]
					if err := os.MkdirAll(dir, 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}"), 0600)
				}
				if args[0] == "git" && args[1] == "rev-parse" {
					_, err := io.WriteString(out, revision)
					return err
				}
				if strings.Contains(command, "config --format json") {
					checked = true
					found := false
					for _, entry := range env {
						found = found || entry == CoveCommitSHA+"="+expected
					}
					if !found {
						t.Errorf("build did not receive checked-out/explicit commit builtin: %v", env)
					}
					return errors.New("fixture ends after build input verification")
				}
				t.Fatalf("unexpected build fixture command %v", args)
				return nil
			}
			r := Request{ID: "publish", Workload: "app", Action: "build", Source: &GitSource{Repository: "https://repository.invalid/app", Branch: "main", ComposePath: "compose.yaml"}, Build: &BuildOptions{Repository: "team/app"}, Registry: &Registry{Server: "registry.invalid", Username: "fixture", Password: "fixture-secret"}, Environment: environment, BuiltinDefaults: map[string]string{CoveCommitSHA: ""}}
			if _, err = e.Submit(r); err != nil {
				t.Fatal(err)
			}
			var jobs sync.WaitGroup
			if err = e.dispatch(t.Context(), &jobs); err != nil {
				t.Fatal(err)
			}
			jobs.Wait()
			if !checked {
				logs, _ := e.Logs(r.ID)
				t.Fatal("build input check not reached", logs)
			}
		})
	}
}
