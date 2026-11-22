package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func ValidateImportObjectKey(key string) error {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.IndexFunc(key, unicode.IsControl) >= 0 || strings.ContainsAny(key, "\\:") || strings.HasPrefix(key, "/") || path.Clean(key) != key || key == "." {
		return errors.New("use an object key relative to the configured S3 path without parent traversal or control characters")
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." || part == "." {
			return errors.New("object keys cannot leave the configured storage path")
		}
	}
	return nil
}

func (e *Engine) readS3Import(ctx context.Context, r Request, storage StorageLocation, key, stage string, out io.Writer, limit int64) (int64, error) {
	if storage.Protocol != "s3" || ValidateStorage(&storage) != nil || ValidateImportObjectKey(key) != nil {
		return 0, errors.New("configured S3 location and relative object key required")
	}
	if err := e.remoteBackupHelper(ctx, io.Discard); err != nil {
		return 0, err
	}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return 0, err
	}
	env, err := storageEnvironment(&storage)
	if err != nil {
		return 0, err
	}
	mounts, err := storageMount(stage, &storage)
	if err != nil {
		return 0, err
	}
	run := func(action string, output io.Writer, command ...string) (result error) {
		name := "cove-s3-import-" + r.ID + "-" + action
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			result = errors.Join(result, e.removeBackupHelper(cleanup, owner, name, r.ID))
		}()
		args := []string{"docker", "run", "--rm", "--log-driver", "none", "--name", name, "--label", "dev.cove.operation=" + r.ID, "--label", "dev.cove.backup-owner=" + owner, "--network", "host", "--read-only", "--tmpfs", "/tmp"}
		args = append(args, mounts...)
		for _, value := range env {
			key, _, _ := strings.Cut(value, "=")
			args = append(args, "-e", key)
		}
		args = append(args, remoteBackupImage, "rclone", "--contimeout", "10s", "--timeout", "1m", "--retries", "1", "--low-level-retries", "1")
		return runInput(ctx, e.Root, append(args, command...), env, nil, output)
	}
	object := "cove:" + path.Join(strings.TrimPrefix(storage.Path, "/"), key)
	metadata := &limitedBuffer{Limit: 32768}
	if err = run("stat", metadata, "lsjson", "--stat", object); err != nil {
		return 0, errors.New("S3 object metadata unavailable")
	}
	var info struct {
		Size  int64
		IsDir bool
	}
	if json.Unmarshal(metadata.data.Bytes(), &info) != nil || info.IsDir || info.Size < 1 || info.Size > limit {
		return 0, errors.New("S3 object is empty, not a file or exceeds transfer limit")
	}
	bounded := &sizeBoundWriter{Writer: out, remaining: info.Size}
	if err = run("read", bounded, "cat", object); err != nil || bounded.remaining != 0 {
		return 0, errors.New("S3 dump transfer was truncated or changed size")
	}
	return info.Size, nil
}
