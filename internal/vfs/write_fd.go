//go:build !windows

package vfs

import (
	"context"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sys/unix"
)

// copyFileRangeBufSize chunks a copy_file_range through the same buffer size
// the streaming file-copy path uses (docs/copy.md), so this shares its
// memory profile instead of introducing a second bespoke limit.
const copyFileRangeBufSize = 1 << 20

var copyFileRangeBufPool = sync.Pool{
	New: func() any { return make([]byte, copyFileRangeBufSize) },
}

// IsWriteHandle reports whether h was opened for writing. fusefs's
// NodeCopyFileRanger calls this to confirm a copy_file_range destination is
// a write handle of this mount before attempting the in-mount fast path;
// every other decision about the handle stays inside this package.
func (h *Handle) IsWriteHandle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writer != nil
}

// WriteFromFD copies n bytes read from srcFD at srcOff into h's staging file
// at dstOff, through the exact same Write path a userspace write would take:
// the bytes land in staging with the same hashes and pendingSize that
// calling Write with those bytes would produce (Write is what actually
// performs each chunk's staging write). This is the vfs half of fusefs's
// copy_file_range(2) fast path (see internal/fusefs's NodeCopyFileRanger and
// docs/pool-v2.md §4.6): that path is only ever taken for a hydrated source
// and a destination write handle of this mount. A copy across mounts (e.g.
// `cp` onto a plugged-in disk) never reaches this at all — the kernel's
// fuse_copy_file_range rejects a cross-superblock pair with EXDEV before it
// ever issues a FUSE request.
//
// A short read from srcFD — end of file before n bytes were available — is
// reported as an error, and nothing past what was actually read is written.
// The number of bytes actually written is returned alongside any error, so a
// caller can tell how far a partial copy got.
func (f *FS) WriteFromFD(ctx context.Context, h *Handle, srcFD int, srcOff, dstOff, n int64) (int64, error) {
	if n < 0 {
		return 0, fmt.Errorf("vfs: negative copy_file_range length %d", n)
	}
	if srcOff < 0 || dstOff < 0 {
		return 0, fmt.Errorf("vfs: negative copy_file_range offset (src %d, dst %d)", srcOff, dstOff)
	}
	if n == 0 {
		return 0, nil
	}
	buf := copyFileRangeBufPool.Get().([]byte)
	defer copyFileRangeBufPool.Put(buf)

	var written int64
	for written < n {
		chunk := buf
		if remaining := n - written; remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		rn, rerr := unix.Pread(srcFD, chunk, srcOff+written)
		if rerr != nil {
			return written, fmt.Errorf("vfs: copy_file_range read source: %w", rerr)
		}
		if rn == 0 {
			// End of file on the source before n bytes were available.
			return written, io.ErrUnexpectedEOF
		}
		wn, werr := f.Write(ctx, h, chunk[:rn], dstOff+written)
		written += int64(wn)
		if werr != nil {
			return written, werr
		}
		if wn != rn {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
