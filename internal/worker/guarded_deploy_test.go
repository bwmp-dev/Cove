package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardedDeploymentRecoveryAndCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.Root, "workloads", "app")
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		previous := deployedState{ComposePath: filepath.Join(dir, "old.compose.json"), Environment: map[string]string{"TOKEN": "previous-secret"}}
		if err = writeJSON(filepath.Join(dir, "current.json"), previous); err != nil {
			t.Fatal(err)
		}
		if err = e.beginDeployGuard(Request{ID: "new", Workload: "app"}, dir, "candidate.compose.json"); err != nil {
			t.Fatal(err)
		}
		if committed {
			if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: filepath.Join(dir, "release-states", "new.compose.json")}); err != nil {
				t.Fatal(err)
			}
		}
		restored := false
		fail := true
		e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
			if strings.Contains(strings.Join(args, " "), "old.compose.json up") {
				if fail {
					return errors.New("worker unavailable")
				}
				restored = true
				if len(env) != 1 || env[0] != "TOKEN=previous-secret" {
					t.Fatal("old environment lost")
				}
			}
			return nil
		}
		err = e.recoverDeployGuard(context.Background(), "app", io.Discard)
		if committed {
			if err != nil || restored {
				t.Fatal("committed release rolled back", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("recovery failure ignored")
		}
		if _, err = os.Stat(filepath.Join(dir, "deploy-guard.json")); err != nil {
			t.Fatal("recovery marker lost")
		}
		fail = false
		if err = e.recoverDeployGuard(context.Background(), "app", io.Discard); err != nil || !restored {
			t.Fatal("previous release not recovered", err)
		}
	}
}

func TestGuardedDeploymentRejectsStateAndMissingHealth(t *testing.T) {
	for _, body := range []string{`{"services":{"app":{"image":"busybox"}}}`, `{"services":{"app":{"healthcheck":{"test":["NONE"]}}}}`, `{"services":{"app":{"volumes":["data:/data"],"healthcheck":{"test":["CMD","true"]}}}}`, `{"services":{"app":{"ports":["80:80"],"healthcheck":{"test":["CMD","true"]}}}}`} {
		var config map[string]any
		if err := json.Unmarshal([]byte(body), &config); err != nil {
			t.Fatal(err)
		}
		if _, err := guardedService(config); err == nil {
			t.Fatal("unsafe candidate accepted", body)
		}
	}
}
