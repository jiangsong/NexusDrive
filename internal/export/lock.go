package export

import (
	"fmt"
	"os"
	"path/filepath"
)

// lockName is the file whose advisory lock marks the process that runs the
// export queue.
const lockName = "exports.lock"

// acquireOwnership takes an exclusive advisory lock on the store directory.
//
// Only the owner may run jobs. A second process running them would write the
// same destination files from two schedulers and check-point over each
// other's bitmaps. Failing to take the lock is not an error: that process
// simply reads the queue and refuses to change it.
func acquireOwnership(dir string) (*os.File, bool, error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("export: open lock: %w", err)
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
