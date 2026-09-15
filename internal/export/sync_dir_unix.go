//go:build !windows

package export

import (
	"os"
	"path/filepath"
)

// syncParent makes a preceding rename durable on filesystems that support
// syncing directory file descriptors.
func syncParent(name string) error {
	d, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
