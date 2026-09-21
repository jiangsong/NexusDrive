//go:build !windows

package fusefs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

var _ fs.NodeCopyFileRanger = (*node)(nil)

// CopyFileRange implements copy_file_range(2), but only for a copy that
// stays inside this mount: a hydrated source's cache fd is read straight
// into the destination write handle's staging file through
// vfs.FS.WriteFromFD, so the copy gets exactly the staging, hash and
// pendingSize bookkeeping a userspace read+write of those bytes would
// produce (docs/pool-v2.md §4.6). Anything that does not fit that shape —
// either handle is not a *file, the source has no cache lease to read from,
// or the destination is not a write handle of this mount — returns ENOTSUP
// so the kernel retries the copy as a plain read/write pair instead.
//
// A copy to a different mount (e.g. `cp` onto a plugged-in disk) never
// reaches this method at all: the kernel's fuse_copy_file_range rejects a
// cross-superblock pair with EXDEV before it issues a FUSE request, so cp
// falls back to read/write on its own without our involvement.
func (n *node) CopyFileRange(ctx context.Context, fhIn fs.FileHandle, offIn uint64, out *fs.Inode, fhOut fs.FileHandle, offOut uint64, length uint64, flags uint64) (uint32, syscall.Errno) {
	src, ok := fhIn.(*file)
	if !ok {
		return 0, syscall.ENOTSUP
	}
	dst, ok := fhOut.(*file)
	if !ok {
		return 0, syscall.ENOTSUP
	}
	if !dst.handle.IsWriteHandle() {
		return 0, syscall.ENOTSUP
	}
	l, ok := src.lease()
	if !ok {
		return 0, syscall.ENOTSUP
	}
	// Pinned for the copy: a concurrent read on src may withdraw the lease
	// once a writer appears, and must not close the descriptor under us.
	fd, ok := n.root.backings.acquire(l)
	if !ok {
		return 0, syscall.ENOTSUP
	}
	defer n.root.backings.release(l)
	n.root.count(opCopyFileRange)
	// Like Write: this grows the destination, so a cached listing entry for
	// it now carries the size from before the copy.
	n.root.forgetIno(dst.handle.Ino)
	written, err := n.root.opt.FS.WriteFromFD(ctx, dst.handle, fd, int64(offIn), int64(offOut), int64(length))
	if err != nil {
		return uint32(written), errno(err)
	}
	return uint32(written), 0
}
