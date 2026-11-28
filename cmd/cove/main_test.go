package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecutableCommandDispatch(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cove")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build command fixture: %v\n%s", err, output)
	}
	t.Setenv("COVE_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("DATABASE_URL", "postgres://%zz")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	t.Setenv("COVE_ENCRYPTION_KEY", key)
	t.Setenv("COVE_PANEL_PLAN_KEY", "")
	invoke := func(t *testing.T, input []byte, args ...string) ([]byte, string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if ctx.Err() != nil {
			t.Fatal("command timed out", args)
		}
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		return stdout.Bytes(), stderr.String(), code
	}
	failure := func(t *testing.T, input []byte, expected string, args ...string) {
		t.Helper()
		stdout, stderr, code := invoke(t, input, args...)
		if code == 0 || !strings.Contains(stderr, expected) || len(stdout) != 0 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
	}
	t.Run("server without arguments", func(t *testing.T) {
		failure(t, nil, "invalid database configuration")
	})
	t.Run("connect special handler", func(t *testing.T) {
		t.Setenv("COVE_ENCRYPTION_KEY", "not-base64")
		failure(t, nil, "invalid host or port", "connect", "--host", "localhost", "--ssh-port", "0")
		failure(t, nil, "Usage of connect", "connect", "--help")
	})
	t.Run("CLI help and globals", func(t *testing.T) {
		t.Setenv("COVE_ENCRYPTION_KEY", "not-base64")
		for _, flag := range []string{"help", "--help", "-h"} {
			stdout, stderr, code := invoke(t, nil, flag, "--json")
			if code != 0 || !bytes.Contains(stdout, []byte(`"commands"`)) || stderr != "" {
				t.Fatalf("%s: code=%d stdout=%s stderr=%s", flag, code, stdout, stderr)
			}
		}
		stdout, stderr, code := invoke(t, nil, "context", "list", "--json")
		if code != 0 || strings.TrimSpace(string(stdout)) != "[]" || stderr != "" {
			t.Fatalf("context list: code=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
		stdout, stderr, code = invoke(t, nil, "deploy", "--context", "missing", "--json")
		if code != 1 || !bytes.Contains(stdout, []byte(`"error"`)) || stderr != "" {
			t.Fatalf("missing CLI context: code=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
	})
	plan := []byte(`{"kind":"cove.panel-backup.v1","job_id":"dispatch-fixture","agent_id":"fixture-agent","actor_id":"fixture","action":"backup","config":{"enabled":true,"interval_hours":24,"keep_last":2,"keep_days":30,"max_bytes":67108864,"storage_id":""}}`)
	for _, local := range []bool{false, true} {
		name := "panel encryption key"
		if local {
			name = "local checkpoint key"
		}
		t.Run(name, func(t *testing.T) {
			if local {
				t.Setenv("COVE_ENCRYPTION_KEY", "")
				t.Setenv("COVE_PANEL_PLAN_KEY", key)
			}
			sealed, stderr, code := invoke(t, plan, "panel-backup-plan", "seal")
			var envelope map[string]string
			if code != 0 || stderr != "" || json.Unmarshal(sealed, &envelope) != nil || envelope["sealed"] == "" || bytes.Contains(sealed, []byte("dispatch-fixture")) {
				t.Fatalf("seal: code=%d stdout=%s stderr=%s", code, sealed, stderr)
			}
			opened, stderr, code := invoke(t, sealed, "panel-backup-plan", "open")
			var result struct {
				Plan map[string]any `json:"plan"`
			}
			var expected map[string]any
			if json.Unmarshal(plan, &expected) != nil {
				t.Fatal("invalid test plan")
			}
			if code != 0 || stderr != "" || json.Unmarshal(opened, &result) != nil {
				t.Fatalf("open: code=%d stdout=%s stderr=%s", code, opened, stderr)
			}
			// Optional fields gain empty defaults when the validated plan is encoded.
			for k, value := range expected {
				if !reflect.DeepEqual(result.Plan[k], value) {
					t.Fatalf("changed recovered %s", k)
				}
			}
			wrong := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
			if local {
				t.Setenv("COVE_PANEL_PLAN_KEY", wrong)
			} else {
				t.Setenv("COVE_ENCRYPTION_KEY", wrong)
			}
			failure(t, sealed, "panel recovery plan validation failed", "panel-backup-plan", "open")
		})
	}
	t.Run("host command validation", func(t *testing.T) {
		failure(t, nil, "use panel-backup-plan seal or open", "panel-backup-plan")
		failure(t, nil, "use panel-backup-plan seal or open", "panel-backup-plan", "invalid")
		failure(t, []byte(`{}`), "panel recovery plan validation failed", "panel-backup-plan", "seal")
		t.Setenv("COVE_PANEL_PLAN_KEY", "not-base64")
		failure(t, plan, "invalid local panel plan key", "panel-backup-plan", "seal")
	})
}
