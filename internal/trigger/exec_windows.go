//go:build windows

package trigger

import "os/exec"

// setProcessGroup is a no-op on Windows.
// UNVERIFIED: Windows has no Setpgid; the equivalent is a Job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, or CREATE_NEW_PROCESS_GROUP plus a
// console control event. Until one is wired and tested on a Windows
// machine, a timed-out command's grandchildren survive the kill.
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills only the child on Windows, see setProcessGroup.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
