//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// reexecSelf replaces this process image with a fresh copy of the same binary,
// same arguments and same environment. It is how a control-plane restart comes
// back: by the time it runs, cmdMount's deferred teardown has released the
// journal lock and detached the mount, and execve keeps the PID, so a
// supervisor watching the process sees a continuous main PID and there is never
// a moment with two owners of the storage. It does not return on success.
func reexecSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
