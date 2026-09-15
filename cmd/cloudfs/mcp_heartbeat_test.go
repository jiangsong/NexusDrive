package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestStdioHeartbeatLivesWithTheServer: the non-owner stdio branch of
// cmdMCP writes its heartbeat as soon as it starts and removes it when it
// stops, so doctor and the connect panel follow the process exactly.
func TestStdioHeartbeatLivesWithTheServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := stdioHeartbeat(ctx, dir)
	if live := agent.LiveStdioProcesses(dir, time.Now()); len(live) != 1 || live[0] != os.Getpid() {
		t.Fatalf("live after start = %v, want [%d]", live, os.Getpid())
	}
	stop()
	stop() // idempotent: cmdMCP's defers may run it after a signal did
	if live := agent.LiveStdioProcesses(dir, time.Now()); len(live) != 0 {
		t.Fatalf("live after stop = %v", live)
	}
}
