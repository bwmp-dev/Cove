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

func TestComposeAdoptionPreservesSourceAndRecoversRegistry(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	path := filepath.Join(source, "compose.yaml")
	if err = os.WriteFile(path, []byte("services: {app: {image: fixture}}"), 0600); err != nil {
		t.Fatal(err)
	}
	id, image, hash := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	labels := map[string]string{"com.docker.compose.project": "legacy-app", "com.docker.compose.service": "app", "com.docker.compose.oneoff": "False", "com.docker.compose.config-hash": hash, "com.docker.compose.project.working_dir": source, "com.docker.compose.project.config_files": path}
	container := map[string]any{"Id": id, "Image": image, "Config": map[string]any{"Labels": labels, "Image": "fixture", "Env": []string{"PASSWORD=private-adopted-value"}}, "HostConfig": map[string]any{}, "Mounts": []any{}, "State": map[string]any{"Status": "running", "Running": true}, "NetworkSettings": map[string]any{"Networks": map[string]any{}}}
	resolved := map[string]any{"name": "legacy-app", "services": map[string]any{"app": map[string]any{"image": "fixture", "environment": map[string]string{"PASSWORD": "private-adopted-value"}}}}
	tags := 0
	runner := func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			if !strings.Contains(strings.Join(args, " "), "label=com.docker.compose.project=legacy-app") {
				t.Fatal("wrong project selector", args)
			}
			_, err := io.WriteString(out, id+"\n")
			return err
		case "inspect":
			return json.NewEncoder(out).Encode([]any{container})
		case "image":
			if len(args) != 5 || args[2] != "tag" || args[3] != image || !strings.HasPrefix(args[4], "cove-release/adopted:") {
				t.Fatal("unexpected source mutation", args)
			}
			tags++
			return nil
		case "compose":
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--project-name legacy-app") || !strings.Contains(joined, "config") {
				t.Fatal("adoption changed source containers", args)
			}
			if strings.Contains(joined, "--hash") {
				_, err := io.WriteString(out, "app "+hash+"\n")
				return err
			}
			return json.NewEncoder(out).Encode(resolved)
		default:
			t.Fatal("unexpected adoption command", args)
		}
		return nil
	}
	e.Run = runner
	resolved["secrets"] = map[string]any{"source": map[string]string{"environment": "HOST_ONLY_SECRET"}}
	if _, err = e.PlanComposeAdoption(context.Background(), "legacy-app"); err == nil || tags != 0 {
		t.Fatal("missing process-only secret was accepted")
	}
	delete(resolved, "secrets")
	plan, err := e.PlanComposeAdoption(context.Background(), "legacy-app")
	if err != nil {
		t.Fatal(err)
	}
	if tags != 0 || strings.Contains(plan.Configuration.Compose, "private-adopted-value") {
		t.Fatal("plan modified source or exposed values")
	}
	request := AdoptionRequest{Project: "legacy-app", Fingerprint: plan.Fingerprint, Workload: "adopted", Release: "adopt-release"}
	container["HostConfig"] = map[string]any{"RestartPolicy": map[string]string{"Name": "always"}}
	if err = e.AdoptCompose(context.Background(), request); err == nil || tags != 0 {
		t.Fatal("changed runtime bypassed reviewed fingerprint")
	}
	container["HostConfig"] = map[string]any{}
	labels["com.docker.compose.config-hash"] = strings.Repeat("d", 64)
	if err = e.AdoptCompose(context.Background(), request); err == nil || tags != 0 {
		t.Fatal("source declaration mismatch changed ownership")
	}
	labels["com.docker.compose.config-hash"] = hash
	if err = e.AdoptCompose(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if tags != 1 || e.composeProject("adopted") != "legacy-app" || e.composeProject("other") != "cove-other" {
		t.Fatal("ownership mapping failed")
	}
	if err = e.AdoptCompose(context.Background(), request); err != nil || tags != 1 {
		t.Fatal("adoption replay was not idempotent", err)
	}
	if err = os.Remove(filepath.Join(e.Root, "adopted-projects.json")); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(e.Root)
	if err != nil || restarted.composeProject("adopted") != "legacy-app" {
		t.Fatal("adoption lost on restart", err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, "adopted-projects.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewEngine(e.Root); err == nil {
		t.Fatal("corrupt registry fell back to a different project")
	}
}

func TestAdoptionProtectsReviewAndRejectsSystemProjects(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.adoptionFingerprint(map[string]string{"password": "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.adoptionFingerprint(map[string]string{"password": "two"})
	if err != nil || first == second {
		t.Fatal("review does not bind source values", err)
	}
	for _, project := range []string{"cove-app", "coolify-proxy", "traefik", "portainer", "../escape", "CAPITAL"} {
		if validAdoptionProject(project) {
			t.Fatal("unsafe project accepted", project)
		}
	}
}
