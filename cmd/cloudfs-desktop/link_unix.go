//go:build desktop && !windows

package main

import (
	"os"
	"os/exec"
)

// configureDaemonCmd needs no special setup on unix: SIGINT to the child is
// delivered directly.
func configureDaemonCmd(*exec.Cmd) {}

// terminateDaemon asks the daemon to shut down cleanly. SIGINT is what
// cmdMount's signal.NotifyContext waits on, so this runs its unmount/close path.
func terminateDaemon(p *os.Process) { _ = p.Signal(os.Interrupt) }
