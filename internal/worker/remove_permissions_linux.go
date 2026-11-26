package worker

import (
	"os"
	"syscall"
)

func removalDirectoryAccess(directory *os.File) error {
	const writeAndSearch, effectiveIDs = 3, 0x200
	return syscall.Faccessat(int(directory.Fd()), ".", writeAndSearch, effectiveIDs)
}
