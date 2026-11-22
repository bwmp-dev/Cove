package worker

import (
	"io/fs"
	"os"
	"syscall"
	"time"
)

func fileOwner(info fs.FileInfo) (uint32, uint32) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, stat.Gid
	}
	return 0, 0
}

func applyMetadata(path string, info fs.FileInfo) error {
	uid, gid := fileOwner(info)
	if err := os.Lchown(path, int(uid), int(gid)); err != nil {
		return err
	}
	mode := info.Mode()
	if mode&fs.ModeSymlink != 0 {
		return nil
	}
	if err := os.Chmod(path, mode.Perm()|mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	return os.Chtimes(path, time.Time{}, info.ModTime())
}

func makeFifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}

func freeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
