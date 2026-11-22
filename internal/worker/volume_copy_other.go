//go:build !linux

package worker

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

func fileOwner(fs.FileInfo) (uint32, uint32) { return 0, 0 }

func applyMetadata(path string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(path, time.Time{}, info.ModTime())
}

func makeFifo(string, uint32) error {
	return errors.New("named pipes can only be copied on Linux")
}

func freeBytes(string) (int64, error) {
	return 0, errors.New("free space is only measured on Linux")
}
