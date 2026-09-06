//go:build desktop

package main

import (
	"os/exec"
	"testing"
	"time"
)

// TestCloseReturnsAfterTheDaemonDiedEarly: when the daemon this shell
// launched exits before it is ready, the readiness loop learns about it —
// and the window still opens with an error, so Close runs later anyway. The
// two used to share a single value on a channel: whichever read it first
// left the other waiting on a process that had already been reaped, and the
// desktop shell never exited. Both watch for the same close now.
func TestCloseReturnsAfterTheDaemonDiedEarly(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child here: %v", err)
	}
	s := newShell("", nil)
	exited := s.own(cmd)

	// What launch() does when the daemon dies before answering.
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never finished")
	}
	if s.exitError() == nil {
		t.Fatal("the exit status was lost")
	}

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an exit signal another caller had taken")
	}
}
