package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildPublishDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_BUILD_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("set COVE_TEST_BUILD_DOCKER=1 in an isolated Linux Docker fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("build-%x", time.Now().UnixNano())
	registry := "cove-" + id + "-registry"
	app := "cove-" + id + "-app"
	base := "cove-" + id + "-base"
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, name := range []string{registry, app, base} {
			e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard)
		}
		if owner, err := e.buildOwner(); err == nil {
			e.Run(cleanup, e.Root, []string{"docker", "buildx", "rm", "--force", "cove-" + owner}, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, io.Discard)
		}
	})
	command := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := e.Run(ctx, e.Root, args, nil, &out); err != nil {
			t.Fatal(args, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	command("docker", "run", "-d", "--name", registry, "-p", "127.0.0.1::5000", "registry:2.8.3")
	port := command("docker", "port", registry, "5000/tcp")
	port = port[strings.LastIndex(port, ":")+1:]
	command("docker", "run", "-d", "--name", app, "busybox:1.37", "sleep", "300")
	before := command("docker", "inspect", "--format", "{{.Id}} {{.State.Running}}", app)
	dir := filepath.Join(e.Root, "workloads", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	command("docker", "create", "--name", base, "busybox:1.37")
	command("docker", "export", "--output", filepath.Join(dir, "rootfs.tar"), base)
	secret := "build-mount-private-fixture-value"
	dockerfile := "FROM scratch\nADD rootfs.tar /\nRUN --mount=type=secret,id=TOKEN [\"/bin/busybox\",\"sh\",\"-c\",\"/bin/busybox test -s /run/secrets/TOKEN && /bin/busybox cat /run/secrets/TOKEN && /bin/busybox echo built > /artifact\"]\nENTRYPOINT [\"/bin/busybox\"]\n"
	if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep generated Compose files and credentials outside the build context sent to BuildKit.
	if err = os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("*\n!rootfs.tar\n!Dockerfile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current := []byte(`{"release":"untouched","compose_path":"production.yaml"}`)
	if err = os.WriteFile(filepath.Join(dir, "current.json"), current, 0600); err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "publish", Workload: id, Action: "build", Compose: "services:\n  app:\n    build: .\n  profiled:\n    build: .\n    profiles: [optional]\n  untouched:\n    image: busybox:1.37\n", Build: &BuildOptions{Repository: "fixture/app", NoCache: true}, Registry: &Registry{Server: "localhost:" + port, Username: "fixture", Password: "local-fixture-password"}, Environment: map[string]string{"BUILD_TOKEN": secret}, FileSecrets: []FileSecret{{Name: "TOKEN", Variable: "BUILD_TOKEN", BuildServices: []string{"app", "profiled"}}}}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status(r.ID)
	logs, _ := e.Logs(r.ID)
	if err != nil || op.State != "succeeded" || op.Build == nil || len(op.Build.Images) != 2 {
		t.Fatal(op, err, logs)
	}
	if strings.Contains(logs, secret) || !strings.Contains(logs, "[redacted]") || !strings.Contains(logs, "Published app") {
		t.Fatal("build log missing/redaction failed", logs)
	}
	image := op.Build.Images[0]
	if op.Build.Images[1].Service != "profiled" {
		t.Fatal("profiled service was not built", op.Build)
	}
	command("docker", "pull", image.Reference)
	if id := command("docker", "image", "inspect", "--format", "{{.Id}}", image.Reference); id != image.ImageID {
		t.Fatal(id, image)
	}
	if content := command("docker", "run", "--rm", image.Reference, "cat", "/artifact"); content != "built" {
		t.Fatal(content)
	}
	command("docker", "run", "--rm", image.Reference, "test", "!", "-e", "/run/secrets/TOKEN")
	if history := command("docker", "history", "--no-trunc", image.Reference); strings.Contains(history, secret) {
		t.Fatal("secret in image history")
	}
	if after := command("docker", "inspect", "--format", "{{.Id}} {{.State.Running}}", app); after != before {
		t.Fatal("application changed", before, after)
	}
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil || !bytes.Equal(data, current) {
		t.Fatal("release changed", err)
	}
	for _, path := range []string{filepath.Join(dir, "pending-build.json"), filepath.Join(dir, r.ID+".build.yaml"), filepath.Join(dir, r.ID+".secrets.compose.json"), filepath.Join(e.Root, "file-secrets", id, r.ID)} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("temporary input retained", path, err)
		}
	}
	if tags := command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"); strings.Contains(tags, buildImageTag(r, "app")) || strings.Contains(tags, buildImageTag(r, "profiled")) {
		t.Fatal("temporary build tag retained")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "image", "rm", image.Reference}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
}
