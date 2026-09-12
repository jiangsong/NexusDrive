//go:build !windows

package export

import (
	"os"
	"syscall"
)

// lockFileExclusive takes a non-blocking exclusive advisory lock (flock).
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
