//go:build linux || darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func currentUID() int { return os.Getuid() }

// MountpointMounted reports whether path is a live mount point, by comparing
// its device number against its parent's.
func MountpointMounted(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return false, err
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("service: unsupported mount stat")
	}
	up, ok := parent.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("service: unsupported parent mount stat")
	}
	return current.Dev != up.Dev, nil
}

// Unmount detaches a mount this process does not own, using whichever helper
// the platform provides.
func Unmount(path string) error {
	var candidates [][]string
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"umount", path}, {"diskutil", "unmount", path}}
	} else {
		candidates = [][]string{{"fusermount3", "-u", path}, {"fusermount", "-u", path}, {"umount", path}}
	}
	var lastErr error
	var lastOut []byte
	for _, c := range candidates {
		bin, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, c[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr, lastOut = err, out
	}
	if lastErr == nil {
		return fmt.Errorf("no unmount helper found; install fuse3 (Linux) or use 'umount %s'", path)
	}
	return fmt.Errorf("unmount %s: %v: %s", path, lastErr, lastOut)
}

// lock serializes install/uninstall against each other with a non-blocking
// advisory lock on a sidecar file next to the service definition. A caller
// that loses the race is told so rather than made to wait, since a UI should
// report "another change is in progress" instead of hanging.
func (rt Runtime) lock() (func(), error) {
	file, err := rt.File()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, fmt.Errorf("service: create directory: %w", err)
	}
	lockPath := file + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("service: open lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("service: another install or uninstall is in progress")
	}
	return func() { unix.Flock(fd, unix.LOCK_UN); f.Close() }, nil
}
