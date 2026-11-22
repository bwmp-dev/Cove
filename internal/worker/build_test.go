package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildPublicationPreservesRelease(t *testing.T) {
	for _, failure := range []string{"", "manifest", "build", "verify", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			e, _ := NewEngine(t.TempDir())
			r := Request{ID: "publish", Workload: "app", Action: "build", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app", NoCache: true}, Registry: &Registry{Server: "registry.example.com", Username: "fixture", Password: "registry-secret"}, Environment: map[string]string{"BUILD_TOKEN": "build-secret"}, FileSecrets: []FileSecret{{Name: "TOKEN", Variable: "BUILD_TOKEN", BuildServices: []string{"app"}}}}
			dir := filepath.Join(e.Root, "workloads", r.Workload)
			os.MkdirAll(dir, 0700)
			current := []byte(`{"release":"production","routes":[{"domain":"example.com"}]}`)
			os.WriteFile(filepath.Join(dir, "current.json"), current, 0600)
			imageID := "sha256:" + strings.Repeat("a", 64)
			digest := "sha256:" + strings.Repeat("b", 64)
			if failure == "manifest" {
				imageID = digest
			}
			var commands []string
			e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
				command := strings.Join(args, " ")
				commands = append(commands, command)
				switch {
				case strings.Contains(command, "config --format json"):
					if cwd != dir {
						t.Fatal("lost Compose working directory")
					}
					_, err := io.WriteString(out, `{"services":{"app":{"build":{"context":".","secrets":["TOKEN"]}},"db":{"image":"postgres:17"}}}`)
					return err
				case strings.Contains(command, " build --builder "):
					if !strings.HasSuffix(command, "--no-cache") || !strings.Contains(strings.Join(env, " "), "BUILDKIT_PROGRESS=plain") {
						t.Fatal(command, env)
					}
					data, err := os.ReadFile(filepath.Join(dir, r.ID+".build-tags.compose.json"))
					if err != nil || !strings.Contains(string(data), buildImageTag(r, "app")) || strings.Contains(string(data), `"db"`) {
						t.Fatal(string(data), err)
					}
					io.WriteString(out, "build-secret registry-secret\n")
					if failure == "build" {
						return errors.New("build failed")
					}
					if failure == "cancel" {
						return context.Canceled
					}
				case strings.Contains(command, "docker image inspect"):
					return json.NewEncoder(out).Encode(map[string]string{"id": imageID, "os": "linux", "architecture": "amd64"})
				case strings.Contains(command, "imagetools inspect --format"):
					return json.NewEncoder(out).Encode(map[string]string{"digest": digest})
				case strings.Contains(command, "imagetools inspect --raw"):
					id := "sha256:" + strings.Repeat("a", 64)
					if failure == "verify" {
						id = "sha256:" + strings.Repeat("c", 64)
					}
					return json.NewEncoder(out).Encode(map[string]any{"config": map[string]string{"digest": id}})
				case strings.Contains(command, "docker image ls"):
					return json.NewEncoder(out).Encode(map[string]string{"Repository": r.Registry.Server + "/" + r.Build.Repository, "Tag": strings.Split(buildImageTag(r, "app"), ":")[1]})
				}
				return nil
			}
			if _, err := e.Submit(r); err != nil {
				t.Fatal(err)
			}
			var jobs sync.WaitGroup
			if err := e.dispatch(context.Background(), &jobs); err != nil {
				t.Fatal(err)
			}
			jobs.Wait()
			op, err := e.Status(r.ID)
			success := failure == "" || failure == "manifest"
			if err != nil || (success && (op.State != "succeeded" || op.Build == nil || len(op.Build.Images) != 1 || op.Build.Images[0].Reference != r.Registry.Server+"/"+r.Build.Repository+"@"+digest)) || (!success && op.State != "failed") {
				t.Fatal(op, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "current.json"))
			if err != nil || string(data) != string(current) {
				t.Fatal("release changed", string(data), err)
			}
			for _, cmd := range commands {
				if strings.Contains(cmd, " up ") || strings.Contains(cmd, " down") || strings.Contains(cmd, "docker network") {
					t.Fatal("runtime changed", cmd)
				}
			}
			for _, path := range []string{filepath.Join(dir, "pending-build.json"), filepath.Join(dir, r.ID+".build.yaml"), filepath.Join(dir, r.ID+".build-tags.compose.json"), filepath.Join(dir, r.ID+".secrets.compose.json"), filepath.Join(e.Root, "file-secrets", r.Workload, r.ID)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("build input retained", path, err)
				}
			}
			logs, _ := e.Logs(r.ID)
			if strings.Contains(logs, "build-secret") || strings.Contains(logs, "registry-secret") {
				t.Fatal("secret exposed", logs)
			}
		})
	}
}

func TestBuildCancellationCleansInputs(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	r := Request{ID: "cancel-build", Workload: "app", Action: "build", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app"}, Registry: &Registry{Server: "registry.example.com", Username: "fixture", Password: "fixture-password"}}
	started := make(chan struct{})
	e.Run = func(ctx context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		if strings.Contains(command, "config --format json") {
			return ioWrite(out, `{"services":{"app":{"build":{"context":"."}}}}`)
		}
		if strings.Contains(command, " build --builder ") {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err := e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("build did not start")
	}
	if _, err := e.Cancel(r); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, _ := e.Status(r.ID)
	if op.State != "cancelled" || op.Build != nil {
		t.Fatal(op)
	}
	for _, path := range []string{filepath.Join(e.Root, "workloads", r.Workload, "pending-build.json"), filepath.Join(e.Root, "workloads", r.Workload, r.ID+".build.yaml"), filepath.Join(e.Root, "build-auth", r.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal(path, err)
		}
	}
}

func TestBuildValidationAndRecoveryOwnership(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	r := Request{ID: "build", Workload: "app", Action: "build", Compose: "services: {}", Build: &BuildOptions{Repository: "team/app"}, Registry: &Registry{Server: "registry.example.com", Username: "fixture", Password: "fixture-password"}}
	if err := ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"", "../app", "team/app:latest", "team/app@sha256:abc", "UPPER/app", strings.Repeat("a", 129)} {
		bad := r
		bad.Build = &BuildOptions{Repository: repository}
		if ValidateRequest(bad) == nil {
			t.Fatal(repository)
		}
	}
	bad := r
	bad.Action = "deploy"
	if ValidateRequest(bad) == nil {
		t.Fatal("deploy accepted build options")
	}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	owner, _ := e.buildOwner()
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	os.MkdirAll(dir, 0700)
	marker := filepath.Join(dir, "pending-build.json")
	writeJSON(marker, pendingBuild{Operation: r.ID, Owner: strings.Repeat("0", 32), Services: []string{"app"}})
	called := false
	e.Run = func(context.Context, string, []string, []string, io.Writer) error { called = true; return nil }
	if e.recoverBuild(context.Background(), r.Workload) == nil || called {
		t.Fatal("foreign owner cleanup allowed")
	}
	writeJSON(marker, pendingBuild{Operation: r.ID, Owner: owner, Services: []string{"app"}})
	var removed []string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[2] == "ls" {
			for _, tag := range []string{buildImageTag(r, "app"), r.Registry.Server + "/" + r.Build.Repository + ":unrelated"} {
				pos := strings.LastIndex(tag, ":")
				json.NewEncoder(out).Encode(map[string]string{"Repository": tag[:pos], "Tag": tag[pos+1:]})
			}
			return nil
		}
		removed = append(removed, args[len(args)-1])
		return nil
	}
	if err := e.recoverBuild(context.Background(), r.Workload); err != nil || len(removed) != 1 || removed[0] != buildImageTag(r, "app") {
		t.Fatal(removed, err)
	}
	j, _ := e.read(r.ID)
	j.Operation.State = "running"
	writeJSON(e.path(r.ID), j)
	os.WriteFile(filepath.Join(dir, r.ID+".build.yaml"), []byte("private input"), 0600)
	if err := e.Recover(); err != nil {
		t.Fatal(err)
	}
	op, _ := e.Status(r.ID)
	if op.State != "interrupted" || !strings.Contains(op.Error, "registry artifacts") {
		t.Fatal(op)
	}
	if _, err := os.Stat(filepath.Join(dir, r.ID+".build.yaml")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
