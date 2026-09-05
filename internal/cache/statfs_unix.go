//go:build linux || darwin

package cache

import "golang.org/x/sys/unix"

// FreeSpace returns the bytes available to an unprivileged process on the
// filesystem holding dir.
func FreeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
