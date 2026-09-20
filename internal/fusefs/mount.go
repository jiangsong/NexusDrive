//go:build !windows

package fusefs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Mount is a live FUSE mount.
type Mount struct {
	server *fuse.Server
	root   *Root
	// rootNode is the inode handed to go-fuse; the kernel-side tree hangs
	// off it, which DropKernelCaches walks.
	rootNode         *node
	path             string
	done             <-chan struct{}
	bulkInvalidation coalescedInvalidation
}

// MountOptions configures Mount.
type MountOptions struct {
	Options
	// Path is the mount point. It must exist and be empty.
	Path string
	// AllowOther exposes the mount to other users. It needs
	// user_allow_other in /etc/fuse.conf.
	AllowOther bool
	// Debug logs the FUSE protocol.
	Debug bool
	// ReadOnly mounts with -o ro.
	ReadOnly bool
}

// MountFS mounts the VFS at the given path and starts serving. The caller
// unmounts with Mount.Unmount.
func MountFS(opt MountOptions) (*Mount, error) {
	if opt.FS == nil {
		return nil, fmt.Errorf("fusefs: no VFS given")
	}
	info, err := os.Stat(opt.Path)
	if err != nil {
		return nil, fmt.Errorf("fusefs: mount point %s: %w", opt.Path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fusefs: mount point %s is not a directory", opt.Path)
	}
	if opt.UID == 0 && opt.GID == 0 {
		opt.UID = uint32(os.Getuid())
		opt.GID = uint32(os.Getgid())
	}
	root := New(opt.Options)
	fsOpts := root.MountOptions("cloudfs", opt.Debug)
	fsOpts.MountOptions.AllowOther = opt.AllowOther
	if opt.ReadOnly {
		fsOpts.MountOptions.Options = append(fsOpts.MountOptions.Options, "ro")
	}

	rootNode := root.RootNode()
	raw := root.rawFS(rootNode, fsOpts)
	server, err := fuse.NewServer(raw, opt.Path, &fsOpts.MountOptions)
	if err != nil {
		root.backings.close()
		return nil, fmt.Errorf("fusefs: mount %s: %w", opt.Path, err)
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return nil, fmt.Errorf("fusefs: wait for mount %s: %w", opt.Path, err)
	}
	root.adoptKernelCapabilities(server.KernelSettings())
	m := &Mount{server: server, root: root, rootNode: rootNode, path: opt.Path, done: raw.done}
	// Kernel cache invalidation: when the VFS changes the tree, tell the
	// kernel so a long attribute timeout and a cached directory stream stay
	// safe. Wired here rather than by the caller so no mount can forget it.
	opt.FS.SetInvalidate(m.InvalidateFunc())
	opt.FS.SetInvalidateEntry(m.InvalidateEntryFunc())
	opt.FS.SetInvalidateAll(func() {
		m.bulkInvalidation.schedule(func() {
			select {
			case <-m.done:
				return
			default:
			}
			m.DropKernelCaches()
		})
	})
	return m, nil
}

// adoptKernelCapabilities reads the INIT handshake and turns on the paths that
// compensate for what this kernel did not offer.
//
// The gate is the capability, never the operating system. macFUSE is what
// lacks CAP_READDIRPLUS today (FUSE 7.19; the capability arrived in 7.21), but
// "macOS" is not the invariant — a macFUSE that catches up, or Fuse-T, or a
// Linux kernel too old for it, each decide this for themselves, and the
// handshake is the only place that knows. go-fuse can only keep a capability
// the kernel proposed, so what INIT says is final.
//
// It runs after WaitMount, when the handshake has completed, which is why the
// state it sets is atomic: the serving goroutine is already accepting
// requests. A request that arrives first simply takes the uncompensated path.
// Settings this cannot read at all are treated as "not offered": the
// compensation is safe either way, and being wrong the other way is silent
// cost.
func (r *Root) adoptKernelCapabilities(settings *fuse.InitIn) {
	offered := settings != nil && settings.Flags64()&fuse.CAP_READDIRPLUS != 0
	r.readdirplus.Store(offered)
	if !offered {
		r.entries.Store(newEntryCache(r.opt.EntryTimeout))
	}
	if settings != nil {
		r.protoMajor.Store(settings.Major)
		r.protoMinor.Store(settings.Minor)
	}
}

// KernelProtocol is the FUSE protocol version negotiated at INIT, or 0.0 if
// the handshake could not be read. Operations that have no capability bit are
// gated on it.
func (m *Mount) KernelProtocol() (major, minor uint32) {
	return m.root.protoMajor.Load(), m.root.protoMinor.Load()
}

// lseekForwarded reports whether a kernel speaking this protocol version can
// forward SEEK_DATA and SEEK_HOLE to the daemon at all. FUSE_LSEEK was added
// in protocol 7.24; below that the kernel answers the syscall itself with
// ENOTTY and file.Lseek is never reached. There is no INIT capability bit for
// it, so the version is the only thing to test.
func lseekForwarded(major, minor uint32) bool {
	return major > 7 || (major == 7 && minor >= 24)
}

// At most one full invalidation runs at a time. A request arriving during a
// traversal schedules another traversal, so already visited entries cannot
// miss a later change. Request goroutines only update bounded state.
type coalescedInvalidation struct {
	mu               sync.Mutex
	running, pending bool
}

func (c *coalescedInvalidation) schedule(run func()) {
	c.mu.Lock()
	c.pending = true
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	go func() {
		for {
			c.mu.Lock()
			if !c.pending {
				c.running = false
				c.mu.Unlock()
				return
			}
			c.pending = false
			c.mu.Unlock()
			run()
		}
	}()
}

// Wait blocks until the filesystem is unmounted.
func (m *Mount) Wait() {
	m.server.Wait()
	// Server.Wait waits for request loops, before closing the connection and
	// calling OnUnmount. Do not return while registered cache leases remain.
	if m.done != nil {
		<-m.done
	}
}

// Unmount detaches the filesystem, retrying briefly while it is busy.
func (m *Mount) Unmount() error {
	var err error
	for i := 0; i < 20; i++ {
		if err = m.server.Unmount(); err == nil {
			if m.done != nil {
				<-m.done
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("fusefs: unmount %s: %w", m.path, err)
}

// Path returns the mount point.
func (m *Mount) Path() string { return m.path }

// OpStats reports how many requests of each kind the kernel has sent this
// mount, and the READ size histogram.
func (m *Mount) OpStats() OpStats { return m.root.snapshot() }

// InvalidateFunc returns a callback for vfs.Options.OnInvalidate that drops the
// kernel's cached attributes, pages and directory stream for a changed inode.
func (m *Mount) InvalidateFunc() func(ino uint64) {
	return func(ino uint64) {
		// Sent from a separate goroutine: the VFS calls this from inside the
		// request that made the change, and a notification about the same
		// inode issued while the kernel still holds that inode's lock can
		// wait on itself. Local changes are already covered by the kernel
		// (it drops its own directory cache on create/unlink/rename); this
		// carries the ones that arrived from the backend or another
		// adapter. The inode is resolved to the node the kernel holds
		// (kernel_nodes.go); one the kernel never looked up needs nothing.
		go m.root.notifyContent(ino)
	}
}

// InvalidateEntryFunc returns a callback that drops one name from the
// kernel's dentry cache: a file that vanished or appeared on the backend must
// not keep answering, or keep failing, from the kernel for the whole entry
// timeout.
func (m *Mount) InvalidateEntryFunc() func(parent uint64, name string) {
	return func(parent uint64, name string) {
		go m.root.notifyEntry(parent, name)
	}
}

// DropKernelCaches tells the kernel to forget every entry and every page it
// holds for this mount. Together with vfs.FS.DropCaches it turns a live mount
// back into a cold one, which is what a benchmark needs and what deleting the
// cache directory alone cannot do: with 30-second entry and attribute
// timeouts the kernel would keep answering lookups by itself. It returns the
// number of entries invalidated.
func (m *Mount) DropKernelCaches() int {
	// The listing entry cache is the other half of "answer without asking".
	// A cold measurement that left it standing would measure a warm mount.
	m.root.forgetEntries()
	if m.rootNode == nil {
		return 0
	}
	return dropInode(&m.rootNode.Inode)
}

// ReaddirplusOffered reports whether the kernel offered CAP_READDIRPLUS at
// INIT — that is, whether it takes a directory's entries and their attributes
// in one reply or sends a LOOKUP per entry. The cost of a tree walk differs by
// an order of magnitude between the two, so a benchmark has to say which one
// it measured.
func (m *Mount) ReaddirplusOffered() bool { return m.root.readdirplus.Load() }

func dropInode(in *fs.Inode) int {
	n := 0
	for name, child := range in.Children() {
		n += dropInode(child)
		if !child.IsDir() {
			// Page cache for the file; ignored when the kernel holds none.
			_ = child.NotifyContent(0, -1)
		}
		_ = in.NotifyEntry(name)
		n++
	}
	return n
}

// Supported reports whether this build and host can mount a FUSE filesystem,
// with a reason when it cannot. `cloudfs doctor` uses it.
func Supported() (bool, string) { return checkPlatform() }

// Platform names the FUSE implementation this build targets.
func Platform() string { return platformName }

// verifyMountable is a cheap pre-flight used by `cloudfs mount` so a failure
// reports a clear cause instead of a kernel errno.
func verifyMountable(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("fusefs: mount point %s: %w", path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("fusefs: mount point %s is not empty", path)
	}
	return nil
}

// VerifyMountable is the exported pre-flight check.
func VerifyMountable(path string) error { return verifyMountable(path) }

// ServeBackground mounts and serves until ctx is cancelled, then unmounts.
func ServeBackground(ctx context.Context, opt MountOptions) (*Mount, error) {
	m, err := MountFS(opt)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = m.Unmount()
	}()
	return m, nil
}
