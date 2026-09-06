//go:build windows

package journal

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusive takes a non-blocking exclusive lock on the whole file with
// LockFileEx. LOCKFILE_FAIL_IMMEDIATELY is the LOCK_NB of Windows: a second
// process gets an error rather than blocking, which is what tells it it does
// not own the queue.
func lockFileExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
}

func unlockFile(f *os.File) {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
