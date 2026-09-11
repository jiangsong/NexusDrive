//go:build !windows

package fusefs

import (
	"context"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// leaseFd returns a descriptor on the fully cached copy of the file behind
// f, obtaining a lease via FS.OpenLocal on first use and reusing it for
// every later caller on this handle. Both PassthroughFd (the kernel's own
// backing registration, gated by the experimental passthrough opt-in) and
// Read's splice fast path (unconditional — see spliceRead) go through this,
// so a handle never holds two independent leases and Release only ever has
// one to tear down.
//
// It returns ok=false when the file is not eligible for a lease at all: not
// fully cached, currently open for writing, or the handle has already been
// released.
func (f *file) leaseFd() (int, bool) {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if f.released {
		return 0, false
	}
	return f.leaseFdLocked()
}

// leaseFdLocked is leaseFd's body, for callers that already hold f.passMu
// (PassthroughFd holds it itself to add its own gate ahead of this).
func (f *file) leaseFdLocked() (int, bool) {
	if f.pass != nil {
		return f.root.backings.fd(f.pass)
	}
	pf, err := f.root.opt.FS.OpenLocal(f.handle)
	if err != nil {
		return 0, false
	}
	f.pass = f.root.backings.offer(pf)
	return f.root.backings.fd(f.pass)
}

// spliceRead returns a zero-copy fuse.ReadResultFd for a read against a
// fully hydrated file, clipped to the file's current size
// (docs/pool-v2.md §4.6). Unlike PassthroughFd's kernel-backing
// registration, this splice path needs no CAP_SYS_ADMIN and is not gated by
// the CLOUDFS_EXPERIMENTAL_PASSTHROUGH opt-in, so it also covers mounts and
// callers that can never negotiate full passthrough. ok is false when Read
// must fall back to the normal FS.Read path instead: no lease is available,
// or off is already at or past end of file.
func (f *file) spliceRead(ctx context.Context, off int64, want int) (fuse.ReadResult, bool) {
	if want <= 0 || off < 0 {
		return nil, false
	}
	fd, ok := f.leaseFd()
	if !ok {
		return nil, false
	}
	at, err := f.root.opt.FS.Stat(ctx, f.handle.Ino)
	if err != nil || off >= at.Size {
		return nil, false
	}
	sz := int64(want)
	if remaining := at.Size - off; remaining < sz {
		sz = remaining
	}
	return fuse.ReadResultFd(uintptr(fd), off, int(sz)), true
}
