package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var scanCacheName = regexp.MustCompile(`^db-[a-f0-9]{64}$`)

func (e *Engine) scanDatabase(ctx context.Context, owner, operation, scannerImage string, proxy ...*outbound.Config) (result scanDatabaseCache, err error) {
	root := filepath.Join(e.Root, "scanner")
	if err := os.MkdirAll(root, 0700); err != nil {
		return scanDatabaseCache{}, err
	}
	var saved scanDatabaseCache
	data, readErr := os.ReadFile(filepath.Join(root, "current.json"))
	if readErr == nil && len(data) <= 16384 && json.Unmarshal(data, &saved) == nil && scanCacheName.MatchString(saved.Path) {
		db, err := validateScanDatabase(filepath.Join(root, saved.Path))
		if err == nil && db.Digest == saved.Digest {
			return saved, nil
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return saved, err
	}
	for _, entry := range entries {
		if entry.Name() == saved.Path {
			continue
		}
		if scanCacheName.MatchString(entry.Name()) || strings.HasPrefix(entry.Name(), "candidate-") {
			if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return saved, errors.New("old scanner database cache could not be removed")
			}
		}
	}
	candidate, err := os.MkdirTemp(root, "candidate-")
	if err != nil {
		return saved, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(candidate)) }()
	db, err := e.refreshScanDatabase(ctx, owner, operation, candidate, scannerImage, proxy...)
	if err != nil {
		return saved, err
	}
	if err = os.Chmod(filepath.Join(candidate, "db", "trivy.db"), 0444); err == nil {
		err = os.Chmod(filepath.Join(candidate, "db", "metadata.json"), 0444)
	}
	if err == nil {
		err = os.Chmod(filepath.Join(candidate, "db"), 0555)
	}
	if err != nil {
		return saved, err
	}
	name := "db-" + strings.TrimPrefix(db.Digest, "sha256:")
	if name == saved.Path {
		return saved, errors.New("refreshed scanner database is still stale")
	}
	if err = os.Rename(candidate, filepath.Join(root, name)); err != nil {
		return saved, err
	}
	current := scanDatabaseCache{ScanDatabase: db, Path: name}
	if err = writeJSON(filepath.Join(root, "current.json"), current); err != nil {
		return saved, err
	}
	if scanCacheName.MatchString(saved.Path) {
		if err = os.RemoveAll(filepath.Join(root, saved.Path)); err != nil {
			return current, errors.New("previous scanner database cache cleanup failed")
		}
	}
	return current, nil
}
