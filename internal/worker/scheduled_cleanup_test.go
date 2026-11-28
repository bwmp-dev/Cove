package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupPolicyPreviewAndRevalidation(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	image := "sha256:" + strings.Repeat("a", 64)
	protected := "sha256:" + strings.Repeat("b", 64)
	used := false
	var mutations []string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "image ls"):
			if !strings.Contains(command, "label=dev.cove.build-owner="+owner) {
				t.Fatal("foreign images included", args)
			}
			fmt.Fprintln(out, image)
			fmt.Fprintln(out, protected)
		case strings.Contains(command, "image inspect"):
			tags := []string{"app:old"}
			if args[len(args)-1] == protected {
				tags = []string{"cove-release/app:retained"}
			}
			data, _ := json.Marshal(map[string]any{"created": old, "size": 123, "tags": tags})
			out.Write(data)
		case strings.Contains(command, "ps -aq"):
			if used {
				fmt.Fprint(out, "stopped-container")
			}
		case strings.Contains(command, "image rm"):
			if args[len(args)-1] != image || strings.Contains(command, "force") {
				t.Fatal("unsafe removal", args)
			}
			mutations = append(mutations, command)
		default:
			t.Fatal("unexpected command", args)
		}
		return nil
	}
	state, plan, err := e.CleanupPolicy(context.Background(), CleanupControl{Action: "preview"})
	if err != nil || len(plan.Items) != 1 || plan.EstimatedBytes != 123 || state.Policy.Volumes || state.Policy.NonCove {
		t.Fatal(state, plan, err)
	}
	used = true
	if _, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "run", Revision: plan.Revision}); err == nil || len(mutations) != 0 {
		t.Fatal("changed preview applied", err)
	}
	used = false
	state, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "run", Revision: plan.Revision})
	if err != nil || state.Removed != 1 || len(mutations) != 1 {
		t.Fatal(state, err, mutations)
	}
	e.active["app"] = true
	if _, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "run", Revision: plan.Revision}); err == nil {
		t.Fatal("cleanup overlapped workload operation")
	}
	delete(e.active, "app")
	policy := state.Policy
	policy.MinimumAgeHours = 0
	if _, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "save", Policy: &policy}); err == nil {
		t.Fatal("unsafe retention accepted")
	}
}

func TestOwnedCachePreviewAndFilters(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(e.Root, "builder-ready.json"), true); err != nil {
		t.Fatal(err)
	}
	p := CleanupPolicy{IntervalHours: 6, MinimumAgeHours: 168, BuildCache: true}
	if _, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "save", Policy: &p}); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		if !strings.Contains(command, "--builder cove-"+owner) || !strings.Contains(strings.Join(env, " "), "BUILDX_CONFIG="+filepath.Join(e.Root, "buildx")) {
			t.Fatal("foreign builder touched", args, env)
		}
		if args[2] == "du" {
			fmt.Fprintln(out, `{"ID":"old","Reclaimable":true,"Size":"4.096kB","LastUsedAt":"8 days ago"}`)
			fmt.Fprintln(out, `{"ID":"shared","Reclaimable":true,"Shared":true,"Size":"8192","LastUsedAt":"9 days ago"}`)
			fmt.Fprintln(out, `{"ID":"unknown","Reclaimable":true,"Size":"unknown","LastUsedAt":"About a week ago"}`)
			return nil
		}
		if args[2] != "prune" || !strings.Contains(command, "--filter id=") || !strings.Contains(command, "--filter until=168h") {
			t.Fatal("unbounded cache pruning", args)
		}
		mutations++
		return nil
	}
	_, plan, err := e.CleanupPolicy(context.Background(), CleanupControl{Action: "preview"})
	if err != nil || len(plan.Items) != 2 || plan.EstimatedBytes != 4096 || plan.SkippedCache != 1 {
		t.Fatal(plan, err)
	}
	if _, _, err = e.CleanupPolicy(context.Background(), CleanupControl{Action: "run", Revision: plan.Revision}); err != nil || mutations != 2 {
		t.Fatal(err, mutations)
	}
}

func TestCleanupVolumesDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_CLEANUP_DOCKER") != "1" {
		t.Skip("COVE_TEST_CLEANUP_DOCKER=1 required; disposable volumes and stopped container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("cove-cleanup-%d", time.Now().UnixNano())
	orphan, used, foreign := prefix+"-orphan", prefix+"-used", prefix+"-foreign"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	for _, id := range []string{orphan, used, foreign} {
		args := []string{"volume", "create"}
		if id != foreign {
			args = append(args, "--label", "com.docker.compose.project=cove-deleted")
		}
		run(append(args, id)...)
	}
	t.Cleanup(func() {
		for _, id := range []string{orphan, used, foreign} {
			if exec.Command("docker", "volume", "inspect", id).Run() == nil {
				if out, err := exec.Command("docker", "volume", "rm", id).CombinedOutput(); err != nil {
					t.Error(err, string(out))
				}
			}
		}
	})
	run("create", "--name", prefix, "--mount", "type=volume,source="+used+",target=/data", "alpine:3.21", "true")
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", prefix).CombinedOutput(); err != nil {
			t.Error(err, string(out))
		}
	})
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "volume" && args[2] == "ls" {
			_, err := fmt.Fprintln(out, orphan, used, foreign)
			return err
		}
		if len(args) > 4 && args[1] == "volume" && args[2] == "inspect" {
			var capture bytes.Buffer
			if err := RunCommand(ctx, dir, args, env, &capture); err != nil {
				return err
			}
			var value map[string]any
			if err := json.Unmarshal(capture.Bytes(), &value); err != nil {
				return err
			}
			value["created"] = time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
			return json.NewEncoder(out).Encode(value)
		}
		return RunCommand(ctx, dir, args, env, out)
	}
	policy := CleanupPolicy{IntervalHours: 6, MinimumAgeHours: 24, Volumes: true}
	if _, _, err = e.CleanupPolicy(ctx, CleanupControl{Action: "save", Policy: &policy}); err != nil {
		t.Fatal(err)
	}
	_, plan, err := e.CleanupPolicy(ctx, CleanupControl{Action: "preview"})
	if err != nil || len(plan.Items) != 1 || plan.Items[0].ID != orphan {
		t.Fatal(plan, err)
	}
	if _, _, err = e.CleanupPolicy(ctx, CleanupControl{Action: "run", Revision: plan.Revision}); err != nil {
		t.Fatal(err)
	}
	run("inspect", prefix)
	run("volume", "inspect", used)
	run("volume", "inspect", foreign)
	policy.NonCove = true
	if _, _, err = e.CleanupPolicy(ctx, CleanupControl{Action: "save", Policy: &policy}); err != nil {
		t.Fatal(err)
	}
	// Only the remaining fixture volumes are enumerated; never adopt unrelated local data in this acceptance test.
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "volume" && args[2] == "ls" {
			_, err := fmt.Fprintln(out, used, foreign)
			return err
		}
		if len(args) > 4 && args[1] == "volume" && args[2] == "inspect" {
			var capture bytes.Buffer
			if err := RunCommand(ctx, dir, args, env, &capture); err != nil {
				return err
			}
			var value map[string]any
			if err := json.Unmarshal(capture.Bytes(), &value); err != nil {
				return err
			}
			value["created"] = time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
			return json.NewEncoder(out).Encode(value)
		}
		return RunCommand(ctx, dir, args, env, out)
	}
	_, plan, err = e.CleanupPolicy(ctx, CleanupControl{Action: "preview"})
	if err != nil || len(plan.Items) != 1 || plan.Items[0].ID != foreign {
		t.Fatal(plan, err)
	}
	if _, _, err = e.CleanupPolicy(ctx, CleanupControl{Action: "run", Revision: plan.Revision}); err != nil {
		t.Fatal(err)
	}
	run("inspect", prefix)
	run("volume", "inspect", used)
}

func TestCleanupVolumeOptInAndReferences(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", "retained"), 0700); err != nil {
		t.Fatal(err)
	}
	p := CleanupPolicy{IntervalHours: 6, MinimumAgeHours: 24, Volumes: true}
	old := time.Now().Add(-30 * 24 * time.Hour)
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "volume ls"):
			fmt.Fprintln(out, "orphan retained foreign cove-ingress")
		case strings.Contains(command, "volume inspect"):
			id := args[len(args)-1]
			labels := map[string]string{}
			if id == "orphan" {
				labels["com.docker.compose.project"] = "cove-deleted"
			}
			if id == "retained" {
				labels["com.docker.compose.project"] = "cove-retained"
			}
			data, _ := json.Marshal(map[string]any{"created": old, "labels": labels})
			out.Write(data)
		case strings.Contains(command, "ps -aq"):
			return nil
		default:
			t.Fatal("unexpected command", args)
		}
		return nil
	}
	plan, err := e.cleanupPlan(context.Background(), p)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].ID != "orphan" || !plan.UnknownSizes {
		t.Fatal(plan, err)
	}
	p.NonCove = true
	plan, err = e.cleanupPlan(context.Background(), p)
	if err != nil || len(plan.Items) != 2 {
		t.Fatal(plan, err)
	}
}
