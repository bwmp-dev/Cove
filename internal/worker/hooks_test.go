package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentHookCleanupAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		root := t.TempDir()
		var ran, removed bool
		e := &Engine{Root: root, Run: func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
			command := strings.Join(args, " ")
			if strings.Contains(command, "compose") {
				ran = true
				for _, required := range []string{"run --rm --no-deps --no-TTY", "--label dev.cove.hook=hook-test", "--entrypoint sh app -ec"} {
					if !strings.Contains(command, required) {
						t.Fatal(command)
					}
				}
				if _, err := os.Stat(filepath.Join(root, "pending-hook.json")); err != nil {
					t.Fatal("hook started without recovery marker", err)
				}
				if fail {
					return errors.New("fixture failure")
				}
				return nil
			}
			if strings.Contains(command, "ps -aq") {
				io.WriteString(out, "owned-hook")
				return nil
			}
			if command == "docker rm --force --volumes owned-hook" {
				removed = true
				return nil
			}
			t.Fatal("unexpected", command)
			return nil
		}}
		err := e.runHook(context.Background(), root, "hook-test", []string{"docker", "compose", "--file", "fixture.yaml"}, nil, &DeploymentHook{Service: "app", Command: "echo fixture"}, true, io.Discard)
		if (err != nil) != fail || !ran || !removed {
			t.Fatal(err, ran, removed)
		}
		if _, err = os.Stat(filepath.Join(root, "pending-hook.json")); !os.IsNotExist(err) {
			t.Fatal("cleanup marker retained", err)
		}
	}
}

func TestHookRecoveryKeepsMarkerUntilCleanupSucceeds(t *testing.T) {
	root := t.TempDir()
	if err := writeJSON(filepath.Join(root, "pending-hook.json"), "hook-test"); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Run: func(context.Context, string, []string, []string, io.Writer) error { return errors.New("unreachable") }}
	if err := e.recoverHook(context.Background(), root); err == nil {
		t.Fatal("ignored cleanup failure")
	}
	if _, err := os.Stat(filepath.Join(root, "pending-hook.json")); err != nil {
		t.Fatal("lost recovery marker")
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error { return nil }
	if err := e.recoverHook(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []*DeploymentHook{{Service: "--bad", Command: "true"}, {Service: "app", Command: ""}} {
		if validateHooks(&DeploymentHooks{Before: hook}, "service") == nil {
			t.Fatal("invalid hook accepted")
		}
	}
	if validateHooks(&DeploymentHooks{Before: &DeploymentHook{Service: "app", Command: "true"}}, "job") == nil {
		t.Fatal("job hooks accepted")
	}
}
