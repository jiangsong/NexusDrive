package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// lockName is the file whose advisory lock marks the process that owns
// agent.db: the daemon that runs retention and session expiry.
const lockName = "agent.lock"

// acquireOwnership takes an exclusive advisory lock on the store directory.
//
// Ownership decides who runs the background housekeeping (audit retention,
// idle-session expiry), not who may write: a stdio MCP process started by an
// agent while the daemon is up must still append its own audit rows, which
// SQLite in WAL mode serialises across processes. Failing to take the lock is
// therefore not an error; that process simply is not the owner.
func acquireOwnership(dir string) (*os.File, bool, error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("agent: open lock: %w", err)
	}
	if err := lockFileExclusive(f); err != nil {
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

// releaseOwnership drops the lock.
func releaseOwnership(f *os.File) {
	if f == nil {
		return
	}
	unlockFile(f)
	f.Close()
}
