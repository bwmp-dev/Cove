package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFileStreamDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_FILES_DOCKER") != "1" && os.Getenv("COVE_DATABASE_FIXTURE") == "" {
		t.Skip("disposable Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workload := fmt.Sprintf("files-stream-%d", time.Now().UnixNano())
	name, volume := "cove-test-"+workload, workload+"-data"
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
			t.Error(err, string(out))
		}
	})
	id := run("run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-"+workload, "--mount", "type=volume,source="+volume+",target=/data", "alpine:3.21", "sleep", "300")
	id = id[:12]
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Error(err, string(out))
		}
	})
	e := &Engine{Root: t.TempDir(), Run: RunCommand}
	payload := bytes.Repeat([]byte{0, 255, 13, 10, 'a'}, 400000)
	digest := sha256.Sum256(payload)
	revision := hex.EncodeToString(digest[:])
	files := FileRequest{Kind: "container", Target: id, Path: "/data/large.bin"}
	transfer := func(action string, body []byte, size, limit int64) (*httptest.ResponseRecorder, error) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(ctx)
		e.serveFileStream(w, r, &StreamRequest{Action: action, Workload: workload, Files: &files, Size: size, Limit: limit}, r.Body)
		_, _, err := ReadStreamResponse(bytes.NewReader(w.Body.Bytes()))
		return w, err
	}
	if _, err := transfer("file_upload", payload, int64(len(payload)), 4<<20); err != nil {
		t.Fatal("streamed upload", err)
	}
	stat, err := e.FileOperation(ctx, workload, FileRequest{Action: "stat", Kind: files.Kind, Target: id, Path: files.Path})
	if err != nil || stat.Size != int64(len(payload)) || stat.Revision != revision {
		t.Fatal("metadata", stat, err)
	}
	w, err := transfer("file_download", nil, 0, 4<<20)
	if err != nil {
		t.Fatal("download", err)
	}
	meta, body, err := ReadStreamResponse(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(actual, payload) || meta.SHA256 != revision {
		t.Fatal("streamed bytes changed", err)
	}
	files.Revision = strings.Repeat("a", 64)
	if _, err := transfer("file_upload", []byte("wrong"), 5, 4<<20); err == nil {
		t.Fatal("stale revision accepted")
	}
	files.Revision = revision
	if _, err := transfer("file_upload", []byte("short"), 100, 4<<20); err == nil {
		t.Fatal("truncated upload accepted")
	}
	if run("exec", name, "sha256sum", files.Path) != revision+"  "+files.Path {
		t.Fatal("failed upload changed existing file")
	}
	if _, err := transfer("file_download", nil, 0, 1<<20); err == nil {
		t.Fatal("oversized download accepted")
	}
	run("exec", name, "ln", "-s", files.Path, "/data/link")
	files.Path = "/data/link"
	if _, err := transfer("file_download", nil, 0, 4<<20); err == nil {
		t.Fatal("symlink followed")
	}
	files.Path = "/data/empty"
	files.Revision = ""
	if _, err := transfer("file_upload", nil, 0, 4<<20); err != nil {
		t.Fatal("empty upload", err)
	}
	if _, err := transfer("file_download", nil, 0, 4<<20); err != nil {
		t.Fatal("empty download", err)
	}
	files.Kind, files.Target, files.Path = "volume", volume, "/volume.bin"
	if _, err := transfer("file_upload", payload, int64(len(payload)), 4<<20); err == nil {
		t.Fatal("running volume changed")
	}
	run("stop", "--time", "1", name)
	if _, err := transfer("file_upload", payload, int64(len(payload)), 4<<20); err != nil {
		t.Fatal("stopped volume upload", err)
	}
	if _, err := transfer("file_download", nil, 0, 4<<20); err != nil {
		t.Fatal("volume download", err)
	}
}
