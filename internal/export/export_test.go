package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
	"cloudfs/test/poolharness"
)

// testConfig is the export block the tests run with: small ranges so a few
// kilobytes still exercise the chunk loop, no yielding (there is no kernel
// here to yield to), one job at a time.
func testConfig() config.Export {
	c := config.DefaultExport()
	c.RangeSize = 64 << 10
	c.MultiRangeMin = 1 << 30
	c.YieldToForeground = false
	return c
}

func newManager(t *testing.T, fsys *vfs.FS, cfg config.Export) *Manager {
	t.Helper()
	return newManagerAt(t, t.TempDir(), fsys, cfg)
}

func newManagerAt(t *testing.T, dir string, fsys *vfs.FS, cfg config.Export) *Manager {
	t.Helper()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Owner() {
		t.Fatal("the test store should be owned by the test")
	}
	m, err := New(Options{Store: st, FS: fsys, Config: cfg})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// drive runs scheduling passes until the job stops moving. Tests never rely
// on the background goroutine's timing.
func drive(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if err := m.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		j, err := m.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State.Terminal() || j.State == StatePaused {
			return j
		}
	}
	t.Fatalf("job %s never settled", id)
	return Job{}
}

// ---------------------------------------------------------------------------
// A single-backend stack, for the cases that are about the destination or the
// content rather than about replica spread.

type solo struct {
	FS   *vfs.FS
	Fake *fakeprovider.Fake
	Up   *upload.Uploader
}

func newSolo(t *testing.T, wrap func(provider.Provider) provider.Provider) *solo {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	fake := fakeprovider.New("disk")
	var p provider.Provider = fake
	if wrap != nil {
		p = wrap(fake)
	}
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "disk", RootID: fake.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return p, true },
		Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:     fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	return &solo{FS: fsys, Fake: fake, Up: up}
}

// payload is deterministic content of a given size, distinct per seed.
func payload(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------

// TestExportSpreadsAcrossMembersAndReadsEachByteOnce: a pool is the reason an
// export is worth parallelising at all. Thirty files with three replicas each
// should come off the three members in roughly equal shares, and the total
// bytes read must be the total size of the files: a replica picked twice, or
// a range fetched again, would show up here as read amplification.
func TestExportSpreadsAcrossMembers(t *testing.T) {
	ctx := context.Background()
	h := poolharness.Open(t, poolharness.Options{
		Members:  []string{"a", "b", "c"},
		Settings: config.Pool{Replicas: 3, MinReplicas: 1},
	})
	// Give every member the same latency. The picker prefers the member
	// that has been measurably faster, and on fakes that answer instantly
	// "faster" is scheduling noise — which is exactly what a spread
	// assertion must not be measuring.
	for _, mem := range h.Members {
		mem.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 2 * time.Millisecond })
	}
	const files, size = 30, 3000
	total := int64(0)
	for i := 0; i < files; i++ {
		p := fmt.Sprintf("/data/f%02d.bin", i)
		content := payload(byte(i), size)
		for _, mem := range h.Members {
			mem.Seed(p, content)
		}
		total += int64(len(content))
	}
	// Warm the tree the way any listing would, so the planner asks nothing.
	if _, err := h.FS.ReadDirPath(ctx, "/data"); err != nil {
		t.Fatal(err)
	}
	before := make([]int, len(h.Members))
	for i, mem := range h.Members {
		before[i] = mem.Calls("ReadRange")
	}
	bytesBefore := h.ReadBytes()

	dest := t.TempDir()
	m := newManager(t, h.FS, testConfig())
	job, err := m.Create(ctx, Request{Sources: []string{"/data"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	done := drive(t, m, job.ID)
	if done.State != StateDone || done.FilesFailed != 0 {
		t.Fatalf("job ended %s/%s with %d failures: %s", done.State, done.PauseReason, done.FilesFailed, done.LastError)
	}
	if done.FilesDone != files {
		t.Fatalf("copied %d of %d files", done.FilesDone, files)
	}
	for i := 0; i < files; i++ {
		name := filepath.Join(dest, "data", fmt.Sprintf("f%02d.bin", i))
		if got := mustRead(t, name); !bytes.Equal(got, payload(byte(i), size)) {
			t.Fatalf("%s has the wrong content", name)
		}
	}
	if got := h.ReadBytes() - bytesBefore; got != total {
		t.Fatalf("the members served %d bytes for %d bytes of files", got, total)
	}
	want := files / len(h.Members)
	for i, mem := range h.Members {
		got := mem.Calls("ReadRange") - before[i]
		if got < want-2 || got > want+2 {
			t.Fatalf("member %s served %d reads, want %d ± 2", mem.Name(), got, want)
		}
	}
}

// TestExportOfCachedContentCostsNoReads: bytes this machine already has are
// not fetched again — neither for a file whose content is in the cache nor
// for one that has not been uploaded yet and exists only as a journal blob.
func TestExportOfCachedContentCostsNoReads(t *testing.T) {
	ctx := context.Background()
	s := newSolo(t, nil)
	content := payload(7, 9000)
	if _, err := s.FS.WriteFile(ctx, "/uploaded.bin", content, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	local := payload(9, 5000)
	if _, err := s.FS.WriteFile(ctx, "/queued.bin", local, false); err != nil {
		t.Fatal(err)
	}
	a, err := s.FS.StatPath(ctx, "/queued.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !a.LocalOnly {
		t.Fatal("the second file should still be waiting in the write queue")
	}

	before := s.Fake.Calls("ReadRange")
	dest := t.TempDir()
	m := newManager(t, s.FS, testConfig())
	job, err := m.Create(ctx, Request{Sources: []string{"/uploaded.bin", "/queued.bin"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	done := drive(t, m, job.ID)
	if done.State != StateDone || done.FilesFailed != 0 {
		t.Fatalf("job ended %s/%s with %d failures: %s", done.State, done.PauseReason, done.FilesFailed, done.LastError)
	}
	if got := s.Fake.Calls("ReadRange") - before; got != 0 {
		t.Fatalf("exporting cached content cost %d backend reads", got)
	}
	if got := mustRead(t, filepath.Join(dest, "uploaded.bin")); !bytes.Equal(got, content) {
		t.Fatal("the uploaded file exported wrong")
	}
	if got := mustRead(t, filepath.Join(dest, "queued.bin")); !bytes.Equal(got, local) {
		t.Fatal("the queued file exported wrong")
	}
	// The destination must be its own file, not a link into the cache: an
	// edit on the drive cannot be allowed to rewrite a cache object.
	for _, name := range []string{"uploaded.bin", "queued.bin"} {
		st, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink != 1 {
			t.Fatalf("%s has %d links; the export must not share an inode with the cache", name, sys.Nlink)
		}
	}
}

// TestExportResumesExactlyWhereItStopped: after a crash the plan is not
// rebuilt and only the ranges the bitmap does not account for are fetched
// again. The assertion is on bytes, because "resume" that quietly restarts
// the file still finishes.
func TestExportResumesExactlyWhereItStopped(t *testing.T) {
	ctx := context.Background()
	s := newSolo(t, nil)
	const size, chunk = 40 << 10, 4 << 10
	content := payload(3, size)
	s.Fake.Seed("/movie.bin", content)
	if _, err := s.FS.StatPath(ctx, "/movie.bin"); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	storeDir := t.TempDir()
	m := newManagerAt(t, storeDir, s.FS, testConfig())
	// Make every small test chunk a durable checkpoint. Production batches
	// these at syncEvery bytes, but the ordering under test is the same.
	m.syncEvery = chunk
	// Stop after four chunks, the way a kill does: no state is written for
	// the chunk in flight.
	const stopAfter = 4
	var written atomic.Int64
	crashCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.writeFault = func(int64) error {
		if written.Add(1) > stopAfter {
			cancel()
			return context.Canceled
		}
		return nil
	}
	job, err := m.Create(ctx, Request{Sources: []string{"/movie.bin"}, Dest: dest, RangeSize: chunk})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.RunOnce(crashCtx)
	m.Stop()
	if err := m.store.Close(); err != nil {
		t.Fatal(err)
	}

	it, readBefore := Item{}, s.Fake.ReadBytes()
	resumed := newManagerAt(t, storeDir, s.FS, testConfig())
	it, err = resumed.store.Item(ctx, job.ID, "movie.bin")
	if err != nil {
		t.Fatal(err)
	}
	if it.DoneBytes == 0 || it.DoneBytes >= size {
		t.Fatalf("the crash left %d of %d bytes recorded; the test needs a partial file", it.DoneBytes, size)
	}
	done := drive(t, resumed, job.ID)
	if done.State != StateDone || done.FilesFailed != 0 {
		t.Fatalf("job ended %s/%s: %s", done.State, done.PauseReason, done.LastError)
	}
	if got := mustRead(t, filepath.Join(dest, "movie.bin")); !bytes.Equal(got, content) {
		t.Fatal("the resumed file has the wrong content")
	}
	if got, want := s.Fake.ReadBytes()-readBefore, int64(size)-it.DoneBytes; got != want {
		t.Fatalf("the resume read %d bytes, want exactly the %d missing ones", got, want)
	}
}

// TestExportDoesNotCheckpointUnsyncedChunks protects the ordering between
// the part file and its bitmap. A crash may make us fetch bytes again, but it
// must never leave the database claiming bytes that were only in the OS page
// cache and can disappear in a power loss.
func TestExportDoesNotCheckpointUnsyncedChunks(t *testing.T) {
	ctx := context.Background()
	s := newSolo(t, nil)
	const size, chunk = 20 << 10, 4 << 10
	s.Fake.Seed("/movie.bin", payload(4, size))
	if _, err := s.FS.StatPath(ctx, "/movie.bin"); err != nil {
		t.Fatal(err)
	}

	m := newManager(t, s.FS, testConfig())
	m.syncEvery = 1 << 20 // no small test chunk reaches a periodic fsync
	crashCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writes atomic.Int64
	m.writeFault = func(int64) error {
		if writes.Add(1) > 1 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	job, err := m.Create(ctx, Request{Sources: []string{"/movie.bin"}, Dest: t.TempDir(), RangeSize: chunk})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.RunOnce(crashCtx)

	it, err := m.store.Item(ctx, job.ID, "movie.bin")
	if err != nil {
		t.Fatal(err)
	}
	if it.DoneBytes != 0 || it.Ranges != "" {
		t.Fatalf("unsynced data was checkpointed: done=%d ranges=%q", it.DoneBytes, it.Ranges)
	}
}

func TestCopyPathHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	if err := os.WriteFile(src, payload(1, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&Manager{}).copyPath(ctx, src, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy with a cancelled context returned %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("cancelled copy created a destination: %v", err)
	}
}

// TestExportPausesOnDiskFaults: a destination that is full, and one that is
// not the filesystem the job was planned against, both pause the job and
// leave its files on the queue with their ranges intact.
func TestExportPausesOnDiskFaults(t *testing.T) {
	ctx := context.Background()
	s := newSolo(t, nil)
	content := payload(11, 20000)
	s.Fake.Seed("/big.bin", content)
	s.Fake.Seed("/second.bin", payload(12, 5000))
	if _, err := s.FS.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	m := newManager(t, s.FS, testConfig())
	var full atomic.Bool
	full.Store(true)
	m.writeFault = func(int64) error {
		if full.Load() {
			return syscall.ENOSPC
		}
		return nil
	}
	job, err := m.Create(ctx, Request{Sources: []string{"/big.bin"}, Dest: dest, RangeSize: 4 << 10})
	if err != nil {
		t.Fatal(err)
	}
	paused := drive(t, m, job.ID)
	if paused.State != StatePaused || paused.PauseReason != PauseDisk {
		t.Fatalf("a full destination left the job %s/%s", paused.State, paused.PauseReason)
	}
	it, err := m.store.Item(ctx, job.ID, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if it.State != ItemPending {
		t.Fatalf("the file is %s, want pending so the resume picks it up", it.State)
	}
	full.Store(false)
	if err := m.Resume(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	done := drive(t, m, job.ID)
	if done.State != StateDone || done.FilesFailed != 0 {
		t.Fatalf("the resumed job ended %s/%s: %s", done.State, done.PauseReason, done.LastError)
	}
	if got := mustRead(t, filepath.Join(dest, "big.bin")); !bytes.Equal(got, content) {
		t.Fatal("the file exported wrong after the disk came back")
	}

	// A destination whose device is not the one the plan recorded is a
	// different filesystem mounted where the drive was.
	job2, err := m.Create(ctx, Request{Sources: []string{"/second.bin"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	real2, err := m.store.Job(ctx, job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.store.SetDestDev(ctx, job2.ID, real2.DestDev+1); err != nil {
		t.Fatal(err)
	}
	pulled := drive(t, m, job2.ID)
	if pulled.State != StatePaused || pulled.PauseReason != PauseDisk {
		t.Fatalf("a swapped destination device left the job %s/%s", pulled.State, pulled.PauseReason)
	}
	if err := m.store.SetDestDev(ctx, job2.ID, real2.DestDev); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(ctx, job2.ID); err != nil {
		t.Fatal(err)
	}
	if back := drive(t, m, job2.ID); back.State != StateDone {
		t.Fatalf("the job did not finish once the drive was back: %s/%s %s", back.State, back.PauseReason, back.LastError)
	}
}

// TestMirrorNeedsItsOwnMarker: a mirror deletes what the plan does not name,
// so it is refused anywhere a previous export of the same sources has not
// left its marker. With the marker, it removes the strays and nothing else.
func TestMirrorNeedsItsOwnMarker(t *testing.T) {
	ctx := context.Background()
	s := newSolo(t, nil)
	s.Fake.Seed("/docs/keep.txt", payload(1, 1200))
	s.Fake.Seed("/docs/sub/also.txt", payload(2, 800))
	if _, err := s.FS.ReadDirPath(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, s.FS, testConfig())

	fresh := t.TempDir()
	if _, err := m.Create(ctx, Request{Sources: []string{"/docs"}, Dest: fresh, Mirror: true}); err != ErrMirrorMarkerMissing {
		t.Fatalf("a mirror into an unmarked directory returned %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, markerName)); err == nil {
		t.Fatal("the refused mirror should not have claimed the directory")
	}

	dest := t.TempDir()
	first, err := m.Create(ctx, Request{Sources: []string{"/docs"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if got := drive(t, m, first.ID); got.State != StateDone {
		t.Fatalf("the first export ended %s: %s", got.State, got.LastError)
	}
	// Someone put something of their own in the destination, and an older
	// export left a file that is no longer in the source.
	stray := filepath.Join(dest, "docs", "stray.txt")
	if err := os.WriteFile(stray, []byte("not from the mount"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dest, "unrelated.txt")
	if err := os.WriteFile(outside, []byte("outside the exported root"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := m.Create(ctx, Request{Sources: []string{"/docs"}, Dest: dest, Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := drive(t, m, second.ID); got.State != StateDone {
		t.Fatalf("the mirror ended %s: %s", got.State, got.LastError)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("the mirror kept a file the source does not have: %v", err)
	}
	for _, keep := range []string{
		filepath.Join(dest, "docs", "keep.txt"),
		filepath.Join(dest, "docs", "sub", "also.txt"),
		outside,
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("the mirror removed %s: %v", keep, err)
		}
	}
}

// corruptor hands back content that does not match the hash the backend
// advertises, the way a truncating proxy or a bad cable does.
type corruptor struct {
	provider.Provider
	target atomic.Value // the backend id whose content comes back wrong
}

func (c *corruptor) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	rc, err := c.Provider.ReadRange(ctx, id, version, off, n)
	if err != nil || c.target.Load() != id {
		return rc, err
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	for i := range b {
		b[i] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// TestExportFailsFileAfterRepeatedHashMismatch: content that arrives wrong is
// retried, and then failed — with the job carrying on, because one bad file
// is not a reason to stop copying a drive.
func TestExportFailsFileAfterRepeatedHashMismatch(t *testing.T) {
	ctx := context.Background()
	var bad *corruptor
	s := newSolo(t, func(p provider.Provider) provider.Provider {
		bad = &corruptor{Provider: p}
		return bad
	})
	s.Fake.SetReportHashes(true)
	s.Fake.Seed("/broken.bin", payload(5, 6000))
	s.Fake.Seed("/fine.bin", payload(6, 4000))
	if _, err := s.FS.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	it, err := s.FS.StatPath(ctx, "/broken.bin")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.FS.Meta().Get(ctx, it.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if n.Hash == "" {
		t.Skip("the backend advertises no content hash; there is nothing to check against")
	}

	bad.target.Store(n.RemoteID)

	dest := t.TempDir()
	m := newManager(t, s.FS, testConfig())
	job, err := m.Create(ctx, Request{Sources: []string{"/broken.bin", "/fine.bin"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	done := drive(t, m, job.ID)
	if done.State != StateDone {
		t.Fatalf("the job ended %s/%s, want done with a failure recorded", done.State, done.PauseReason)
	}
	if done.FilesFailed != 1 || done.FilesDone != 1 {
		t.Fatalf("%d files failed and %d succeeded; the good file must still be copied", done.FilesFailed, done.FilesDone)
	}
	if got := mustRead(t, filepath.Join(dest, "fine.bin")); !bytes.Equal(got, payload(6, 4000)) {
		t.Fatal("the intact file did not survive the other one failing")
	}
	broken, err := m.store.Item(ctx, job.ID, "broken.bin")
	if err != nil {
		t.Fatal(err)
	}
	if broken.State != ItemFailed {
		t.Fatalf("the corrupted file is %s, want failed", broken.State)
	}
	if broken.Attempts != maxAttempts-1 {
		t.Fatalf("the corrupted file was tried %d extra times, want %d", broken.Attempts, maxAttempts-1)
	}
	if _, err := os.Stat(filepath.Join(dest, "broken.bin")); !os.IsNotExist(err) {
		t.Fatalf("content that failed its hash was published anyway: %v", err)
	}
}

// TestWarmPlanningCostsNoListing: the planner reads the tree the VFS already
// has. A second export of the same source asks the backend nothing at all
// while it plans — which is also what makes re-running an export cheap.
func TestWarmPlanningCostsNoListing(t *testing.T) {
	ctx := context.Background()
	h := poolharness.New(t, "a", "b")
	for i := 0; i < 12; i++ {
		p := fmt.Sprintf("/tree/d%d/f%d.bin", i%3, i)
		content := payload(byte(i), 700)
		for _, mem := range h.Members {
			mem.Seed(p, content)
		}
	}
	dest := t.TempDir()
	m := newManager(t, h.FS, testConfig())
	first, err := m.Create(ctx, Request{Sources: []string{"/tree"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if got := drive(t, m, first.ID); got.State != StateDone || got.FilesFailed != 0 {
		t.Fatalf("the first export ended %s: %s", got.State, got.LastError)
	}

	lists := h.Calls("List")
	second, err := m.Create(ctx, Request{Sources: []string{"/tree"}, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	// One pass is enough to plan; stop there and look at what it cost.
	if err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if extra := h.Calls("List") - lists; extra != 0 {
		t.Fatalf("planning a warm tree cost %d listings", extra)
	}
	done := drive(t, m, second.ID)
	if done.State != StateDone {
		t.Fatalf("the second export ended %s: %s", done.State, done.LastError)
	}
	if done.FilesSkipped != done.FilesTotal {
		t.Fatalf("the second export re-copied %d of %d files", done.FilesTotal-done.FilesSkipped, done.FilesTotal)
	}
	if done.FilesTotal != 12 {
		t.Fatalf("planned %d files, want 12", done.FilesTotal)
	}
}
