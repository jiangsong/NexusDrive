//go:build !windows

package export

import (
	"os"
	"syscall"
)

// deviceOf returns the filesystem identity of a path. A destination whose
// device changed is a different filesystem mounted where the drive used to
// be — the case that makes "the drive was pulled" detectable rather than a
// long run of write errors.
func deviceOf(info os.FileInfo) (uint64, bool) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(s.Dev), true
}
