package worker

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func verifyCacheOwner(path, owner string, writable bool) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("the build cache filesystem must be private to the worker")
	}
	marker := filepath.Join(path, ".cove-build-cache-owner.json")
	info, err = os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256 || info.Mode().Perm()&0077 != 0 {
		return errors.New("build cache ownership cannot be verified")
	}
	data, err := os.ReadFile(marker)
	var existing string
	if err != nil || json.Unmarshal(data, &existing) != nil || existing != owner {
		return errors.New("build cache belongs to another owner or ownership is unreadable")
	}
	if !writable {
		return nil
	}
	file, err := os.CreateTemp(path, ".cove-write-probe-")
	if err != nil {
		return cacheWriteFailure(err)
	}
	name := file.Name()
	_, writeErr := file.Write([]byte("cove-build-cache\n"))
	syncErr := file.Sync()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return errors.Join(cacheWriteFailure(writeErr), cacheWriteFailure(syncErr), cacheWriteFailure(closeErr), cacheWriteFailure(removeErr))
}

func cacheWriteFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) {
		return errBuildCacheFull
	}
	return errors.New("the verified build cache filesystem is not writable")
}

func claimCacheOwner(path, owner string) error {
	marker := filepath.Join(path, ".cove-build-cache-owner.json")
	if _, err := os.Lstat(marker); err == nil {
		return verifyCacheOwner(path, owner, true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("build cache ownership cannot be read")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("the build cache filesystem must be private to the worker")
	}
	directory, err := os.Open(path)
	if err != nil {
		return errors.New("build cache state cannot be read")
	}
	entries, readErr := directory.ReadDir(3)
	closeErr := directory.Close()
	if readErr != nil && readErr != io.EOF || closeErr != nil {
		return errors.New("build cache state cannot be read")
	}
	for _, entry := range entries {
		if entry.Name() != "lost+found" || !entry.IsDir() {
			return errors.New("a newly claimed build cache filesystem must be empty")
		}
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("build cache ownership could not be claimed")
	}
	data, _ := json.Marshal(owner)
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr = file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("build cache ownership could not be persisted")
	}
	if err = syncDirectory(path); err != nil {
		return errors.New("build cache ownership could not be persisted")
	}
	return verifyCacheOwner(path, owner, true)
}
