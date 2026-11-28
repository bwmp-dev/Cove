package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPITRMetadataWorkerOwnershipDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" || os.Geteuid() != 0 {
		t.Skip("disposable root Docker fixture required")
	}
	directory, err := os.MkdirTemp(fixture, "pitr-metadata-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if err = os.Chown(directory, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, "status.json")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read := func() ([]byte, error) {
		var out bytes.Buffer
		err := RunCommand(ctx, fixture, []string{"docker", "run", "--rm", "--network", "none", "--read-only", "--user", "1000:1000", "--mount", "type=bind,source=" + directory + ",target=/state,readonly", "busybox:1.37", "cat", "/state/status.json"}, nil, &out)
		return out.Bytes(), err
	}
	if err = writeJSON(filename, map[string]string{"state": "disabled"}); err != nil {
		t.Fatal(err)
	}
	if _, err = read(); err == nil {
		t.Fatal("root-owned receipt unexpectedly readable by worker")
	}
	for _, state := range []string{"disabled", "capturing"} {
		if err = writePITRMetadata(filename, map[string]string{"state": state}); err != nil {
			t.Fatal(err)
		}
		data, err := read()
		if err != nil || !bytes.Contains(data, []byte(state)) {
			t.Fatalf("worker cannot read helper receipt: %s %v", data, err)
		}
		info, err := os.Lstat(filename)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("receipt lost private permissions", err)
		}
	}
	unsafe := filepath.Join(directory, "alias")
	if err = os.Symlink(directory, unsafe); err != nil {
		t.Fatal(err)
	}
	if err = writePITRMetadata(filepath.Join(unsafe, "status.json"), nil); err == nil {
		t.Fatal("accepted symlink metadata directory")
	}
}

func TestPITRArchiveLockWorkerOwnershipDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" || os.Geteuid() != 0 {
		t.Skip("disposable root Docker fixture required")
	}
	directory, err := os.MkdirTemp(fixture, "pitr-lock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if err = os.Chown(directory, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	workerLock := func() error {
		return RunCommand(ctx, fixture, []string{"docker", "run", "--rm", "--network", "none", "--read-only", "--user", "1000:1000", "--mount", "type=bind,source=" + directory + ",target=/state", "busybox:1.37", "sh", "-ec", "umask 077; exec 9<>/state/worker.lock; flock -n 9"}, nil, &bytes.Buffer{})
	}
	unlock, err := lockWorker(directory)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err = workerLock(); err == nil {
		t.Fatal("worker opened a root-created private archive mutex")
	}
	if err = os.Remove(filepath.Join(directory, "worker.lock")); err != nil {
		t.Fatal(err)
	}
	if err = workerLock(); err != nil {
		t.Fatal("worker could not create its own archive mutex", err)
	}
	unlock, err = lockWorker(directory)
	if err != nil {
		t.Fatal("helper could not use the worker's archive mutex", err)
	}
	if err = workerLock(); err == nil {
		unlock()
		t.Fatal("worker ignored the helper's archive lock")
	}
	unlock()
	if err = workerLock(); err != nil {
		t.Fatal("worker could not acquire the archive mutex after the helper released it", err)
	}
}
