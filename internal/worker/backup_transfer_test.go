package worker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupDownloadDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workload := "download-" + filepath.Base(fixture)
	root := filepath.Join(fixture, "download")
	e := &Engine{Root: root, Run: RunCommand}
	run := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args[:min(len(args), 3)], err, out)
		}
		return strings.TrimSpace(out)
	}
	if err := os.MkdirAll(filepath.Join(root, "workloads", workload), 0700); err != nil {
		t.Fatal(err)
	}
	name, volume := "cove-"+workload, "cove-data-"+workload
	run("docker", "volume", "create", "--label", "cove.test=true", volume)
	t.Cleanup(func() {
		cleanup := context.Background()
		e.Run(cleanup, root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard)
		if err := e.Run(cleanup, root, []string{"docker", "volume", "rm", volume, "cove-backups-" + workload}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	run("docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=app", "--volume", volume+":/data", "busybox:1.37", "sh", "-c", "while true; do sleep 1; done")
	run("docker", "exec", name, "sh", "-ec", `head -c 70000 /dev/zero | tr '\000' x > /data/marker`)
	compose := filepath.Join(root, "workloads", workload, "compose.json")
	if err := writeJSON(compose, map[string]any{"services": map[string]any{"app": map[string]any{"image": "busybox:1.37"}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "workloads", workload, "current.json"), deployedState{ComposePath: compose, Environment: map[string]string{"SYNTHETIC_SECRET": "metadata-must-not-be-exported"}}); err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "backup", Workload: workload, Action: "backup", Backup: &BackupConfig{Repository: "local", Password: "synthetic-download-password", KeepLast: 2}}
	if err := e.performBackup(ctx, r, io.Discard); err != nil {
		t.Fatal(err)
	}
	r.ID, r.Action, r.BackupID = "download", "restore", "backup"
	file, meta, cleanup, err := e.backupDownload(ctx, r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	n, err := io.Copy(digest, file)
	if err != nil || n != meta.Size || hex.EncodeToString(digest.Sum(nil)) != meta.SHA256 {
		t.Fatal("download integrity metadata mismatch", err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(file)
	marker := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(header.Name, "manifest") {
			t.Fatal("release metadata included in plaintext download")
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		contents, err := io.ReadAll(archive)
		if err != nil || !strings.HasSuffix(header.Name, "/marker") || string(contents) != strings.Repeat("x", 70000) {
			t.Fatal("unexpected exported file", header.Name, err)
		}
		marker = true
	}
	if !marker {
		t.Fatal("volume data missing from download")
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = e.backupDownload(ctx, r, 1024); err == nil {
		t.Fatal("download bypassed configured limit")
	}
	if _, err = os.Stat(filepath.Join(root, "backup-transfer")); !os.IsNotExist(err) {
		t.Fatal("failed transfer retained plaintext staging", err)
	}
	if run("docker", "inspect", "--format", "{{.State.Running}}", name) != "true" {
		t.Fatal("download stopped the source")
	}
}
