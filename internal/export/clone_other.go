//go:build !linux && !darwin

package export

import "syscall"

// cloneFile has no copy-on-write path on this platform.
func cloneFile(src, dst string) error { return syscall.ENOTSUP }
