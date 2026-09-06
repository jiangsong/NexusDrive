//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// openLockedFile opens a sidecar lock file and takes an exclusive lock on it
// with LockFileEx, blocking until granted. Windows has no O_NOFOLLOW; a
// planted symlink is a different, weaker threat model on NTFS, and is accepted
// here rather than emulated.
func openLockedFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openLockedFileNB fails immediately when the lock is held.
func openLockedFileNB(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped)); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openNoFollow opens a file read-only. Windows does not offer O_NOFOLLOW here;
// the credential-file mode check that follows the open is the guard that
// matters on this platform.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
