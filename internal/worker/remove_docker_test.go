package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkloadDeletionDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_DELETION_DOCKER") != "1" {
		t.Skip("set COVE_TEST_DELETION_DOCKER=1 in an owned Docker fixture with cached busybox and Restic images")
	}
	for _, scenario := range []string{"generated", "user-named", "shared", "backup", "bind", "no-container-bind"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			owner, err := e.buildOwner()
			if err != nil {
				t.Fatal(err)
			}
			id := "remove-" + owner[:12]
			projects := []string{id}
			docker := func(args ...string) string {
				t.Helper()
				out, err := e.output(ctx, append([]string{"docker"}, args...)...)
				if err != nil {
					t.Fatal(args, err, out)
				}
				return out
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
				defer stop()
				run := func(args ...string) string {
					out, err := e.output(cleanup, append([]string{"docker"}, args...)...)
					if err != nil {
						t.Error("fixture cleanup", args, err, out)
					}
					return out
				}
				for _, container := range strings.Fields(run("ps", "-aq", "--filter", "label=cove.removal.fixture="+owner)) {
					if run("inspect", "--format", `{{index .Config.Labels "cove.removal.fixture"}}`, container) != owner {
						t.Error("fixture container ownership changed")
						continue
					}
					run("rm", "-f", "-v", container)
				}
				for _, workload := range projects {
					for _, network := range strings.Fields(run("network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)) {
						if run("network", "inspect", "--format", `{{index .Labels "com.docker.compose.project"}}`, network) == "cove-"+workload {
							run("network", "rm", network)
						}
					}
					for _, label := range []string{"com.docker.compose.project=cove-" + workload, "cove.backup=" + workload, "dev.cove.restore=" + workload} {
						for _, volume := range strings.Fields(run("volume", "ls", "-q", "--filter", "label="+label)) {
							key, value, _ := strings.Cut(label, "=")
							if run("volume", "inspect", "--format", fmt.Sprintf(`{{index .Labels %q}}`, key), volume) == value {
								run("volume", "rm", volume)
							}
						}
					}
				}
				seenImages := map[string]bool{}
				for _, image := range strings.Fields(run("image", "ls", "-q", "--no-trunc", "--filter", "label=cove.removal.fixture="+owner)) {
					if seenImages[image] {
						continue
					}
					seenImages[image] = true
					var metadata struct {
						RepoTags []string
						Config   struct{ Labels map[string]string }
					}
					if json.Unmarshal([]byte(run("image", "inspect", "--format", "{{json .}}", image)), &metadata) != nil || metadata.Config.Labels["cove.removal.fixture"] != owner {
						t.Error("fixture image ownership changed")
						continue
					}
					if len(metadata.RepoTags) == 0 {
						run("image", "rm", image)
					} else {
						for _, tag := range metadata.RepoTags {
							run("image", "rm", tag)
						}
					}
				}
			})
			// Commit a cached local rootfs with the ownership labels emitted by Cove builds.
			// This tests deletion against real images without fetching a build dependency.
			seed := "cove-removal-seed-" + owner
			docker("create", "--pull", "never", "--name", seed, "--label", "cove.removal.fixture="+owner, "busybox:1.37", "sh", "-ec", "test -f /data/value || echo deletion-value > /data/value; exec sleep 600")
			tag := "cove-" + id + "-app:latest"
			if scenario == "user-named" {
				tag = "cove-removal-user-" + owner + ":chosen"
			}
			docker("commit", "--change", "LABEL dev.cove.build-owner="+owner, "--change", "LABEL com.docker.compose.project=cove-"+id, "--change", "LABEL com.docker.compose.service=app", seed, tag)
			image := docker("image", "inspect", "--format", "{{.Id}}", tag)
			docker("rm", seed)
			dir := filepath.Join(e.Root, "workloads", id)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			volume := "cove-removal-data-" + owner
			compose := fmt.Sprintf("services:\n  app:\n    image: %s\n    volumes: [data:/data]\nvolumes:\n  data:\n    name: %s\n", tag, volume)
			bindPath := filepath.Join(dir, "customer-data")
			if scenario == "bind" || scenario == "no-container-bind" {
				if err := os.MkdirAll(bindPath, 0700); err != nil {
					t.Fatal(err)
				}
				compose = fmt.Sprintf("services:\n  app:\n    image: %s\n    volumes:\n      - type: bind\n        source: %q\n        target: /data\n", tag, bindPath)
			}
			r := Request{ID: "deploy", Workload: id, Action: "deploy", Kind: "service", Compose: compose}
			perform := func(request Request) {
				t.Helper()
				if err := writeJSON(e.path(request.ID), journal{Request: request, Operation: Operation{ID: request.ID, Workload: request.Workload, Action: request.Action, State: "running"}}); err != nil {
					t.Fatal(err)
				}
				if err := e.perform(ctx, request); err != nil {
					log, _ := e.Logs(request.ID)
					t.Fatal(request.Action, err, log)
				}
				j, err := e.read(request.ID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				j.Operation.State = "succeeded"
				j.Operation.FinishedAt = &now
				if err := writeJSON(e.path(request.ID), j); err != nil {
					t.Fatal(err)
				}
			}
			perform(r)
			container := docker("ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+id)
			if docker("exec", container, "cat", "/data/value") != "deletion-value" {
				t.Fatal("fixture application did not write persistent data")
			}
			var backup *BackupConfig
			if scenario == "backup" {
				backup = &BackupConfig{Repository: "local", Password: "fixture-backup-password", KeepLast: 3, IntervalHours: 1}
				request := r
				request.ID, request.Action, request.Backup = "backup", "backup", backup
				perform(request)
			}
			stop := r
			stop.ID, stop.Action = "stop", "stop"
			perform(stop)
			if scenario == "no-container-bind" {
				docker("rm", container)
			}
			if scenario == "shared" {
				docker("run", "-d", "--pull", "never", "--name", "cove-removal-shared-"+owner, "--label", "cove.removal.fixture="+owner, "--label", "com.docker.compose.project=cove-other-"+owner, tag, "sleep", "600")
			}
			if err := os.WriteFile(filepath.Join(dir, "disposable.log"), []byte("discard"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := e.RemoveWorkload(ctx, id)
			if err != nil || result.CleanupWarning != "" {
				t.Fatal("deletion", result, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "disposable.log")); !os.IsNotExist(err) {
				t.Fatal("disposable state remains", err)
			}
			if scenario == "generated" || scenario == "user-named" || scenario == "shared" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) || result.WorkerFilesRetained {
					t.Fatal("unused state tree remains", result, err)
				}
			}
			remaining := docker("image", "ls", "--all", "--quiet", "--no-trunc")
			if scenario == "generated" || scenario == "bind" || scenario == "no-container-bind" {
				if strings.Contains(remaining, image) {
					t.Fatal("generated-only image remains")
				}
			} else if !strings.Contains(remaining, image) {
				t.Fatal("named, shared or backup-referenced image was removed")
			}
			if scenario == "user-named" && docker("image", "inspect", "--format", "{{.Id}}", tag) != image {
				t.Fatal("user-chosen tag changed")
			}
			if scenario == "shared" && docker("inspect", "--format", "{{.State.Running}}", "cove-removal-shared-"+owner) != "true" {
				t.Fatal("other workload container changed")
			}
			if scenario == "bind" || scenario == "no-container-bind" {
				if raw, err := os.ReadFile(filepath.Join(bindPath, "value")); err != nil || strings.TrimSpace(string(raw)) != "deletion-value" {
					t.Fatal("bind data changed", err)
				}
			} else if docker("run", "--rm", "--pull", "never", "--mount", "type=volume,source="+volume+",target=/data,readonly", "busybox:1.37", "cat", "/data/value") != "deletion-value" {
				t.Fatal("named volume data changed")
			}
			if backup != nil {
				copyID := id + "-copy"
				projects = append(projects, copyID)
				copy := Request{ID: "copy", Workload: copyID, Kind: "service", Action: "restore_copy", Backup: backup, BackupID: "backup", RestoreCopy: &RestoreCopy{Source: id}}
				plan, err := e.PlanRestoreCopy(ctx, copy, id)
				if err != nil {
					t.Fatal("deleted resource backup no longer plans a restore copy", err)
				}
				copy.Compose, copy.Environment = plan.Compose, plan.Environment
				if err := os.MkdirAll(filepath.Join(e.Root, "workloads", copyID), 0700); err != nil {
					t.Fatal(err)
				}
				if err := e.performRestoreCopy(ctx, copy, io.Discard); err != nil {
					t.Fatal("restore copy after deletion", err)
				}
				copy.ID, copy.Action, copy.RestoreCopy = "copy-deploy", "deploy", nil
				perform(copy)
				copied := docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+copyID)
				if docker("exec", copied, "cat", "/data/value") != "deletion-value" {
					t.Fatal("restored application bytes differ")
				}
				t.Log("Real local backup planned, restored into isolated storage and deployed after deleting the source resource")
			}
		})
	}
}
