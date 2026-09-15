//go:build !windows

package trigger

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a process group of its own, so that
// the timeout can reach the grandchildren a `claude -p` or a shell script
// leaves behind.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the child's whole process group. The group id equals
// the child's pid because setProcessGroup made it the leader. Falls back
// to killing just the child if the group is already gone.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
