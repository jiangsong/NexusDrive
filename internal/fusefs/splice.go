//go:build !windows

package fusefs

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// leaseFdLocked returns a descriptor on the fully cached copy of the file
// behind f, for PassthroughFd, which holds f.passMu itself to add its own
// gate ahead of this. Every other caller pins the lease with
// backingRegistry.acquire for as long as it uses the descriptor.
func (f *file) leaseFdLocked() (int, bool) {
	l, ok := f.leaseLocked()
	if !ok {
		return 0, false
	}
	return f.root.backings.fd(l)
}

// leaseLocked returns the handle's lease on the fully cached copy of the
// file behind f, obtaining one via FS.OpenLocal on first use and reusing it
// for every later caller on this handle. PassthroughFd (the kernel's own
// backing registration, gated by the experimental passthrough opt-in),
// Read's splice fast path (unconditional — see spliceRead) and
// CopyFileRange all go through this, so a handle never holds two
// independent leases and Release only ever has one to tear down.
//
// It returns ok=false when the file is not eligible for a lease at all: not
// fully cached, or currently open for writing. Requires f.passMu.
func (f *file) leaseLocked() (*backingLease, bool) {
	if !f.root.opt.FS.LocalReadAllowed(f.handle) {
		return nil, false
	}
	if f.pass != nil {
		return f.pass, true
	}
	pf, err := f.root.opt.FS.OpenLocal(f.handle)
	if err != nil {
		return nil, false
	}
	f.pass = f.root.backings.offer(pf)
	return f.pass, true
}

// lease is leaseLocked for callers that do not hold f.passMu; it also
// refuses a handle that has already been released.
func (f *file) lease() (*backingLease, bool) {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if f.released {
		return nil, false
	}
	return f.leaseLocked()
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
	l, ok := f.lease()
	if !ok {
		return nil, false
	}
	if !f.root.opt.FS.LocalCurrent(f.handle) {
		// The file is being, or has been, rewritten through another
		// descriptor: the leased entry is the old version. Drop the offer
		// so a later read can lease the new version once it is cached; a
		// backing the kernel already registered stays with the kernel
		// (the experimental passthrough gate exists for that case).
		f.dropOffer()
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
	// The descriptor is consumed after this handler returns, so it is
	// pinned until go-fuse calls Done; a concurrent dropOffer on the same
	// handle withdraws the lease without closing it in the meantime.
	fd, ok := f.root.backings.acquire(l)
	if !ok {
		return nil, false
	}
	return &leasedResult{
		ReadResult: fuse.ReadResultFd(uintptr(fd), off, int(sz)),
		fd:         uintptr(fd),
		off:        off,
		sz:         int(sz),
		backings:   f.root.backings,
		lease:      l,
	}, true
}

// leasedResult is a fuse.ReadResultFd whose descriptor stays open until
// go-fuse has finished with it. It implements go-fuse's seekableResult so
// the server still splices from the descriptor instead of reading it.
type leasedResult struct {
	fuse.ReadResult
	fd       uintptr
	off      int64
	sz       int
	backings *backingRegistry
	lease    *backingLease
	done     sync.Once
}

func (r *leasedResult) Seekable() (fd uintptr, off int64, sz int) { return r.fd, r.off, r.sz }

func (r *leasedResult) Done() {
	r.ReadResult.Done()
	r.done.Do(func() { r.backings.release(r.lease) })
}

// dropOffer releases the handle's lease when the kernel never registered
// it; a registered backing is owned by that registration until the kernel
// unregisters it.
func (f *file) dropOffer() {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if f.pass != nil && f.pass.id == 0 {
		f.root.backings.releaseOffer(f.pass)
		f.pass = nil
	}
}
