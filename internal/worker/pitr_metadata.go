package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func writePITRMetadata(filename string, value any) error {
	directory := filepath.Dir(filename)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("PITR metadata directory is not private")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > snapshotHostInputLimit {
		return errors.New("PITR metadata exceeds its bounded size")
	}
	file, err := os.CreateTemp(directory, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	// The helper runs as root to read PostgreSQL storage; its receipts belong to the worker.
	uid, gid := fileOwner(info)
	if err = file.Chown(int(uid), int(gid)); err == nil {
		err = file.Chmod(0600)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err = os.Rename(file.Name(), filename); err != nil {
		return err
	}
	return syncDirectory(directory)
}
