//go:build !windows

// Package fusefs adapts the VFS to the kernel through go-fuse. It is a thin
// translation layer: inode identity, attribute and entry timeouts, readdirplus
// and errno mapping live here; every decision about caching, consistency and
// uploads stays in internal/vfs (docs/DESIGN.md §4.6).
package fusefs

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Options configures the adapter.
type Options struct {
	FS *vfs.FS
	// AttrTimeout and EntryTimeout are how long the kernel may trust an
	// attribute or a name. Writes invalidate actively, so these can be
	// generous.
	AttrTimeout  time.Duration
	EntryTimeout time.Duration
	// KernelDirCache lets the kernel keep directory streams
	// (FOPEN_CACHE_DIR). On by default; CLOUDFS_NO_DIRCACHE=1 turns it off
	// as an escape hatch when a listing looks stale.
	KernelDirCache bool
	// NegativeTimeout caches "no such file" in the kernel.
	NegativeTimeout time.Duration
	// UID and GID own every file. Zero means the current process owner.
	UID, GID uint32
}

// Root holds the shared adapter configuration. The kernel-facing root is an
// ordinary node built by RootNode, so the mount point behaves exactly like any
// other directory in the tree.
type Root struct {
	opt  Options
	node uint64
	// passthrough is whether hydrated files may be handed to the kernel.
	passthrough bool
	backings    *backingRegistry
	// opCounters tallies every request this process serves; passthrough
	// reads never show up here, which is how a test can tell the two apart.
	opCounters
}

// New builds the root node for mounting.
func New(opt Options) *Root {
	if opt.AttrTimeout == 0 {
		opt.AttrTimeout = 30 * time.Second
	}
	if opt.EntryTimeout == 0 {
		opt.EntryTimeout = 30 * time.Second
	}
	opt.KernelDirCache = os.Getenv("CLOUDFS_NO_DIRCACHE") == ""
	if opt.NegativeTimeout == 0 {
		opt.NegativeTimeout = 5 * time.Second
	}
	ok, _ := PassthroughEnabled()
	return &Root{opt: opt, node: 1, passthrough: ok, backings: newBackingRegistry()}
}

// MountOptions returns the transfer limits and timeouts for go-fuse. Kernel
// writeback_cache is not enabled; it conflicts with passthrough negotiation.
func (r *Root) MountOptions(name string, debug bool) *fs.Options {
	o := &fs.Options{
		AttrTimeout:     &r.opt.AttrTimeout,
		EntryTimeout:    &r.opt.EntryTimeout,
		NegativeTimeout: &r.opt.NegativeTimeout,
	}
	o.MountOptions = fuse.MountOptions{
		FsName:        name,
		Name:          "cloudfs",
		Debug:         debug,
		DisableXAttrs: false,
		EnableLocks:   true,
	}
	// Sizes and extra flags differ per platform: macFUSE negotiates a much
	// smaller maximum write than Linux, and Finder needs a volume name.
	applyPlatformOptions(&o.MountOptions)
	return o
}

// node is a file or directory in the tree, identified by the VFS inode.
type node struct {
	fs.Inode
	root *Root
	ino  uint64
}

var (
	_ fs.NodeGetattrer      = (*node)(nil)
	_ fs.NodeLookuper       = (*node)(nil)
	_ fs.NodeReaddirer      = (*node)(nil)
	_ fs.NodeOpendirHandler = (*node)(nil)
	_ fs.NodeOpener         = (*node)(nil)
	_ fs.NodeCreater        = (*node)(nil)
	_ fs.NodeMkdirer        = (*node)(nil)
	_ fs.NodeUnlinker       = (*node)(nil)
	_ fs.NodeRmdirer        = (*node)(nil)
	_ fs.NodeRenamer        = (*node)(nil)
	_ fs.NodeSetattrer      = (*node)(nil)
	_ fs.NodeStatfser       = (*node)(nil)
	_ fs.NodeGetxattrer     = (*node)(nil)
)

// Ino returns the VFS inode of the root.
func (r *Root) Ino() uint64 { return r.node }

// RootNode returns the root inode. MountFS wires it through rawFS so backing
// registrations retain their cache leases; bare fs.Mount lacks that proxy.
func (r *Root) RootNode() *node { return &node{root: r, ino: r.node} }

// newNode wraps a VFS inode as a kernel node.
func (r *Root) newNode(ino uint64, isDir bool) (*node, fs.StableAttr) {
	mode := uint32(syscall.S_IFREG)
	if isDir {
		mode = syscall.S_IFDIR
	}
	return &node{root: r, ino: ino}, fs.StableAttr{Mode: mode, Ino: ino}
}

// errno maps VFS errors to kernel error numbers.
// debugErrno makes every error that falls through to EIO visible in the log;
// EIO is the kernel's word for "something else", and the something else is
// what a bug report needs.
var debugErrno = os.Getenv("CLOUDFS_DEBUG_ERRNO") != ""

func errno(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, vfs.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, vfs.ErrExists):
		return syscall.EEXIST
	case errors.Is(err, vfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, vfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, vfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, vfs.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, vfs.ErrUploadCancelled), errors.Is(err, vfs.ErrUploadPurging):
		return syscall.EBUSY
	case errors.Is(err, vfs.ErrNoSpace), errors.Is(err, syscall.ENOSPC):
		return syscall.ENOSPC
	case errors.Is(err, vfs.ErrCrossMount):
		return syscall.EXDEV
	case errors.Is(err, provider.ErrUnsupported):
		return syscall.ENOTSUP
	case errors.Is(err, provider.ErrAuth):
		return syscall.EACCES
	case errors.Is(err, provider.ErrRiskControl), errors.Is(err, provider.ErrRateLimited):
		return syscall.EAGAIN
	case errors.Is(err, provider.ErrUnavailable):
		// The data exists; nothing holding it can be reached right now.
		// EHOSTDOWN tells a shell exactly that, where EIO would say the
		// file is broken.
		return syscall.EHOSTDOWN
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return syscall.EINTR
	default:
		if debugErrno {
			log.Printf("fusefs: unmapped error reported as EIO: %v", err)
		}
		return syscall.EIO
	}
}

func (r *Root) fillAttr(a *fuse.Attr, at vfs.Attr) {
	a.Ino = at.Ino
	a.Size = uint64(at.Size)
	a.Blocks = uint64((at.Size + 511) / 512)
	mode := at.Mode
	if mode == 0 {
		if at.IsDir {
			mode = 0o755
		} else {
			mode = 0o644
		}
	}
	if at.IsDir {
		a.Mode = syscall.S_IFDIR | mode
		a.Nlink = 2
	} else {
		a.Mode = syscall.S_IFREG | mode
		a.Nlink = 1
	}
	secs := uint64(at.MTime.Unix())
	nsecs := uint32(at.MTime.Nanosecond())
	a.Mtime, a.Mtimensec = secs, nsecs
	a.Ctime, a.Ctimensec = secs, nsecs
	a.Atime, a.Atimensec = secs, nsecs
	a.Owner.Uid = r.opt.UID
	a.Owner.Gid = r.opt.GID
}

// vfsIno returns the VFS inode this kernel node stands for.
func (n *node) vfsIno() uint64 { return n.ino }

func (n *node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.root.count(opGetattr)
	at, err := n.root.opt.FS.Stat(ctx, n.vfsIno())
	if err != nil {
		return errno(err)
	}
	n.root.fillAttr(&out.Attr, at)
	out.SetTimeout(n.root.opt.AttrTimeout)
	return 0
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.root.count(opLookup)
	at, err := n.root.opt.FS.Lookup(ctx, n.vfsIno(), name)
	if err != nil {
		return nil, errno(err)
	}
	child, stable := n.root.newNode(at.Ino, at.IsDir)
	inode := n.NewInode(ctx, child, stable)
	n.root.fillAttr(&out.Attr, at)
	out.SetEntryTimeout(n.root.opt.EntryTimeout)
	out.SetAttrTimeout(n.root.opt.AttrTimeout)
	return inode, 0
}

// OpendirHandle reads the directory once and hands the kernel a handle it
// may keep.
//
// The handle serves the entries itself: with this hook go-fuse does not wrap
// a nil handle, and a handle without Readdirent answers every readdir with
// nothing. It numbers its entries and supports Seekdir, which the kernel
// needs to page through a cached directory.
//
// It also answers readdirplus lookups from the listing it already holds
// (fs.FileLookuper). Without that go-fuse asks the node for every entry, and
// on a 2,041-entry tree those lookups cost more than the listing itself.
//
// FOPEN_CACHE_DIR is what makes a warm walk cheap: without it every readdir
// crosses into this process at ~150 µs per entry, against ~7 µs when the
// kernel answers from its own cache. The cache is safe because the kernel
// drops it itself on every create, unlink and rename made through the mount,
// and the VFS invalidates the inode for changes that arrive from the backend.
func (n *node) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.root.count(opOpendir)
	// The listing is built on first use, not here: when the kernel serves
	// the directory from its own cache it opens the handle and never reads
	// from it, and building a 50-entry listing for each of those opens was
	// most of a warm tree walk.
	h := &dirHandle{n: n}
	if !n.root.opt.KernelDirCache {
		return h, 0, 0
	}
	return h, fuse.FOPEN_CACHE_DIR | fuse.FOPEN_KEEP_CACHE, 0
}

// Readdir builds a plain directory stream; go-fuse only calls it when
// OpendirHandle is bypassed.
func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	n.root.count(opReaddir)
	entries, err := n.root.opt.FS.ReadDir(ctx, n.vfsIno())
	if err != nil {
		return nil, errno(err)
	}
	list := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: entryMode(e), Ino: e.Ino})
	}
	return fs.NewListDirStream(list), 0
}

func entryMode(e vfs.Attr) uint32 {
	if e.IsDir {
		return syscall.S_IFDIR
	}
	return syscall.S_IFREG
}

// dirHandle is an open directory: the listing, taken on the first read, a
// cursor, and enough to answer readdirplus for each entry without another
// trip to the metadata store.
type dirHandle struct {
	n       *node
	entries []vfs.Attr
	loaded  bool
	idx     int
}

// load fetches the listing once.
func (d *dirHandle) load(ctx context.Context) syscall.Errno {
	if d.loaded {
		return 0
	}
	d.n.root.count(opReaddir)
	entries, err := d.n.root.opt.FS.ReadDir(ctx, d.n.vfsIno())
	if err != nil {
		return errno(err)
	}
	d.entries, d.loaded = entries, true
	return 0
}

var (
	_ fs.FileReaddirenter = (*dirHandle)(nil)
	_ fs.FileSeekdirer    = (*dirHandle)(nil)
	_ fs.FileReleasedirer = (*dirHandle)(nil)
	_ fs.FileLookuper     = (*dirHandle)(nil)
)

func (d *dirHandle) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	if errno := d.load(ctx); errno != 0 {
		return nil, errno
	}
	if d.idx >= len(d.entries) {
		return nil, 0
	}
	e := d.entries[d.idx]
	d.idx++
	return &fuse.DirEntry{Name: e.Name, Mode: entryMode(e), Ino: e.Ino, Off: uint64(d.idx)}, 0
}

func (d *dirHandle) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if errno := d.load(ctx); errno != 0 {
		return errno
	}
	if off > uint64(len(d.entries)) {
		return syscall.EINVAL
	}
	d.idx = int(off)
	return 0
}

func (d *dirHandle) Releasedir(ctx context.Context, releaseFlags uint32) {}

// Lookup answers readdirplus for the entry Readdirent just produced. go-fuse
// promises name is that entry; anything else falls back to the node.
func (d *dirHandle) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if d.idx > 0 && d.idx <= len(d.entries) && d.entries[d.idx-1].Name == name {
		d.n.root.count(opDirLookup)
		at := d.entries[d.idx-1]
		child, stable := d.n.root.newNode(at.Ino, at.IsDir)
		inode := d.n.NewInode(ctx, child, stable)
		d.n.root.fillAttr(&out.Attr, at)
		out.SetEntryTimeout(d.n.root.opt.EntryTimeout)
		out.SetAttrTimeout(d.n.root.opt.AttrTimeout)
		return inode, 0
	}
	return d.n.Lookup(ctx, name, out)
}

// file is an open handle.
type file struct {
	root   *Root
	handle *vfs.Handle
	// pass is an offer; a successful kernel registration owns its lease until
	// that shared backing ID is unregistered, independently of this handle.
	pass     *backingLease
	passMu   sync.Mutex
	released bool
}

var (
	_ fs.FileReader          = (*file)(nil)
	_ fs.FileWriter          = (*file)(nil)
	_ fs.FileFlusher         = (*file)(nil)
	_ fs.FileReleaser        = (*file)(nil)
	_ fs.FileFsyncer         = (*file)(nil)
	_ fs.FileGetattrer       = (*file)(nil)
	_ fs.FilePassthroughFder = (*file)(nil)
)

// PassthroughFd hands the kernel a descriptor on the fully cached copy of the
// file, so reads bypass this process entirely (FUSE passthrough, Linux 6.9+).
//
// It is offered only for a read handle on a file whose every block is
// present and whose content is not a pending local write: those two rules
// gate the initial offer. go-fuse reuses an inode's backing for later opens
// without consulting this method again: mixed IO modes and version changes
// remain unresolved, so this path requires an explicit experimental opt-in.
func (f *file) PassthroughFd() (int, bool) {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if f.released {
		return 0, false
	}
	if f.pass != nil {
		return f.root.backings.fd(f.pass)
	}
	if !f.root.passthrough {
		return 0, false
	}
	pf, err := f.root.opt.FS.OpenLocal(f.handle)
	if err != nil {
		return 0, false
	}
	f.pass = f.root.backings.offer(pf)
	return f.root.backings.fd(f.pass)
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.root.count(opOpen)
	write := flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
	if write {
		var cancel context.CancelFunc
		ctx, cancel = durable(ctx)
		defer cancel()
	}
	h, err := n.root.opt.FS.Open(ctx, n.vfsIno(), write)
	if err != nil {
		return nil, 0, errno(err)
	}
	if flags&syscall.O_TRUNC != 0 && write {
		if err := n.root.opt.FS.Truncate(ctx, h, 0); err != nil {
			n.root.opt.FS.Release(ctx, h)
			return nil, 0, errno(err)
		}
	}
	return &file{root: n.root, handle: h}, 0, 0
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	n.root.count(opCreate)
	ctx, cancel := durable(ctx)
	defer cancel()
	h, err := n.root.opt.FS.Create(ctx, n.vfsIno(), name)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	at := n.root.opt.FS.HandleAttr(ctx, h)
	child, stable := n.root.newNode(at.Ino, false)
	inode := n.NewInode(ctx, child, stable)
	n.root.fillAttr(&out.Attr, at)
	out.SetEntryTimeout(n.root.opt.EntryTimeout)
	out.SetAttrTimeout(n.root.opt.AttrTimeout)
	return inode, &file{root: n.root, handle: h}, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.root.count(opMkdir)
	ctx, cancel := durable(ctx)
	defer cancel()
	at, err := n.root.opt.FS.Mkdir(ctx, n.vfsIno(), name)
	if err != nil {
		return nil, errno(err)
	}
	child, stable := n.root.newNode(at.Ino, true)
	inode := n.NewInode(ctx, child, stable)
	n.root.fillAttr(&out.Attr, at)
	out.SetEntryTimeout(n.root.opt.EntryTimeout)
	out.SetAttrTimeout(n.root.opt.AttrTimeout)
	return inode, 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	n.root.count(opUnlink)
	ctx, cancel := durable(ctx)
	defer cancel()
	return errno(n.root.opt.FS.Remove(ctx, n.vfsIno(), name, false))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.root.count(opRmdir)
	ctx, cancel := durable(ctx)
	defer cancel()
	return errno(n.root.opt.FS.Remove(ctx, n.vfsIno(), name, false))
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.root.count(opRename)
	ctx, cancel := durable(ctx)
	defer cancel()
	target, ok := newParent.(*node)
	if !ok {
		// Every directory in this filesystem is a *node, so anything else is
		// a different filesystem: the kernel expects EXDEV.
		return syscall.EXDEV
	}
	return errno(n.root.opt.FS.Rename(ctx, n.vfsIno(), name, target.vfsIno(), newName))
}

func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.root.count(opSetattr)
	ctx, cancel := durable(ctx)
	defer cancel()
	if sz, ok := in.GetSize(); ok {
		f, isFile := fh.(*file)
		if !isFile {
			// No descriptor came with the truncate. The VFS applies it to the
			// write handles already open on this inode rather than opening one
			// of its own, because a second handle means a second staging file
			// and two competing snapshots of the same write.
			if err := n.root.opt.FS.TruncatePath(ctx, n.vfsIno(), int64(sz)); err != nil {
				return errno(err)
			}
		} else if err := n.root.opt.FS.Truncate(ctx, f.handle, int64(sz)); err != nil {
			return errno(err)
		}
	}
	// Mode, owner and times are not propagated to the remotes: cloud drives
	// have no POSIX permission model. Report the current attributes so the
	// caller sees a consistent result rather than an error.
	at, err := n.root.opt.FS.Stat(ctx, n.vfsIno())
	if err != nil {
		return errno(err)
	}
	n.root.fillAttr(&out.Attr, at)
	out.SetTimeout(n.root.opt.AttrTimeout)
	return 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.root.count(opStatfs)
	const bsize = 4096
	out.Bsize = bsize
	out.Frsize = bsize
	out.NameLen = 255
	// When the backends can say how much space they have — a pool's
	// members together, a drive's quota — df shows that: it is the space a
	// user is buying more of. The cache budget still decides ENOSPC.
	if sp := n.root.opt.FS.Space(ctx); sp.Known {
		out.Blocks = uint64(sp.Total) / bsize
		out.Bfree = uint64(sp.Free()) / bsize
		out.Bavail = out.Bfree
		return 0
	}
	// Otherwise report the cache budget: that is what actually limits a
	// write before the upload queue drains.
	st := n.root.opt.FS.Cache().Stats()
	used := uint64(st.Bytes) / bsize
	// A large notional capacity keeps tools from refusing to write; the real
	// backpressure is ENOSPC from the cache.
	total := used + (1 << 40 / bsize)
	out.Blocks = total
	out.Bfree = total - used
	out.Bavail = out.Bfree
	return 0
}

// Getxattr exposes cloudfs state so a user can see whether a file is uploaded
// without leaving the shell: getfattr -n user.cloudfs.state <path>.
func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	n.root.count(opGetxattr)
	// The kernel probes security.* and system.* on every create and write;
	// answer those without touching the metadata store.
	if !strings.HasPrefix(attr, "user.cloudfs.") {
		return 0, syscall.ENODATA
	}
	at, err := n.root.opt.FS.Stat(ctx, n.vfsIno())
	if err != nil {
		return 0, errno(err)
	}
	var val string
	switch attr {
	case "user.cloudfs.state":
		if at.LocalOnly {
			val = "local"
		} else {
			val = "synced"
		}
	case "user.cloudfs.remote":
		val = at.Remote
	case "user.cloudfs.cached":
		val = formatPercent(at.Cached)
	default:
		return 0, syscall.ENODATA
	}
	if len(dest) < len(val) {
		return uint32(len(val)), syscall.ERANGE
	}
	return uint32(copy(dest, val)), 0
}

func formatPercent(f float64) string {
	pct := int(f*100 + 0.5)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return strconv.Itoa(pct) + "%"
}

func (f *file) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.root.count(opRead)
	f.root.countRead(len(dest))
	n, err := f.root.opt.FS.Read(ctx, f.handle, dest, off)
	if err != nil && n == 0 {
		if isEOF(err) {
			return fuse.ReadResultData(nil), 0
		}
		return nil, errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) }

func (f *file) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.root.count(opWrite)
	n, err := f.root.opt.FS.Write(ctx, f.handle, data, off)
	if err != nil {
		return uint32(n), errno(err)
	}
	return uint32(n), 0
}

// Flush runs on close(2), and unlike RELEASE the kernel waits for its reply.
// The commit has to happen here so a caller whose close() returned can read
// back what it wrote and sees any error that prevented it.
//
// FLUSH can arrive more than once on the same handle, and before further
// writes: the kernel sends it for every descriptor that closes, including
// duplicates, which is exactly what a shell redirection does. So this commits
// the staged data but keeps the handle writable; RELEASE does the teardown.
func (f *file) Flush(ctx context.Context) syscall.Errno {
	f.root.count(opFlush)
	ctx, cancel := durable(ctx)
	defer cancel()
	return errno(f.root.opt.FS.Sync(ctx, f.handle))
}

func (f *file) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f.root.count(opFsync)
	ctx, cancel := durable(ctx)
	defer cancel()
	return errno(f.root.opt.FS.Sync(ctx, f.handle))
}

// Release runs after the kernel has dropped the handle. Committing already
// happened in Flush; FS.Release is idempotent, so this only cleans up handles
// the kernel closed without flushing.
func (f *file) Release(ctx context.Context) syscall.Errno {
	f.root.count(opRelease)
	ctx, cancel := durable(ctx)
	defer cancel()
	f.passMu.Lock()
	f.released = true
	if f.pass != nil {
		f.root.backings.releaseOffer(f.pass)
		f.pass = nil
	}
	f.passMu.Unlock()
	return errno(f.root.opt.FS.Release(ctx, f.handle))
}

// durable detaches an operation from the interrupt that may arrive while it
// runs, keeping only a ceiling on how long it may take. Every operation that
// changes the tree runs under it: a create, mkdir, unlink, rename or truncate
// abandoned halfway is a worse outcome than one that takes its time, and a
// Go caller preempted by SIGURG mid-syscall would otherwise see its create
// fail at random.
//
// The kernel cancels a FUSE request when the calling thread takes a signal,
// and go-fuse reports that by cancelling the request context. For a read that
// is right: the caller will retry. For a commit it is not. Go programs are
// preempted by SIGURG at arbitrary points, so close(2) on a written file would
// return EINTR at random — and close(2) is not retryable, so the caller has no
// way to recover, while the data it wrote sits uncommitted. Committing to the
// end and answering the original request is both correct and what the caller
// asked for.
func durable(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(vfs.FromKernel(context.WithoutCancel(ctx)), 5*time.Minute)
}

func (f *file) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	at, err := f.root.opt.FS.Stat(ctx, f.handle.Ino)
	if err != nil {
		return errno(err)
	}
	f.root.fillAttr(&out.Attr, at)
	out.SetTimeout(f.root.opt.AttrTimeout)
	return 0
}

// The root is an ordinary node, so no separate set of root operations exists.
