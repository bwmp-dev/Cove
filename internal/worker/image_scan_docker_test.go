package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageScanDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_SCANNER_DOCKER") != "1" {
		t.Skip("requires the owned nested scanner fixture")
	}
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	ctx, stop := context.WithTimeout(context.Background(), 12*time.Minute)
	defer stop()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(e.Root, "scanner")
	candidate := filepath.Join(root, "candidate-test")
	if err = os.MkdirAll(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(os.Getenv("COVE_TEST_SCANNER_DATABASE_ARCHIVE"))
	if err != nil {
		t.Fatal(err)
	}
	if disk, diskErr := e.output(ctx, "df", "-Pk", e.Root); diskErr == nil {
		t.Log("owned fixture filesystem", disk)
	}
	database, err := extractScanDatabase(file, candidate)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	name := "db-" + strings.TrimPrefix(database.Digest, "sha256:")
	if err = os.Rename(candidate, filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	if err = e.Run(ctx, e.Root, []string{"chmod", "-R", "a+rX", filepath.Join(root, name)}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(root, "current.json"), scanDatabaseCache{ScanDatabase: database, Path: name}); err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	base := "cove-scan-fixture-" + owner + "-base:test"
	if err = e.Run(ctx, e.Root, []string{"docker", "tag", "sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373", base}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := e.Run(cleanup, e.Root, []string{"docker", "image", "rm", base}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	}()
	for _, fixture := range []struct {
		label string
		java  bool
		job   bool
	}{{"plain", false, false}, {"java", true, false}, {"retained-job", false, true}} {
		label, java := fixture.label, fixture.java
		t.Run(label, func(t *testing.T) {
			seed := "cove-scan-fixture-" + owner + "-" + label
			image := seed + ":test"
			app := seed + "-app"
			workload := "scan-" + label
			run := func(args ...string) {
				t.Helper()
				output := &limitedBuffer{Limit: 16384}
				if err := e.Run(ctx, e.Root, args, nil, output); err != nil {
					t.Fatal("fixture Docker command failed", args[1], err, output.data.String())
				}
			}
			command := "true"
			if java {
				command = "touch /fixture.JAR"
			}
			run("docker", "run", "--pull", "never", "--name", seed, "--network", "none", "--entrypoint", "/bin/sh", "--label", "cove.scan-fixture="+owner, base, "-c", command)
			run("docker", "commit", "--change", "ENV COVE_FIXTURE_SECRET=fixture-only-do-not-store", "--change", "LABEL cove.scan-fixture="+owner, seed, image)
			run("docker", "rm", seed)
			command = "exec sleep 600"
			if fixture.job {
				command = "exit 0"
			}
			run("docker", "run", "-d", "--pull", "never", "--name", app, "--network", "none", "--entrypoint", "/bin/sh", "--label", "cove.scan-fixture="+owner, "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=app", image, "-c", command)
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				labels, labelErr := e.output(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.scan-fixture"}}`, app)
				if labelErr != nil || strings.TrimSpace(labels) != owner {
					t.Error("fixture ownership unavailable; refusing cleanup")
					return
				}
				if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", app}, nil, io.Discard); err != nil {
					t.Error(err)
				}
				if err := e.Run(cleanup, e.Root, []string{"docker", "image", "rm", image}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}()
			if fixture.job {
				run("docker", "wait", app)
			}
			snapshot := func() string {
				value, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.Running}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}", app)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			r := Request{ID: "scan-" + label, Workload: workload, Kind: "service", Action: "image_scan"}
			if fixture.job {
				r.Kind, r.JobService = "job", "app"
			}
			if _, err = e.Submit(r); err != nil {
				t.Fatal(err)
			}
			journal, _ := e.read(r.ID)
			e.execute(ctx, journal)
			operation, err := e.Status(r.ID)
			if err != nil || operation.State != "succeeded" || operation.ImageScan == nil {
				t.Fatal(operation.State, err)
			}
			report := operation.ImageScan
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "fixture-only-do-not-store") || snapshot() != before || len(report.Images) != 1 || len(report.Images[0].Findings) == 0 {
				t.Fatal("scan changed the application, leaked metadata or lost findings", report.State, report.Message)
			}
			if java {
				if report.State != "unknown" || report.Images[0].Coverage.JavaArchives != "not_scanned" || report.Images[0].Coverage.OS != "scanned" || report.Complete(time.Now().UTC()) {
					t.Fatal("Java dependency gap appeared complete", string(encoded))
				}
			} else if !report.Complete(time.Now().UTC()) {
				t.Fatal("plain fixture coverage unavailable", string(encoded))
			}
			t.Log("real offline scan", label, report.State, len(report.Images[0].Findings), "findings; application unchanged")
		})
	}
}
