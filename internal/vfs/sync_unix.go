//go:build !windows

package vfs

import "syscall"

// syncDisks flushes every filesystem's pending writes. DropCaches uses it so a
// cold benchmark measures cold reads and not the previous run's writeback.
func syncDisks() { syscall.Sync() }
