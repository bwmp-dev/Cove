package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestComposeAdoptionDockerAcceptance(t *testing.T) {
	if os.Getenv("COVE_ADOPTION_DOCKER") != "1" {
		t.Skip("set COVE_ADOPTION_DOCKER=1 for an isolated local Compose project")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	project := "adopt-test-" + strings.ToLower(rand.Text())
	workload := "adopt-" + strings.ToLower(rand.Text())
	if err := os.WriteFile(path, []byte("services:\n  app:\n    image: postgres:17-alpine\n    command: ['sleep', '300']\n    network_mode: none\n    labels:\n      cove.test: 'true'\n    environment:\n      IMPORT_TEST: private-adoption-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	compose := []string{"compose", "--project-name", project, "--file", path}
	if output, err := exec.Command("docker", append(append([]string{}, compose...), "up", "--detach", "--pull", "never", "--no-build")...).CombinedOutput(); err != nil {
		t.Fatal("could not create owned Compose fixture", err, string(output))
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", append(append([]string{}, compose...), "down", "--volumes", "--remove-orphans")...).CombinedOutput(); err != nil {
			t.Error("could not remove owned fixture", err, string(output))
		}
	})
	identity := func() string {
		t.Helper()
		out, err := exec.Command("docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+project).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	id := identity()
	if len(id) != 64 {
		t.Fatal("owned fixture has no unique identity")
	}
	started := func() string {
		t.Helper()
		out, err := exec.Command("docker", "inspect", "--format", "{{.State.StartedAt}}", id).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	before := started()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, environment []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "ps" && strings.Contains(strings.Join(args, " "), "label=com.docker.compose.project --format") {
			args = append(append([]string{}, args...), "--filter", "label=com.docker.compose.project="+project)
		}
		return RunCommand(ctx, dir, args, environment, out)
	}
	projects, err := e.ScanComposeProjects(context.Background())
	if err != nil || len(projects) != 1 || projects[0].Project != project {
		t.Fatal("isolated scan did not identify fixture", err, projects)
	}
	plan, err := e.PlanComposeAdoption(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if identity() != id || started() != before {
		t.Fatal("planning restarted the source")
	}
	request := AdoptionRequest{Project: project, Fingerprint: plan.Fingerprint, Workload: workload, Release: "initial-adoption"}
	serviceHash := sha256.Sum256([]byte("app"))
	tag := "cove-release/" + workload + ":initial-adoption-" + hex.EncodeToString(serviceHash[:8])
	t.Cleanup(func() {
		if exec.Command("docker", "image", "inspect", tag).Run() != nil {
			return
		}
		if output, err := exec.Command("docker", "image", "rm", tag).CombinedOutput(); err != nil {
			t.Error("could not remove owned retention tag", err, string(output))
		}
	})
	if err = e.AdoptCompose(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if identity() != id || started() != before {
		t.Fatal("adoption restarted or recreated the source")
	}

	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	containers, err := restarted.Containers(context.Background(), workload)
	if err != nil || len(containers) != 1 || !strings.HasPrefix(id, containers[0].ID) {
		t.Fatal("restarted worker lost source ownership", err)
	}
	for _, action := range []string{"stop", "start"} {
		request := Request{ID: "acceptance-" + action, Workload: workload, Action: action, RecordDrift: true}
		if _, err = restarted.Submit(request); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err = restarted.dispatch(context.Background(), &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		operation, err := restarted.Status(request.ID)
		if err != nil || operation.State != "succeeded" {
			t.Fatal("adopted lifecycle action failed", action, err, operation.Error)
		}
		if identity() != id {
			t.Fatal("lifecycle command recreated the adopted container")
		}
	}
}
