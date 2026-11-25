package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutableWorkerDispatch(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cove-worker")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	for command, arguments := range map[string]int{
		"healthcheck": 1, "tunnel": 1, "snapshot-reap": 1, "build-recipe-output": 1, "buildpacks-helper": 1,
		"build-recipe-plan": 0, "build-recipe-egress": 0, "static-site": 0, "build-cache-filesystem": 0,
		"firewall-revert": 2, "firewall-restore": 2, "host-security-run": 3, "firewall-revert-attempt": 3,
		"probe": 0, "rpc": 0, "daemon": 0, "ingress-prepare": 0,
		"volume-copy": 0, "volume-free": 0, "volume-digest": 0,
		"link-gateway": 0, "idle-gateway": 0, "route-helper": 0,
		"snapshot-host": 0, "host-gpus": 0, "host-firewall": 0, "host-maintenance": 0,
		"pitr-fetch": 0, "pitr-forget": 0, "pitr-base": 0, "pitr-runner": 0,
		"cloudflare-origin": 0, "cloudflare-origin-probe": 0, "cloudflare-connector-origin-probe": 0,
	} {
		t.Run(command, func(t *testing.T) {
			for count := 0; count <= arguments+1; count++ {
				if count == arguments {
					continue
				}
				args := append([]string{command}, strings.Fields(strings.Repeat("invalid ", count))...)
				output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(output), "usage:") || strings.Contains(string(output), "panic:") {
					t.Fatalf("%v: %v\n%s", args, err, output)
				}
			}
		})
	}
	t.Run("cloudflare probe arity", func(t *testing.T) {
		for count := 0; count <= 3; count++ {
			args := append([]string{"cloudflare-probe"}, strings.Fields(strings.Repeat("invalid ", count))...)
			output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
			var exit *exec.ExitError
			want := 2
			if count == 1 || count == 2 {
				want = 1
			}
			if !errors.As(err, &exit) || exit.ExitCode() != want || strings.Contains(string(output), "panic:") {
				t.Fatalf("%v: %v\n%s", args, err, output)
			}
		}
	})
	t.Run("certificate probe optional file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "probes.json")
		if err := os.WriteFile(file, []byte("[]"), 0600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.CommandContext(ctx, binary, "certificate-probe", file).CombinedOutput(); err != nil {
			t.Fatalf("probe file dispatch: %v %s", err, output)
		}
		output, err := exec.CommandContext(ctx, binary, "certificate-probe", file, "extra").CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(output), "usage:") {
			t.Fatalf("probe arity: %v %s", err, output)
		}
	})
	t.Run("scheduled host helper dispatch", func(t *testing.T) {
		output, err := exec.CommandContext(ctx, binary, "host-security-run", "relative-root", strings.Repeat("a", 32), "apt").CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Contains(string(output), "usage:") || strings.Contains(string(output), "panic:") || len(output) == 0 {
			t.Fatalf("native host helper was not reached: %v %s", err, output)
		}
	})
	t.Run("missing command", func(t *testing.T) {
		output, err := exec.CommandContext(ctx, binary).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || strings.Contains(string(output), "panic:") {
			t.Fatalf("%v\n%s", err, output)
		}
	})
	t.Run("valid health check", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/ready" {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()
		output, err := exec.CommandContext(ctx, binary, "healthcheck", server.URL+"/ready").CombinedOutput()
		if err != nil || requests.Load() != 1 {
			t.Fatalf("health command did not reach the service: %v, requests %d\n%s", err, requests.Load(), output)
		}
	})
}
