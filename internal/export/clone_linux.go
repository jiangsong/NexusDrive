package export

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dst a copy-on-write clone of src with FICLONE, which btrfs
// and XFS support and every other filesystem refuses (the caller then copies
// the bytes).
func cloneFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := unix.IoctlFileClone(int(d.Fd()), int(s.Fd())); err != nil {
		d.Close()
		os.Remove(dst)
		return err
	}
	return d.Close()
}
