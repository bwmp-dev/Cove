package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type WorkloadRemovalResult struct {
	WorkerFilesRetainedReason string `json:"worker_files_retained_reason,omitempty"`
	WorkerFilesRetained       bool   `json:"worker_files_retained"`
	CleanupWarning            string `json:"cleanup_warning,omitempty"`
}

type workloadRemovalState struct {
	retainedRoots map[string]bool
	metadataBytes int64
	result        WorkloadRemovalResult
	keep          map[string]bool
	images        map[string]bool
	path          string
	root          *os.Root
}

func (s *workloadRemovalState) uncertain(reason string) {
	s.result.WorkerFilesRetained = true
	s.result.CleanupWarning = "Worker files and images were kept because " + reason + "."
}

func (s *workloadRemovalState) readJSON(path string, value any) error {
	if err := s.validatePath(path); err != nil {
		return err
	}
	f, err := s.root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return errors.Join(errors.New("removal metadata exceeds its read limit"), err)
	}
	s.metadataBytes += int64(len(raw))
	if s.metadataBytes > 512<<20 {
		return errors.New("removal metadata exceeds its total read limit")
	}
	return json.Unmarshal(raw, value)
}

func (s *workloadRemovalState) relative(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	relative, err := filepath.Rel(filepath.Join(s.root.Name(), s.path), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(s.path, relative), true
}

func (s *workloadRemovalState) retain(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validatePath(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("recovery asset is not a regular file or directory")
	}
	s.retainedRoots[path] = true
	for parent := path; parent != "."; parent = filepath.Dir(parent) {
		s.keep[parent] = true
		if parent == s.path {
			break
		}
	}
	return nil
}

func (s *workloadRemovalState) backupMetadata(ctx context.Context) error {
	directory := filepath.Join(s.path, "backups")
	if err := s.validatePath(directory); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := fs.ReadDir(s.root.FS(), filepath.ToSlash(directory))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 16384 {
		return errors.New("backup metadata inventory exceeds its limit")
	}
	seen := map[string]bool{}
	var retainJSON func(string) error
	retainJSON = func(path string) error {
		if seen[path] {
			return nil
		}
		seen[path] = true
		if len(seen) > 16384 {
			return errors.New("backup reference inventory exceeds its limit")
		}
		if err := s.retain(ctx, path); err != nil {
			return err
		}
		var value any
		if err := s.readJSON(path, &value); err != nil {
			return err
		}
		var inspect func(any) error
		inspect = func(value any) error {
			switch value := value.(type) {
			case string:
				value = strings.ReplaceAll(value, "$$", "$")
				s.images[value] = true
				if relative, own := s.relative(value); own {
					info, err := s.root.Lstat(relative)
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if !info.IsDir() && strings.HasSuffix(relative, ".compose.json") {
						return retainJSON(relative)
					}
					return s.retain(ctx, relative)
				}
			case map[string]any:
				for _, child := range value {
					if err := inspect(child); err != nil {
						return err
					}
				}
			case []any:
				for _, child := range value {
					if err := inspect(child); err != nil {
						return err
					}
				}
			}
			return nil
		}
		return inspect(value)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		var manifest backupManifest
		if err := s.readJSON(path, &manifest); err != nil {
			return err
		}
		if manifest.State.ComposePath != "" {
			relative, own := s.relative(manifest.State.ComposePath)
			if !own {
				return errors.New("backup release path is outside its resource")
			}
			if _, err := s.root.Lstat(relative); err != nil {
				return errors.New("backup release metadata is missing")
			}
		}
		if err := retainJSON(path); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) planWorkloadRemoval(ctx context.Context, id string, containers []Container) (*workloadRemovalState, error) {
	absoluteRoot, err := filepath.Abs(e.Root)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return nil, err
	}
	s := &workloadRemovalState{root: root, path: filepath.Join("workloads", id), keep: map[string]bool{}, retainedRoots: map[string]bool{}, images: map[string]bool{}}
	for _, path := range []string{"workloads", s.path} {
		info, err := root.Lstat(path)
		if os.IsNotExist(err) {
			return s, nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("resource state is not an owned directory")
		}
	}
	if _, err := root.Lstat(filepath.Join(s.path, "copy-recovery.json")); err == nil {
		s.uncertain("a restore-copy recovery checkpoint remains")
		return s, nil
	} else if !os.IsNotExist(err) {
		root.Close()
		return nil, err
	}
	var current deployedState
	if err := s.readJSON(filepath.Join(s.path, "current.json"), &current); err != nil {
		s.uncertain("the last deployed snapshot is unavailable")
		return s, nil
	}
	snapshot, own := s.relative(current.ComposePath)
	if !own {
		s.uncertain("the last deployed snapshot path could not be verified")
		return s, nil
	}
	var config map[string]any
	if err := s.readJSON(snapshot, &config); err != nil {
		s.uncertain("the last deployed snapshot could not be read")
		return s, nil
	}
	bindRetained := false
	bind := func(source string) error {
		if strings.Contains(strings.ReplaceAll(source, "$$", ""), "$") || !filepath.IsAbs(source) {
			return errors.New("a bind source is unresolved")
		}
		if path, own := s.relative(strings.ReplaceAll(source, "$$", "$")); own {
			if err := s.retain(ctx, path); err != nil {
				return err
			}
			bindRetained = bindRetained || s.retainedRoots[path]
		}
		return nil
	}
	volumes := map[string]bool{}
	if err := e.removalBindSources(ctx, config, containers, bind, volumes); err != nil {
		s.uncertain("bind-mounted data could not be determined with confidence")
		return s, nil
	}
	if err := s.backupMetadata(ctx); err != nil {
		if os.IsPermission(err) {
			s.uncertain("backup recovery metadata is not accessible to the worker")
			return s, nil
		}
		root.Close()
		return nil, errors.New("backup recovery metadata could not be preserved; resource removal refused")
	}
	if err := s.preflightCleanup(ctx); err != nil {
		if os.IsPermission(err) {
			s.uncertain("the worker lacks permission to clean up its state directory")
			return s, nil
		}
		root.Close()
		return nil, err
	}
	s.result.WorkerFilesRetained = len(s.keep) > 0
	if bindRetained {
		s.result.WorkerFilesRetainedReason = "Bind-mounted data inside this resource's state directory was kept."
	}
	return s, nil
}

func (s *workloadRemovalState) apply(ctx context.Context) (result error) {
	defer func() {
		if os.IsPermission(result) {
			s.result.WorkerFilesRetained = true
			s.result.CleanupWarning = "Some worker files were kept because the worker could not remove them with its current permissions."
			result = nil
		}
	}()
	if s.result.CleanupWarning != "" {
		return nil
	}
	if err := s.preflightCleanup(ctx); err != nil {
		return err
	}
	if len(s.keep) == 0 {
		return s.root.RemoveAll(s.path)
	}
	return fs.WalkDir(s.root.FS(), filepath.ToSlash(s.path), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.retainedRoots[filepath.FromSlash(path)] && entry.IsDir() {
			return fs.SkipDir
		}
		if s.keep[filepath.FromSlash(path)] {
			return nil
		}
		if err := s.root.RemoveAll(filepath.FromSlash(path)); err != nil {
			return err
		}
		if entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

func (s *workloadRemovalState) validatePath(path string) error {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	parent := ""
	for _, part := range parts {
		parent = filepath.Join(parent, part)
		info, err := s.root.Lstat(parent)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("resource recovery path contains a symlink")
		}
	}
	return nil
}
