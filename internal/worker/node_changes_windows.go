package worker

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func lockNodeChanges(root string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(root, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// Keep the lock range outside the empty file contents so crash snapshots can read it.
	overlap := windows.Overlapped{Offset: 1 << 31}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
	if err != nil {
		file.Close()
		return nil, err
	}
	return func() { windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlap); file.Close() }, nil
}
