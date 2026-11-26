package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
)

func (s *workloadRemovalState) preflightCleanup(ctx context.Context) error {
	check := func(path string) error {
		directory, err := s.root.Open(path)
		if err != nil {
			return err
		}
		defer directory.Close()
		return removalDirectoryAccess(directory)
	}
	if len(s.keep) == 0 {
		if err := check(filepath.Dir(s.path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return fs.WalkDir(s.root.FS(), filepath.ToSlash(s.path), func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if s.retainedRoots[filepath.FromSlash(path)] {
				return fs.SkipDir
			}
			return check(filepath.FromSlash(path))
		}
		return nil
	})
}
