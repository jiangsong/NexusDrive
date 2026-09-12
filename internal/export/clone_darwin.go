package export

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dst a copy-on-write clone of src. clonefile(2) creates dst,
// so any earlier attempt at that name is removed first. The clone shares
// blocks with the cache object but is a separate inode: editing the exported
// file on the drive cannot reach back into the cache.
func cloneFile(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return unix.Clonefile(src, dst, 0)
}
