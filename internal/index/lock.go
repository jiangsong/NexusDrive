package index

import (
	"fmt"
	"os"
	"path/filepath"
)

// lockName is the file whose advisory lock marks the process that runs the
// indexer: the one that extracts text, drains index_pending and reconciles
// against meta.
const lockName = "index.lock"

// acquireOwnership takes an exclusive advisory lock on the store directory.
//
// Only the owner may run the indexer. Two processes extracting the same
// files would race their upserts and double every remote fetch. Failing to
// take the lock is not an error: that process (the CLI, a second daemon on
// the same cache directory) simply reads the index.
func acquireOwnership(dir string) (*os.File, bool, error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("index: open lock: %w", err)
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
