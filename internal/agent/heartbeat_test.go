package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveStdioProcessesDropsStaleHeartbeats: a heartbeat counts while it is
// fresh, stops counting once it is older than HeartbeatStale, and the stale
// file is gone after the look that judged it — a crashed stdio server must
// not keep doctor warning forever.
func TestLiveStdioProcessesDropsStaleHeartbeats(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	now := time.Now()
	if got := LiveStdioProcesses(dir, now); len(got) != 0 {
		t.Fatalf("a missing directory reported %v", got)
	}
	if err := WriteHeartbeat(dir, 4242); err != nil {
		t.Fatal(err)
	}
	if err := WriteHeartbeat(dir, 17); err != nil {
		t.Fatal(err)
	}
	if got := LiveStdioProcesses(dir, now); len(got) != 2 || got[0] != 17 || got[1] != 4242 {
		t.Fatalf("live = %v, want [17 4242]", got)
	}
	// Files that are not heartbeats are left alone, whatever their age.
	if err := os.WriteFile(filepath.Join(dir, "agent.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-HeartbeatStale - time.Second)
	if err := os.Chtimes(filepath.Join(dir, "stdio-17.hb"), old, old); err != nil {
		t.Fatal(err)
	}
	if got := LiveStdioProcesses(dir, now); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("live after 17 went stale = %v, want [4242]", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "stdio-17.hb")); !os.IsNotExist(err) {
		t.Fatalf("the stale heartbeat was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.db")); err != nil {
		t.Fatalf("a non-heartbeat file was touched: %v", err)
	}
	// Refreshing brings a process back; removing it twice is fine.
	if err := WriteHeartbeat(dir, 17); err != nil {
		t.Fatal(err)
	}
	if got := LiveStdioProcesses(dir, now); len(got) != 2 {
		t.Fatalf("live after refresh = %v", got)
	}
	for range 2 {
		if err := RemoveHeartbeat(dir, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if got := LiveStdioProcesses(dir, now); len(got) != 1 || got[0] != 17 {
		t.Fatalf("live after remove = %v, want [17]", got)
	}
}
