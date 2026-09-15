//go:build windows && winfsp

package winfs

import (
	"context"
	"errors"
	"path"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// filesystem implements cgofuse's FileSystemInterface by delegating to vfs.FS.
// It embeds FileSystemBase so the operations cloudfs does not support (hard
// links, symlinks, xattr, chmod/chown) default to -ENOSYS, which is the same
// answer the Linux adapter gives.
type filesystem struct {
	fuse.FileSystemBase
	fs       *vfs.FS
	uid, gid uint32
	readOnly bool
	ctx      context.Context

	mu    sync.Mutex
	ops   map[string]int64
	rbyte int64
	rsize map[string]int64

	// dirMu guards a per-open-directory snapshot so Readdir can resume from an
	// offset the host hands back when its buffer fills, rather than re-listing
	// from the top each call (which truncates or loops a large directory).
	dirMu     sync.Mutex
	dirs      map[uint64]*dirSnapshot
	nextDirFH uint64
}

// dirSnapshot is one open directory's listing, taken at Opendir and paged
// through by Readdir until Releasedir drops it.
type dirSnapshot struct {
	dir     string
	entries []vfs.Attr
}

func newFilesystem(opt MountOptions) *filesystem {
	uid, gid := opt.UID, opt.GID
	if uid == 0 {
		uid = ^uint32(0) // -1: "the mounting user", WinFsp's default owner
	}
	if gid == 0 {
		gid = ^uint32(0)
	}
	return &filesystem{
		fs: opt.FS, uid: uid, gid: gid, readOnly: opt.ReadOnly,
		ctx:   context.Background(),
		ops:   map[string]int64{},
		rsize: map[string]int64{},
		dirs:  map[uint64]*dirSnapshot{},
	}
}

func (f *filesystem) count(op string) {
	f.mu.Lock()
	f.ops[op]++
	f.mu.Unlock()
}

func (f *filesystem) snapshot() OpStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make(map[string]int64, len(f.ops))
	for k, v := range f.ops {
		ops[k] = v
	}
	rs := make(map[string]int64, len(f.rsize))
	for k, v := range f.rsize {
		rs[k] = v
	}
	return OpStats{Ops: ops, ReadBytes: f.rbyte, ReadSizes: rs}
}

// errc maps a vfs error to the negative errno cgofuse expects. It mirrors
// fusefs.errno, so the two adapters return the same code for the same cause.
func errc(err error) int {
	switch {
	case err == nil:
		return 0
	case is(err, vfs.ErrNotFound):
		return -fuse.ENOENT
	case is(err, vfs.ErrExists):
		return -fuse.EEXIST
	case is(err, vfs.ErrIsDir):
		return -fuse.EISDIR
	case is(err, vfs.ErrNotDir):
		return -fuse.ENOTDIR
	case is(err, vfs.ErrNotEmpty):
		return -fuse.ENOTEMPTY
	case is(err, vfs.ErrReadOnly), is(err, vfs.ErrNotOwner):
		return -fuse.EROFS
	case is(err, vfs.ErrUploadCancelled), is(err, vfs.ErrUploadPurging):
		return -fuse.EBUSY
	case is(err, vfs.ErrNoSpace):
		return -fuse.ENOSPC
	case is(err, vfs.ErrCrossMount):
		return -fuse.EXDEV
	case is(err, provider.ErrUnsupported):
		return -fuse.ENOSYS
	case is(err, provider.ErrAuth):
		return -fuse.EACCES
	case is(err, provider.ErrRiskControl), is(err, provider.ErrRateLimited):
		return -fuse.EAGAIN
	case is(err, provider.ErrUnavailable):
		// The data exists; nothing holding it can be reached right now.
		// cgofuse has no EHOSTDOWN; host-unreachable is the nearest
		// status WinFsp knows.
		return -fuse.EHOSTUNREACH
	default:
		return -fuse.EIO
	}
}

// is is errors.Is, named short because errc reads better as a table.
func is(err, target error) bool { return errors.Is(err, target) }

// fillStat maps a vfs.Attr onto a cgofuse Stat_t.
func (f *filesystem) fillStat(a vfs.Attr, st *fuse.Stat_t) {
	mode := a.Mode
	if mode == 0 {
		if a.IsDir {
			mode = 0o755
		} else {
			mode = 0o644
		}
	}
	if a.IsDir {
		st.Mode = fuse.S_IFDIR | mode
		st.Nlink = 2
	} else {
		st.Mode = fuse.S_IFREG | mode
		st.Nlink = 1
	}
	st.Ino = a.Ino
	st.Size = a.Size
	st.Uid = f.uid
	st.Gid = f.gid
	ts := fuse.NewTimespec(a.MTime)
	st.Atim, st.Mtim, st.Ctim, st.Birthtim = ts, ts, ts, ts
}

// Statfs reports coarse volume statistics. UNVERIFIED: the numbers are nominal;
// Windows shows them in the drive's properties but nothing in cloudfs depends
// on them.
func (f *filesystem) Statfs(_ string, st *fuse.Statfs_t) int {
	const block = 4096
	st.Bsize = block
	st.Frsize = block
	st.Namemax = 255
	// When the backends report their space — a pool's members together,
	// a drive's quota — the drive's properties show that.
	if sp := f.fs.Space(f.ctx); sp.Known {
		st.Blocks = uint64(sp.Total) / block
		st.Bfree = uint64(sp.Free()) / block
		st.Bavail = st.Bfree
		return 0
	}
	st.Blocks = 1 << 40 / block
	st.Bfree = st.Blocks / 2
	st.Bavail = st.Bfree
	return 0
}

// Getattr answers stat. The root and every path resolve through vfs by path.
func (f *filesystem) Getattr(p string, st *fuse.Stat_t, _ uint64) int {
	f.count("getattr")
	a, err := f.fs.StatPath(f.ctx, clean(p))
	if err != nil {
		return errc(err)
	}
	f.fillStat(a, st)
	return 0
}

// Opendir is accepted so Readdir can run; the directory handle is unused
// because Readdir lists by path.
func (f *filesystem) Opendir(p string) (int, uint64) {
	entries, err := f.fs.ReadDirPath(f.ctx, clean(p))
	if err != nil {
		return errc(err), ^uint64(0)
	}
	f.dirMu.Lock()
	f.nextDirFH++
	fh := f.nextDirFH
	f.dirs[fh] = &dirSnapshot{dir: clean(p), entries: entries}
	f.dirMu.Unlock()
	return 0, fh
}

// Readdir lists a directory. It fills a full Stat_t per entry (readdir-plus).
func (f *filesystem) Readdir(p string, fill func(name string, st *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	f.count("readdir")
	f.dirMu.Lock()
	snap := f.dirs[fh]
	f.dirMu.Unlock()
	var entries []vfs.Attr
	if snap != nil {
		entries = snap.entries
	} else {
		// No handle (a host that lists without Opendir): fall back to a fresh
		// listing. Paging is still honoured within this one call.
		var err error
		entries, err = f.fs.ReadDirPath(f.ctx, clean(p))
		if err != nil {
			return errc(err)
		}
	}
	// The virtual list is [".", "..", entries...]. The offset passed to fill is
	// where a later Readdir resumes *after* that entry, so a host whose buffer
	// fills mid-listing continues instead of restarting from the top.
	i := ofst
	if i <= 0 {
		if !fill(".", nil, 1) {
			return 0
		}
		i = 1
	}
	if i == 1 {
		if !fill("..", nil, 2) {
			return 0
		}
		i = 2
	}
	for ; i-2 < int64(len(entries)); i++ {
		a := entries[i-2]
		if !representable(a.Name) {
			// A name Windows cannot hold is skipped rather than silently
			// rewritten; see naming.go for why, and the limitation this is.
			continue
		}
		var st fuse.Stat_t
		f.fillStat(a, &st)
		if !fill(a.Name, &st, i+1) {
			return 0
		}
	}
	return 0
}

// Open resolves the path and opens a vfs handle, returning its FH.
func (f *filesystem) Open(p string, flags int) (int, uint64) {
	f.count("open")
	write := flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0
	if write && f.readOnly {
		return -fuse.EROFS, ^uint64(0)
	}
	a, err := f.fs.StatPath(f.ctx, clean(p))
	if err != nil {
		return errc(err), ^uint64(0)
	}
	h, err := f.fs.Open(f.ctx, a.Ino, write)
	if err != nil {
		return errc(err), ^uint64(0)
	}
	return 0, h.FH
}

// Create makes and opens a file, returning its FH.
func (f *filesystem) Create(p string, flags int, _ uint32) (int, uint64) {
	f.count("create")
	if f.readOnly {
		return -fuse.EROFS, ^uint64(0)
	}
	dir, name := split(p)
	if !representable(name) {
		return -fuse.EINVAL, ^uint64(0)
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	parent, err := f.fs.StatPath(ctx, dir)
	if err != nil {
		return errc(err), ^uint64(0)
	}
	h, err := f.fs.Create(ctx, parent.Ino, name)
	if err != nil {
		return errc(err), ^uint64(0)
	}
	return 0, h.FH
}

// Mknod is how WinFsp creates a plain file when it does not immediately open
// it; route it through Create and drop the handle.
func (f *filesystem) Mknod(p string, mode uint32, _ uint64) int {
	f.count("mknod")
	rc, fh := f.Create(p, fuse.O_WRONLY, mode)
	if rc == 0 {
		if h, ok := f.fs.HandleByFH(fh); ok {
			f.fs.Release(f.ctx, h)
		}
	}
	return rc
}

func (f *filesystem) Read(_ string, buff []byte, ofst int64, fh uint64) int {
	f.count("read")
	h, ok := f.fs.HandleByFH(fh)
	if !ok {
		return -fuse.EBADF
	}
	n, err := f.fs.Read(f.ctx, h, buff, ofst)
	if err != nil {
		return errc(err)
	}
	f.mu.Lock()
	f.rbyte += int64(n)
	f.mu.Unlock()
	return n
}

func (f *filesystem) Write(_ string, buff []byte, ofst int64, fh uint64) int {
	f.count("write")
	if f.readOnly {
		return -fuse.EROFS
	}
	h, ok := f.fs.HandleByFH(fh)
	if !ok {
		return -fuse.EBADF
	}
	n, err := f.fs.Write(f.ctx, h, buff, ofst)
	if err != nil {
		return errc(err)
	}
	return n
}

// Flush commits staged data, exactly as the Linux adapter does on FLUSH: the
// caller whose handle is closing must be able to read back what it wrote.
// UNVERIFIED: WinFsp's CLEANUP/CLOSE dispatch differs from FUSE's FLUSH, and
// how often it calls Flush on a closing handle must be checked on real
// hardware — the "FLUSH fires per descriptor" assumption from Linux may not
// hold, though committing here and tearing down in Release is safe either way.
func (f *filesystem) Flush(_ string, fh uint64) int {
	f.count("flush")
	h, ok := f.fs.HandleByFH(fh)
	if !ok {
		return -fuse.EBADF
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	return errc(f.fs.Sync(ctx, h))
}

func (f *filesystem) Fsync(_ string, _ bool, fh uint64) int {
	f.count("fsync")
	h, ok := f.fs.HandleByFH(fh)
	if !ok {
		return -fuse.EBADF
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	return errc(f.fs.Sync(ctx, h))
}

// Release drops the handle. The commit already happened in Flush; vfs.Release
// is idempotent.
func (f *filesystem) Release(_ string, fh uint64) int {
	f.count("release")
	h, ok := f.fs.HandleByFH(fh)
	if !ok {
		return 0
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	return errc(f.fs.Release(ctx, h))
}

func (f *filesystem) Releasedir(_ string, fh uint64) int {
	f.dirMu.Lock()
	delete(f.dirs, fh)
	f.dirMu.Unlock()
	return 0
}

// Truncate resizes a file. WinFsp may pass a valid fh (an open handle) or the
// sentinel for a path-only truncate; vfs.TruncatePath prefers an open write
// handle and falls back, which is the invariant the Linux side relies on too.
func (f *filesystem) Truncate(p string, size int64, fh uint64) int {
	f.count("truncate")
	if f.readOnly {
		return -fuse.EROFS
	}
	if h, ok := f.fs.HandleByFH(fh); ok {
		ctx, cancel := durable(f.ctx)
		defer cancel()
		return errc(f.fs.Truncate(ctx, h, size))
	}
	a, err := f.fs.StatPath(f.ctx, clean(p))
	if err != nil {
		return errc(err)
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	return errc(f.fs.TruncatePath(ctx, a.Ino, size))
}

func (f *filesystem) Mkdir(p string, _ uint32) int {
	f.count("mkdir")
	if f.readOnly {
		return -fuse.EROFS
	}
	dir, name := split(p)
	if !representable(name) {
		return -fuse.EINVAL
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	parent, err := f.fs.StatPath(ctx, dir)
	if err != nil {
		return errc(err)
	}
	_, err = f.fs.Mkdir(ctx, parent.Ino, name)
	return errc(err)
}

func (f *filesystem) Unlink(p string) int {
	f.count("unlink")
	if f.readOnly {
		return -fuse.EROFS
	}
	dir, name := split(p)
	ctx, cancel := durable(f.ctx)
	defer cancel()
	parent, err := f.fs.StatPath(ctx, dir)
	if err != nil {
		return errc(err)
	}
	return errc(f.fs.Remove(ctx, parent.Ino, name, false))
}

func (f *filesystem) Rmdir(p string) int {
	f.count("rmdir")
	if f.readOnly {
		return -fuse.EROFS
	}
	dir, name := split(p)
	ctx, cancel := durable(f.ctx)
	defer cancel()
	parent, err := f.fs.StatPath(ctx, dir)
	if err != nil {
		return errc(err)
	}
	return errc(f.fs.Remove(ctx, parent.Ino, name, false))
}

func (f *filesystem) Rename(oldpath, newpath string) int {
	f.count("rename")
	if f.readOnly {
		return -fuse.EROFS
	}
	oldDir, oldName := split(oldpath)
	newDir, newName := split(newpath)
	if !representable(newName) {
		return -fuse.EINVAL
	}
	ctx, cancel := durable(f.ctx)
	defer cancel()
	oldParent, err := f.fs.StatPath(ctx, oldDir)
	if err != nil {
		return errc(err)
	}
	newParent, err := f.fs.StatPath(ctx, newDir)
	if err != nil {
		return errc(err)
	}
	return errc(f.fs.Rename(ctx, oldParent.Ino, oldName, newParent.Ino, newName))
}

// clean normalises a WinFsp path (already forward-slashed) to the absolute
// virtual path vfs expects.
func clean(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return path.Clean("/" + p)
}

// split returns the parent directory and base name of a path.
func split(p string) (dir, name string) {
	c := clean(p)
	return path.Dir(c), path.Base(c)
}

// durable detaches a commit from a cancellation so a close that must persist is
// not abandoned midway. It mirrors fusefs.durable.
func durable(ctx context.Context) (context.Context, context.CancelFunc) {
	// vfs.FromKernel tells the VFS this change came from the mount itself, so it
	// does not turn around and invalidate what the kernel already made
	// consistent — the same marking fusefs.durable applies. It matters once
	// host.Notify is wired (winfs.go); marking now keeps the two adapters
	// consistent and avoids a silent invalidation storm later.
	return context.WithTimeout(vfs.FromKernel(context.WithoutCancel(ctx)), 5*time.Minute)
}
