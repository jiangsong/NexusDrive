//go:build !linux && !darwin

package service

import "fmt"

func currentUID() int { return 0 }

// MountpointMounted cannot be determined outside Linux and macOS in this
// build; the platforms that lack service integration also lack the mount.
func MountpointMounted(string) (bool, error) { return false, nil }

// StaleMount cannot be determined outside Linux and macOS in this build.
func StaleMount(string) bool { return false }

// DetachStale is unsupported outside Linux and macOS in this build.
func DetachStale(path string) error { return Unmount(path) }

// Unmount is unsupported outside Linux and macOS in this build.
func Unmount(path string) error {
	return fmt.Errorf("unmounting is not supported on this platform; detach %s with the system tools", path)
}

// lock is a no-op where there is no service manager to serialize against;
// Install/Uninstall fail earlier with an unsupported-platform error anyway.
func (rt Runtime) lock() (func(), error) { return func() {}, nil }
