//go:build linux || darwin

package fusefs

import "golang.org/x/sys/unix"

// getxattr reads one extended attribute, used to check cloudfs file state from
// the mount the way a user would with getfattr.
func getxattr(path, attr string, dest []byte) (int, error) {
	return unix.Getxattr(path, attr, dest)
}
