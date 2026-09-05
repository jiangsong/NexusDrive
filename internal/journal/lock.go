package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockName is the file whose advisory lock marks the process that owns the
// queue.
const lockName = "journal.lock"

// acquireOwnership takes an exclusive advisory lock on the journal directory.
//
// Only the owner may run recovery. Recovery rewrites rows and deletes files,
// which is correct exactly once at startup and destructive at any other time:
// a second process running it against a live daemon would push rows that are
// uploading right now back onto the queue, delete the staging file a write is
// still filling, and remove the data behind a dead letter. Every `cloudfs`
// subcommand builds the same stack, so without this a status command would
// corrupt the queue it was asked to report on.
//
// Failing to take the lock is not an error: the process simply does not own
// the queue and skips recovery.
func acquireOwnership(dir string) (*os.File, bool, error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("journal: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Someone else owns it. Keep the handle closed and carry on read-only.
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
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
