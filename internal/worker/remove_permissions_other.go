//go:build !linux

package worker

import "os"

func removalDirectoryAccess(_ *os.File) error { return nil }
