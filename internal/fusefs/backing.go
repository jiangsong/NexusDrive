//go:build !windows

package fusefs

import (
	"sync"
	"syscall"

	"cloudfs/internal/cache"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type backingPeer interface {
	RegisterBackingFd(*fuse.BackingMap) (int32, syscall.Errno)
	UnregisterBackingFd(int32) syscall.Errno
}

// backingLease changes ownership from a file handle to the kernel registration
// when RegisterBackingFd succeeds. go-fuse shares that registration across an
// inode's opens; closing the first file handle must not release its cache lease.
type backingLease struct {
	file *cache.WholeFile
	id   int32
	// inflight counts the splice results handed to go-fuse that still name
	// file's descriptor. go-fuse splices a ReadResultFd after the Read
	// handler has returned, so a release that arrives while one is
	// outstanding — another read on the handle noticing a sibling writer —
	// must wait: dropped records it, and the last Done completes it.
	inflight int
	dropped  bool
}

type backingRegistry struct {
	fs.ServerCallbacks
	mu         sync.Mutex
	peer       backingPeer
	closed     bool
	wired      bool
	offers     map[int32]*backingLease
	registered map[int32]*backingLease
}

func newBackingRegistry() *backingRegistry {
	return &backingRegistry{offers: map[int32]*backingLease{}, registered: map[int32]*backingLease{}}
}

func (b *backingRegistry) offer(f *cache.WholeFile) *backingLease {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := &backingLease{file: f}
	if b.closed || !b.wired {
		f.Close()
		l.file = nil
		return l
	}
	b.offers[int32(f.Fd())] = l
	return l
}

func (b *backingRegistry) fd(l *backingLease) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || l.file == nil {
		return 0, false
	}
	return int(l.file.Fd()), true
}

// acquire hands out the lease's descriptor for one splice result and keeps
// the file open until the matching release.
func (b *backingRegistry) acquire(l *backingLease) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || l.file == nil {
		return 0, false
	}
	l.inflight++
	return int(l.file.Fd()), true
}

// release is the end of one splice result's use of the descriptor.
func (b *backingRegistry) release(l *backingLease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l.inflight--
	if l.inflight == 0 && l.dropped && l.file != nil && l.id == 0 {
		l.file.Close()
		l.file = nil
	}
}

// releaseOffer releases only a descriptor which the kernel never registered.
// The offer is withdrawn at once; the descriptor closes once no splice
// result names it any more.
func (b *backingRegistry) releaseOffer(l *backingLease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.file == nil || l.id != 0 {
		return
	}
	delete(b.offers, int32(l.file.Fd()))
	if l.inflight > 0 {
		l.dropped = true
		return
	}
	l.file.Close()
	l.file = nil
}

func (b *backingRegistry) RegisterBackingFd(m *fuse.BackingMap) (int32, syscall.Errno) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.offers[m.Fd]
	if b.closed || l == nil || l.file == nil {
		return 0, syscall.EBADF
	}
	delete(b.offers, m.Fd)
	var id int32
	errno := syscall.ENOTSUP
	if b.peer != nil {
		id, errno = b.peer.RegisterBackingFd(m)
	}
	if errno != 0 || id <= 0 {
		l.file.Close()
		l.file = nil
		if errno == 0 {
			errno = syscall.EIO
		}
		return 0, errno
	}
	l.id = id
	b.registered[id] = l
	return id, 0
}

func (b *backingRegistry) UnregisterBackingFd(id int32) syscall.Errno {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.registered[id]
	if l == nil {
		return syscall.ENOENT
	}
	errno := b.peer.UnregisterBackingFd(id)
	if errno != 0 && errno != syscall.ENOENT {
		// go-fuse drops its ID even on an ioctl error. Keep the cache charge
		// until disconnect, when the kernel connection releases all backing IDs.
		return errno
	}
	delete(b.registered, id)
	l.file.Close()
	l.file = nil
	return 0
}

// close runs only after the FUSE connection has closed, not merely when its
// last request-reading goroutine exits. See fuse.Server.Serve / OnUnmount.
func (b *backingRegistry) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, group := range []map[int32]*backingLease{b.offers, b.registered} {
		for id, l := range group {
			if l.file != nil {
				l.file.Close()
				l.file = nil
			}
			delete(group, id)
		}
	}
}

type backingFS struct {
	fuse.RawFileSystem
	backings    *backingRegistry
	done        chan struct{}
	unmountOnce sync.Once
}

func (r *Root) rawFS(root *node, opt *fs.Options) *backingFS {
	r.backings.wired = true
	r.backings.ServerCallbacks = opt.ServerCallbacks
	if peer, ok := opt.ServerCallbacks.(backingPeer); ok {
		r.backings.peer = peer
	}
	opt.ServerCallbacks = r.backings
	return &backingFS{RawFileSystem: fs.NewNodeFS(root, opt), backings: r.backings, done: make(chan struct{})}
}

func (r *backingFS) Init(s *fuse.Server) {
	r.backings.ServerCallbacks = s
	r.backings.peer, _ = any(s).(backingPeer)
	// go-fuse v2.11's rawBridge.Init only overwrites ServerCallbacks with s.
	// NewNodeFS already received our callback proxy: do not overwrite it.
}

func (r *backingFS) OnUnmount() {
	r.unmountOnce.Do(func() {
		defer close(r.done)
		defer r.backings.close()
		r.RawFileSystem.OnUnmount()
	})
}
