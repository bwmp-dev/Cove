package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFileRequestBoundaries(t *testing.T) {
	for _, p := range []string{"/a\x01file", "/a\x7ffile", "/a\u0085file"} {
		if ValidateFileRequest(FileRequest{Action: "read", Kind: "container", Target: strings.Repeat("a", 12), Path: p}) == nil {
			t.Fatalf("control character accepted: %q", p)
		}
	}
	for _, p := range []string{"../file", "/../file", "/a/../file", "/a/./file", "//file", "/a/", "/a\\b", "/a\nfile", "/proc/1/environ", "/run/secrets/password"} {
		if ValidateFileRequest(FileRequest{Action: "read", Kind: "container", Target: strings.Repeat("a", 12), Path: p}) == nil {
			t.Fatalf("unsafe path accepted: %q", p)
		}
	}
	big := make([]byte, MaxFileBytes+1)
	if ValidateFileRequest(FileRequest{Action: "write", Kind: "volume", Target: "data", Path: "/file", Data: &big}) == nil {
		t.Fatal("oversized upload accepted")
	}
	if ValidateFileRequest(FileRequest{Action: "delete", Kind: "volume", Target: "data", Path: "/"}) == nil {
		t.Fatal("root deletion accepted")
	}
	if err := ValidateFileRequest(FileRequest{Action: "list", Kind: "volume", Target: "data", Path: "/"}); err != nil {
		t.Fatal(err)
	}
}

func TestFileBrowserDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_FILES_DOCKER") != "1" && os.Getenv("COVE_DATABASE_FIXTURE") == "" {
		t.Skip("COVE_TEST_FILES_DOCKER=1 required; creates disposable Docker resources")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workload := fmt.Sprintf("files-%d", time.Now().UnixNano())
	volume, name := workload+"-data", "cove-test-"+workload
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("volume", "create", "--label", "com.docker.compose.project=cove-"+workload, volume)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", volume).CombinedOutput(); err != nil {
			t.Errorf("volume cleanup failed: %v %s", err, out)
		}
	})
	foreign := volume + "-foreign"
	run("volume", "create", "--label", "com.docker.compose.project=cove-other", foreign)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", foreign).CombinedOutput(); err != nil {
			t.Errorf("foreign fixture cleanup failed: %v %s", err, out)
		}
	})
	run("run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-"+workload, "--mount", "type=volume,source="+volume+",target=/data", "--mount", "type=volume,source="+foreign+",target=/foreign", "alpine:3.21", "sh", "-ec", "printf original > /data/hello.txt; chown 1234:1234 /data/hello.txt; chmod 640 /data/hello.txt; ln -s /etc/passwd /data/link; mkdir /data/empty; touch /tmp/ready; sleep 90")
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("container cleanup failed: %v %s", err, out)
		}
	})
	run("exec", name, "sh", "-ec", "until test -f /tmp/ready; do sleep 0.1; done")
	e := &Engine{Root: t.TempDir(), Run: RunCommand}
	targets, err := e.FileOperation(ctx, workload, FileRequest{Action: "targets"})
	if err != nil {
		t.Fatal(err)
	}
	container := ""
	for _, target := range targets.Targets {
		if target.Kind == "container" {
			container = target.ID
		}
	}
	if container == "" || len(targets.Targets) != 2 {
		t.Fatal("missing owned targets", targets.Targets)
	}
	if _, err = e.FileOperation(ctx, workload, FileRequest{Action: "list", Kind: "volume", Target: foreign, Path: "/"}); err == nil {
		t.Fatal("foreign volume accepted")
	}
	r := FileRequest{Action: "read", Kind: "container", Target: container, Path: "/data/hello.txt"}
	original, err := e.FileOperation(ctx, workload, r)
	if err != nil || string(original.Data) != "original" {
		t.Fatal("file read failed", err)
	}
	updated := []byte("updated\n")
	inode := run("exec", name, "stat", "-c", "%d:%i", "/data/hello.txt")
	r.Action = "write"
	r.Data = &updated
	r.Revision = original.Revision
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal("file write failed", err)
	}
	if metadata := run("exec", name, "stat", "-c", "%u:%g:%a", "/data/hello.txt"); metadata != "1234:1234:640" {
		t.Fatal("file metadata changed", metadata)
	}
	if current := run("exec", name, "stat", "-c", "%d:%i", "/data/hello.txt"); current != inode {
		t.Fatal("existing file inode replaced", inode, current)
	}
	if _, err = e.FileOperation(ctx, workload, r); err == nil {
		t.Fatal("stale revision accepted")
	}
	r.Action = "read"
	r.Path = "/data/link"
	r.Data = nil
	if _, err = e.FileOperation(ctx, workload, r); err == nil {
		t.Fatal("symlink followed")
	}
	binary := []byte{0, 1, 2, 255}
	r = FileRequest{Action: "write", Kind: "container", Target: container, Path: "/data/binary.bin", Data: &binary}
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal("binary upload failed", err)
	}
	r.Action = "read"
	r.Data = nil
	read, err := e.FileOperation(ctx, workload, r)
	if err != nil || !bytes.Equal(read.Data, binary) {
		t.Fatal("binary download failed", err)
	}
	r.Action = "delete"
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal("file deletion failed", err)
	}
	empty := []byte{}
	r = FileRequest{Action: "write", Kind: "container", Target: container, Path: "/data/literal;name", Data: &empty}
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal("empty file creation failed", err)
	}
	r.Action = "read"
	r.Data = nil
	if read, err := e.FileOperation(ctx, workload, r); err != nil || len(read.Data) != 0 || len(read.Revision) != 64 {
		t.Fatal("empty file read failed", err)
	}
	r.Action = "delete"
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal("literal filename deletion failed", err)
	}
	mutationContext, disconnect := context.WithCancel(ctx)
	originalRunner := e.Run
	e.Run = func(operationContext context.Context, dir string, args, env []string, out io.Writer) error {
		disconnect()
		return originalRunner(operationContext, dir, args, env, out)
	}
	r = FileRequest{Action: "write", Kind: "container", Target: container, Path: "/data/disconnected", Data: &binary}
	_, err = e.FileOperation(mutationContext, workload, r)
	e.Run = originalRunner
	if err != nil {
		t.Fatal("accepted upload did not finish after caller disconnect", err)
	}
	r.Action = "read"
	r.Data = nil
	if saved, err := e.FileOperation(ctx, workload, r); err != nil || !bytes.Equal(saved.Data, binary) {
		t.Fatal("disconnected upload was truncated", err)
	}
	r.Action = "delete"
	if _, err = e.FileOperation(ctx, workload, r); err != nil {
		t.Fatal(err)
	}
	volumeRequest := FileRequest{Action: "write", Kind: "volume", Target: volume, Path: "/hello.txt", Data: &updated, Revision: original.Revision}
	if _, err = e.FileOperation(ctx, workload, volumeRequest); err == nil {
		t.Fatal("live volume changed")
	}
	run("stop", "--time", "1", name)
	volumeRequest.Action = "read"
	volumeRequest.Data = nil
	current, err := e.FileOperation(ctx, workload, volumeRequest)
	if err != nil || string(current.Data) != "updated\n" {
		t.Fatal("volume read failed", err)
	}
	volumeRequest.Action = "write"
	volumeRequest.Data = &binary
	volumeRequest.Revision = current.Revision
	if _, err = e.FileOperation(ctx, workload, volumeRequest); err != nil {
		t.Fatal("stopped volume write failed", err)
	}
	volumeRequest.Action = "list"
	volumeRequest.Path = "/"
	volumeRequest.Data = nil
	listed, err := e.FileOperation(ctx, workload, volumeRequest)
	if err != nil || len(listed.Entries) != 3 {
		t.Fatal("volume listing failed", err)
	}
	volumeRequest.Action = "delete"
	volumeRequest.Path = "/empty"
	if _, err = e.FileOperation(ctx, workload, volumeRequest); err != nil {
		t.Fatal("empty directory deletion failed", err)
	}
}
