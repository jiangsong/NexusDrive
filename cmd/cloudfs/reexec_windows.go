//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// reexecSelf starts a fresh copy of the daemon and returns so the caller can
// exit. Windows has no execve; a spawn-then-exit is the closest equivalent.
// The parent has already released the journal lock (cmdMount's deferred
// teardown ran before this is called), so the child can acquire it. This build
// mounts nothing, so there is no FUSE owner to hand over either.
func reexecSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	proc, err := os.StartProcess(exe, os.Args, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   os.Environ(),
	})
	if err != nil {
		return err
	}
	return proc.Release()
}
