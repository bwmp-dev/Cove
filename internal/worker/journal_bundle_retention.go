package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

func (e *Engine) pruneOperationComposeBundle(ctx context.Context, candidate JournalCandidate) error {
	root, err := os.OpenRoot(e.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	snapshots, err := operationComposeSnapshots(root, candidate)
	if err != nil {
		return err
	}
	relative := filepath.Join("workloads", candidate.Workload, "compose-bundles", candidate.ID)
	info, err := root.Lstat(relative)
	if os.IsNotExist(err) {
		return removeOperationComposeSnapshots(root, snapshots)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("operation Compose bundle is not an owned directory")
	}
	count, total := 0, int64(0)
	err = fs.WalkDir(root.FS(), filepath.ToSlash(relative), func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count++
		if count > 16384 {
			return errors.New("operation Compose bundle exceeds its pruning entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in operation Compose bundle")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if !info.Mode().IsRegular() || total > 64<<20 {
			return errors.New("operation Compose bundle exceeds its pruning file or byte limits")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := root.RemoveAll(relative); err != nil {
		return err
	}
	return removeOperationComposeSnapshots(root, snapshots)
}

func operationComposeSnapshotNames(id string) []string {
	names := []string{"builtin-" + id + ".json"}
	for _, suffix := range []string{"bundle", "recipe", "environment-preview", "job", "limits", "promotion", "secrets", "preview", "generation", "guarded-replacement", "rollback-input"} {
		names = append(names, id+"."+suffix+".compose.json")
	}
	return names
}

func operationComposeSnapshots(root *os.Root, candidate JournalCandidate) ([]string, error) {
	paths := []string{}
	for _, name := range operationComposeSnapshotNames(candidate.ID) {
		path := filepath.Join("workloads", candidate.Workload, name)
		info, err := root.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("operation Compose snapshot is not a regular file")
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func removeOperationComposeSnapshots(root *os.Root, paths []string) error {
	for _, path := range paths {
		if err := root.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
