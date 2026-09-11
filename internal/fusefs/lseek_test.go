//go:build !windows

package fusefs

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLseekNoHolesMapsDataAndHoleQueries exercises the pure SEEK_DATA/
// SEEK_HOLE mapping without a mount: cloudfs never reports a file as
// sparse, so every offset before EOF is "data" and EOF is the only place a
// "hole" query can land.
func TestLseekNoHolesMapsDataAndHoleQueries(t *testing.T) {
	const size = 100

	cases := []struct {
		name      string
		off       uint64
		whence    uint32
		wantOff   uint64
		wantErrno syscall.Errno
	}{
		{"SEEK_DATA before EOF", 10, unix.SEEK_DATA, 10, 0},
		{"SEEK_DATA at EOF", size, unix.SEEK_DATA, 0, syscall.ENXIO},
		{"SEEK_DATA past EOF", size + 50, unix.SEEK_DATA, 0, syscall.ENXIO},
		{"SEEK_HOLE before EOF", 10, unix.SEEK_HOLE, size, 0},
		{"SEEK_HOLE at EOF", size, unix.SEEK_HOLE, 0, syscall.ENXIO},
		{"SEEK_HOLE past EOF", size + 50, unix.SEEK_HOLE, 0, syscall.ENXIO},
		{"unsupported whence", 0, 99, 0, syscall.EINVAL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOff, gotErrno := lseekNoHoles(tc.off, size, tc.whence)
			if gotErrno != tc.wantErrno {
				t.Fatalf("errno = %v, want %v", gotErrno, tc.wantErrno)
			}
			if gotErrno == 0 && gotOff != tc.wantOff {
				t.Fatalf("offset = %d, want %d", gotOff, tc.wantOff)
			}
		})
	}
}
