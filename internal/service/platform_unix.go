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

// errNoHelper marks a helper this machine does not have, so the caller moves
// on to the next candidate instead of reporting the absence as a failure.
var errNoHelper = errors.New("service: unmount helper not installed")

// runHelper runs one unmount helper. Tests replace it.
var runHelper = func(name string, args ...string) ([]byte, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, errNoHelper
	}
	return exec.Command(bin, args...).CombinedOutput()
}

// mountIsStale reports whether a mount point has no server left. Tests
// replace it; production reads the kernel through StaleMount.
var mountIsStale = StaleMount

// staleErrno reports whether err is how a kernel answers a request to a
// mount whose server is gone. Linux says ENOTCONN. macFUSE says ENXIO,
// printed as "device not configured", so a check written against ENOTCONN
// alone sees every macOS stale mount as healthy.
func staleErrno(err error) bool {
	return errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.ENXIO)
}

// StaleMount reports whether path is a mount nobody serves. The directory is
// opened, not just stat'ed: the kernel answers stat from the attributes it
// cached while the last server was alive, for as long as their timeout runs,
// while an open always goes to the server.
func StaleMount(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return staleErrno(err)
	}
	d, err := os.Open(path)
	if err != nil {
		return staleErrno(err)
	}
	d.Close()
	return false
}

// runCandidates tries each helper in turn and reports the first success.
// Helpers this machine does not have are skipped, not counted as failures.
func runCandidates(candidates [][]string) (err error, out []byte, tried bool) {
	for _, c := range candidates {
		o, cerr := runHelper(c[0], c[1:]...)
		if errors.Is(cerr, errNoHelper) {
			continue
		}
		if cerr == nil {
			return nil, nil, true
		}
		err, out, tried = cerr, o, true
	}
	return err, out, tried
}

// forcedCandidates are the helpers that take down a mount even while
// something sits inside it.
func forcedCandidates(path string) [][]string {
	if runtime.GOOS == "darwin" {
		return [][]string{{"umount", "-f", path}, {"diskutil", "unmount", "force", path}}
	}
	return [][]string{{"fusermount3", "-uz", path}, {"fusermount", "-uz", path}, {"umount", "-l", path}}
}

// politeCandidates are the helpers that refuse a mount someone is using.
func politeCandidates(path string) [][]string {
	if runtime.GOOS == "darwin" {
		return [][]string{{"umount", path}, {"diskutil", "unmount", path}}
	}
	return [][]string{{"fusermount3", "-u", path}, {"fusermount", "-u", path}, {"umount", path}}
}

// DetachStale takes down a mount nobody serves — the kernel still lists it,
// a request to it answers ENOTCONN on Linux or ENXIO on macOS — even while
// something sits inside it. A shell whose working directory is the mount, or
// a file manager showing it, keeps a plain unmount refused with "target is
// busy", and the daemon that left the mount is gone, so nothing will ever
// serve those holders again. A lazy detach lets them go on failing exactly
// as they do now while the path is freed for the next mount. It is only for
// a mount already known to be stale: lazily detaching a live one would
// strand its users.
func DetachStale(path string) error {
	err, out, tried := runCandidates(forcedCandidates(path))
	if err == nil && tried {
		return nil
	}
	if !tried {
		return fmt.Errorf("no unmount helper found; install fuse3 (Linux) or use 'umount -l %s'", path)
	}
	return fmt.Errorf("detach %s: %v: %s", path, err, out)
}

// Unmount detaches a mount this process does not own, using whichever helper
// the platform provides. A mount whose server is gone is what those helpers
// refuse — macOS answers "Unmount failed for <path>" and nothing more — so
// the forced detach is the fallback once the mount point is confirmed
// stale. Without it a daemon that died without unmounting left its mount
// point unusable for every later mount, and the person had to find
// `umount -f` themselves. A live mount that refuses is busy, not stale, and
// is left alone.
func Unmount(path string) error {
	err, out, tried := runCandidates(politeCandidates(path))
	if err == nil && tried {
		return nil
	}
	if mountIsStale(path) {
		if derr := DetachStale(path); derr == nil {
			return nil
		}
	}
	if !tried {
		return fmt.Errorf("no unmount helper found; install fuse3 (Linux) or use 'umount %s'", path)
	}
	return fmt.Errorf("unmount %s: %v: %s", path, err, out)
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
