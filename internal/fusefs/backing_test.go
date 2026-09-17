package fusefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// kernelBackingStub retains a duplicate descriptor like a backing registration.
// It does not emulate inode IO-mode negotiation or prove kernel correctness.
type kernelBackingStub struct {
	fs.ServerCallbacks
	mu                             sync.Mutex
	next                           int32
	files                          map[int32]*os.File
	registerErr, unregisterErr     syscall.Errno
	registrations, unregistrations int
}

func (k *kernelBackingStub) RegisterBackingFd(m *fuse.BackingMap) (int32, syscall.Errno) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.registrations++
	if k.registerErr != 0 {
		return 0, k.registerErr
	}
	fd, err := syscall.Dup(int(m.Fd))
	if err != nil {
		return 0, syscall.EBADF
	}
	k.next++
	k.files[k.next] = os.NewFile(uintptr(fd), "kernel-backing")
	return k.next, 0
}

func (k *kernelBackingStub) UnregisterBackingFd(id int32) syscall.Errno {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unregistrations++
	if k.unregisterErr != 0 {
		return k.unregisterErr
	}
	f := k.files[id]
	if f == nil {
		return syscall.ENOENT
	}
	f.Close()
	delete(k.files, id)
	return 0
}

func (k *kernelBackingStub) disconnect() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, f := range k.files {
		f.Close()
		delete(k.files, id)
	}
}

type backingFixture struct {
	raw    *backingFS
	root   *Root
	cache  *cache.Cache
	key    cache.FileKey
	kernel *kernelBackingStub
	nodeID uint64
	fake   *fakeprovider.Fake
}

func newBackingFixture(t *testing.T) *backingFixture {
	return newBackingFixtureWithPolicy(t, true)
}

func newBackingFixtureWithPolicy(t *testing.T, experimental bool) *backingFixture {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 16, MaxBytes: 7, HydrateAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	m, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	p := fakeprovider.New("test")
	entry := p.Seed("f", []byte("content"))
	v, err := vfs.New(vfs.Options{Meta: m, Cache: c, Mounts: []vfs.Mount{{Prefix: "/", Remote: "test", RootID: fakeprovider.RootID, Provider: p, Mode: config.ModeWriteback}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	k := cache.FileKey{Remote: "test", RemoteID: entry.ID, Version: entry.Version}
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.LinkFile(k, src, 7); err != nil {
		t.Fatal(err)
	}
	r := New(Options{FS: v})
	if experimental {
		r.passthrough = true
	} // exercise the real bridge without a mount
	kernel := &kernelBackingStub{files: map[int32]*os.File{}}
	opt := r.MountOptions("test", false)
	opt.ServerCallbacks = kernel
	raw := r.rawFS(r.RootNode(), opt)
	// Exercise the production Init override. The bridge must retain the
	// registry proxy instead of replacing it with a bare server callback.
	raw.Init(nil)
	raw.backings.ServerCallbacks, raw.backings.peer = kernel, kernel
	t.Cleanup(func() { kernel.disconnect(); raw.OnUnmount() })
	var out fuse.EntryOut
	if st := raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, "f", &out); st != fuse.OK {
		t.Fatal(st)
	}
	return &backingFixture{raw: raw, root: r, cache: c, key: k, kernel: kernel, nodeID: out.NodeId, fake: p}
}

func (e *backingFixture) open(t *testing.T) fuse.OpenOut {
	t.Helper()
	var out fuse.OpenOut
	if st := e.raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: e.nodeID}}, &out); st != fuse.OK {
		t.Fatal(st)
	}
	return out
}

func (e *backingFixture) release(out fuse.OpenOut) {
	e.raw.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: e.nodeID}, Fh: out.Fh})
}

func TestBackingLeaseOutlivesTheFirstOfMultipleHandles(t *testing.T) {
	e := newBackingFixture(t)
	first, second := e.open(t), e.open(t)
	if first.BackingID == 0 || first.BackingID != second.BackingID || second.OpenFlags&fuse.FOPEN_PASSTHROUGH == 0 || e.kernel.registrations != 1 {
		t.Fatalf("expected shared backing: %+v %+v", first, second)
	}
	e.release(first)
	e.cache.Forget(e.key)
	if s := e.cache.Stats(); s.Bytes != 7 || s.LeasedBytes != 7 {
		t.Fatalf("first close released live kernel data: %+v", s)
	}
	if err := e.cache.Put(cache.FileKey{Remote: "other", RemoteID: "x"}, 0, []byte("x"), 1); !errors.Is(err, cache.ErrNoSpace) {
		t.Fatalf("spent kernel-held capacity: %v", err)
	}
	b := make([]byte, 7)
	if n, err := e.kernel.files[first.BackingID].ReadAt(b, 0); n != 7 || err != nil || string(b) != "content" {
		t.Fatalf("backing content: %q %v", b, err)
	}
	e.release(second)
	if s := e.cache.Stats(); s.Bytes != 0 || s.LeasedBytes != 0 {
		t.Fatalf("last close leaked data: %+v", s)
	}
	if e.kernel.unregistrations != 1 {
		t.Fatalf("unregistered %d times", e.kernel.unregistrations)
	}
}

func TestBackingRegistrationFailureReleasesOfferAndFallsBack(t *testing.T) {
	e := newBackingFixture(t)
	e.kernel.registerErr = syscall.EPERM
	first := e.open(t)
	if first.BackingID != 0 || first.OpenFlags&fuse.FOPEN_PASSTHROUGH != 0 {
		t.Fatalf("failed registration offered passthrough: %+v", first)
	}
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("failed registration leaked cache lease")
	}
	e.release(first)
	second := e.open(t)
	e.release(second)
	if e.kernel.registrations != 1 {
		t.Fatal("bridge did not retain its unsupported-kernel fallback")
	}
}

func TestFailedBackingUnregistrationRetainsChargeUntilDisconnect(t *testing.T) {
	e := newBackingFixture(t)
	h := e.open(t)
	e.kernel.unregisterErr = syscall.EIO
	e.release(h)
	e.cache.Forget(e.key)
	if s := e.cache.Stats(); s.Bytes != 7 || s.LeasedBytes != 7 {
		t.Fatalf("failed unregister freed kernel-held bytes: %+v", s)
	}
	e.kernel.disconnect()
	e.raw.OnUnmount()
	if s := e.cache.Stats(); s.Bytes != 0 || s.LeasedBytes != 0 {
		t.Fatalf("disconnect leaked bytes: %+v", s)
	}
}

func TestUnmountReleasesRegistrationsWithoutReleaseRequests(t *testing.T) {
	e := newBackingFixture(t)
	e.open(t)
	e.open(t)
	e.cache.Forget(e.key)
	e.kernel.disconnect()
	e.raw.OnUnmount()
	if s := e.cache.Stats(); s.Bytes != 0 || s.LeasedBytes != 0 {
		t.Fatalf("unmount leaked bytes: %+v", s)
	}
}

func TestUnregisteredOfferIsReleasedWithItsHandle(t *testing.T) {
	e := newBackingFixture(t)
	a, err := e.root.opt.FS.StatPath(context.Background(), "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(context.Background(), a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	if _, ok := f.PassthroughFd(); !ok {
		t.Fatal("missing offer")
	}
	if e.cache.Stats().LeasedBytes != 7 {
		t.Fatal("offer not leased")
	}
	f.Release(context.Background())
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("unregistered offer leaked")
	}
	if _, ok := f.PassthroughFd(); ok {
		t.Fatal("closed handle reoffered fd")
	}
}

func TestUnwiredAdapterDoesNotOfferAnUntrackedBacking(t *testing.T) {
	e := newBackingFixture(t)
	r := New(Options{FS: e.root.opt.FS})
	r.passthrough = true
	a, err := r.opt.FS.StatPath(context.Background(), "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.opt.FS.Open(context.Background(), a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: r, handle: h}
	defer f.Release(context.Background())
	if _, ok := f.PassthroughFd(); ok {
		t.Fatal("offered fd without registration proxy")
	}
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("unwired offer leaked a lease")
	}
}

func TestConcurrentBackingOpenReleaseAndGC(t *testing.T) {
	e := newBackingFixture(t)
	anchor := e.open(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				h := e.open(t)
				e.release(h)
				if err := e.cache.GC(); err != nil {
					t.Error(err)
				}
				if s := e.cache.Stats(); s.LeasedBytes != 7 {
					t.Errorf("anchor lease lost: %+v", s)
				}
			}
		}()
	}
	wg.Wait()
	e.release(anchor)
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("anchor lease leaked")
	}
}

// This verifies the temporary default-safety gate, not a solution for mixed
// IO modes in the experimental passthrough path.
func TestDefaultOpenWriteWithExistingReaderStillUsesJournal(t *testing.T) {
	t.Setenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH", "")
	t.Setenv("CLOUDFS_NO_PASSTHROUGH", "")
	e := newBackingFixtureWithPolicy(t, false)
	j, err := journal.Open(journal.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	e.root.opt.FS.SetWriteBackend(j, nil)
	old, err := e.cache.OpenWhole(e.key)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	reader := e.open(t)
	defer e.release(reader)
	var writer fuse.OpenOut
	hdr := fuse.InHeader{NodeId: e.nodeID}
	if st := e.raw.Open(nil, &fuse.OpenIn{InHeader: hdr, Flags: syscall.O_RDWR}, &writer); st != fuse.OK {
		t.Fatal(st)
	}
	defer e.release(writer)
	for _, h := range []fuse.OpenOut{reader, writer} {
		if h.BackingID != 0 || h.OpenFlags&fuse.FOPEN_PASSTHROUGH != 0 {
			t.Fatalf("default mode bypasses VFS: %+v", h)
		}
	}
	if n, st := e.raw.Write(nil, &fuse.WriteIn{InHeader: hdr, Fh: writer.Fh, Size: 7}, []byte("CHANGED")); n != 7 || st != fuse.OK {
		t.Fatalf("write: %d %v", n, st)
	}
	if st := e.raw.Flush(nil, &fuse.FlushIn{InHeader: hdr, Fh: writer.Fh}); st != fuse.OK {
		t.Fatal(st)
	}
	rows, _, err := j.ListActive(context.Background(), "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("write not journaled: %v %v", rows, err)
	}
	got, err := os.ReadFile(rows[0].BlobPath)
	if err != nil || string(got) != "CHANGED" {
		t.Fatalf("journal content: %q %v", got, err)
	}
	buf := make([]byte, 7)
	if _, err := old.ReadAt(buf, 0); err != nil || string(buf) != "content" {
		t.Fatalf("immutable backing mutated: %q %v", buf, err)
	}
}
