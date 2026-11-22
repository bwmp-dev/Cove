package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPreflightIsReadOnlyAndDoesNotExposeSecrets(t *testing.T) {
	for _, failImage := range []bool{false, true} {
		t.Run(map[bool]string{false: "accessible", true: "denied"}[failImage], func(t *testing.T) {
			root := t.TempDir()
			calls := 0
			e := &Engine{Root: root, Run: func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
				calls++
				joined := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(joined, "docker info "):
					return nil
				case strings.Contains(joined, " config --format json"):
					if !strings.Contains(strings.Join(env, "\n"), "CHECK=private-value") {
						t.Fatal("missing environment")
					}
					_, err := io.WriteString(out, `{"services":{"app":{"image":"busybox:1.37"}}}`)
					return err
				case joined == "docker manifest inspect -- busybox:1.37":
					if failImage {
						return errors.New("private-value registry denial")
					}
					return nil
				default:
					t.Fatalf("unexpected command: %v", args)
					return nil
				}
			}}
			report := e.Preflight(context.Background(), Request{ID: "preflight-test", Workload: "test", Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"CHECK": "private-value"}})
			if calls != 3 {
				t.Fatal(calls, report)
			}
			found := false
			for _, check := range report.Checks {
				if check.Name == "Image: app" {
					found = true
					if (check.State == "error") != failImage {
						t.Fatal(check)
					}
				}
			}
			if !found {
				t.Fatal("image check missing")
			}
			raw, err := json.Marshal(report)
			if err != nil || strings.Contains(string(raw), "private-value") {
				t.Fatal("secret exposed", err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatal("temporary files left behind", files, err)
			}
		})
	}
}

func TestPreflightRejectsInvalidRequestsAndConcurrentInspections(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Run: func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("executed invalid request")
		return nil
	}}
	report := e.Preflight(context.Background(), Request{Action: "stop"})
	if len(report.Checks) != 1 || report.Checks[0].State != "error" {
		t.Fatal(report)
	}
	e.preflightMu.Lock()
	defer e.preflightMu.Unlock()
	report = e.Preflight(context.Background(), Request{})
	if len(report.Checks) != 1 || report.Checks[0].Name != "Preflight" {
		t.Fatal(report)
	}
}

func TestPreflightChecksFileSecretsAndDeploymentGuardWithoutMutating(t *testing.T) {
	for _, secret := range []bool{true, false} {
		e := &Engine{Root: t.TempDir(), Run: func(_ context.Context, _ string, args, env []string, out io.Writer) error {
			command := strings.Join(args, " ")
			if strings.HasPrefix(command, "docker info ") {
				return nil
			}
			if strings.Contains(command, " config --format json") {
				_, err := io.WriteString(out, `{"services":{"app":{"image":"busybox:1.37"}}}`)
				return err
			}
			t.Fatal("preflight continued past invalid safety configuration", args)
			return nil
		}}
		r := Request{ID: "inspect", Workload: "app", Action: "deploy", Compose: "services: {}"}
		enabled := true
		if secret {
			r.FileSecrets = []FileSecret{{Name: "token", Variable: "MISSING", Services: []string{"app"}}}
		} else {
			r.GuardedDeploy = &enabled
		}
		report := e.Preflight(context.Background(), r)
		found := false
		for _, check := range report.Checks {
			if check.State == "error" && (check.Name == "File secrets" || check.Name == "Deployment safety") {
				found = true
			}
		}
		if !found {
			t.Fatal("missing safety failure", report)
		}
		entries, err := os.ReadDir(e.Root)
		if err != nil || len(entries) != 0 {
			t.Fatal("inspection left files", entries, err)
		}
	}
}
