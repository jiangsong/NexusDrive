//go:build !windows

package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// openLockedFile opens (creating if needed) a sidecar lock file and takes an
// exclusive advisory lock on it, blocking until it is granted. It is how the
// config editors serialize read/modify/atomic-rename against each other and
// against a concurrent credential rotation. O_NOFOLLOW refuses to follow a
// symlink planted where the lock file should be.
func openLockedFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openNoFollow opens an existing file read-only, refusing a symlink. Used to
// read a private credential file without following a planted link.
func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
