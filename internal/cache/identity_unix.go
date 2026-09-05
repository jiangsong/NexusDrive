//go:build linux || darwin

package cache

import (
	"os"
	"syscall"
)

type diskIdentity struct{ device, inode uint64 }

func identity(info os.FileInfo) diskIdentity {
	s := info.Sys().(*syscall.Stat_t)
	return diskIdentity{uint64(s.Dev), uint64(s.Ino)}
}
