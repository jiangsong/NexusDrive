//go:build desktop && windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureDaemonCmd puts the daemon in its own process group so a console
// break can be sent to just it, not to this shell.
func configureDaemonCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// terminateDaemon asks the daemon to shut down cleanly. Windows has no SIGINT
// to another process; a console break to the child's own process group is the
// closest request that reaches cmdMount's shutdown path. If it cannot be
// delivered, the caller's timeout falls back to a hard kill.
//
// UNVERIFIED: the break-to-shutdown path must be confirmed on a real Windows
// build (see docs/distribution.md). Correctness of the mount-owner invariant
// does not depend on it — a hard kill still releases the journal lock — only
// the cleanliness of the unmount does.
func terminateDaemon(p *os.Process) {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid)); err != nil {
		_ = p.Kill()
	}
}
