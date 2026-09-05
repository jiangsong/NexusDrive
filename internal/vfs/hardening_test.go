package vfs

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/test/fakeprovider"
)

func TestCompletedWriteReleasesStagingObjectProtection(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/discard.txt", []byte("committed contents"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending: %+v %v", rows, err)
	}
	n, err := e.store.Resolve(ctx, "/ali/discard.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Get(ctx, rows[0].ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("row remains: %v", err)
	}
	if _, err := os.Stat(rows[0].BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed staging reservation leaked: %v", err)
	}
}

// TestRandomReadsDoNotArmReadAhead: on a big file a random 4 KiB read lands
// in the block after the previous one often enough that block adjacency alone
// kept opening the read-ahead window, and every false start pulled whole
// blocks the reader never used (500 random reads fetched 116 MB of a 256 MiB
// file). Only a run of contiguous reads may arm it.
func TestRandomReadsDoNotArmReadAhead(t *testing.T) {
	// The shape of the real case: a file of 64 blocks, so one read in
	// thirty lands next to the previous one, and enough sub-blocks per
	// block that locality promotion cannot fire by chance.
	const block, sub, blocks = 64 << 10, 1 << 10, 64
	e := newEnv(t, envOpt{blockSize: block, subBlockSize: sub, readAheadBlocks: 8})
	ctx := context.Background()
	size := block * blocks
	e.fake.Seed("big.bin", bytes.Repeat([]byte("r"), size))
	n, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadDir(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)

	const reads = 500
	rng := rand.New(rand.NewSource(7))
	buf := make([]byte, 64)
	for i := 0; i < reads; i++ {
		if _, err := e.fs.Read(ctx, h, buf, rng.Int63n(int64(size-len(buf)))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // any read-ahead would run by now
	got := e.fake.ReadBytes()
	// One sub-block per read, plus a quarter for reads that straddle two;
	// a single whole block (64 KiB) already breaks the budget.
	if limit := int64(reads*sub + reads*sub/4); got > limit {
		t.Fatalf("%d random reads of %d bytes fetched %d bytes from the provider, want at most %d (no whole blocks)", reads, len(buf), got, limit)
	}
	t.Logf("%d random reads fetched %d bytes (%d per read)", reads, got, got/reads)
}

// TestMediaSeekCancelsOldReadAhead is the playback shape behind STRM/WebDAV:
// after sequential reads arm a window, jumping to 80% must cancel requests
// around the abandoned position before they consume bytes or cache space.
func TestMediaSeekCancelsOldReadAhead(t *testing.T) {
	const block, sub, blocks = 4096, 512, 100
	e := newEnv(t, envOpt{blockSize: block, subBlockSize: sub, readAheadBlocks: 8})
	ctx := context.Background()
	size := block * blocks
	e.fake.Seed("movie.mkv", bytes.Repeat([]byte("m"), size))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)

	buf := make([]byte, block)
	for i := int64(0); i < 2; i++ {
		if _, err := e.fs.Read(ctx, h, buf, i*block); err != nil {
			t.Fatal(err)
		}
	}
	// Delay the third demand read and the read-ahead it arms. Once that read
	// returns, wait until both background requests entered the provider.
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 200 * time.Millisecond })
	if _, err := e.fs.Read(ctx, h, buf, 2*block); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for e.fake.Calls("ReadRange") < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls := e.fake.Calls("ReadRange"); calls < 5 {
		t.Fatalf("read-ahead did not enter provider: %d calls", calls)
	}

	// Block 4 is already owned by the old prefetch. Treating the jump from
	// block 2 as a seek cancels that flight; the foreground read must retry
	// it rather than leaking context.Canceled to the player.
	if _, err := e.fs.Read(ctx, h, buf, 4*block); err != nil {
		t.Fatalf("foreground read did not recover from cancelled prefetch: %v", err)
	}
	seek := int64(size * 80 / 100)
	small := make([]byte, sub)
	if _, err := e.fs.Read(ctx, h, small, seek); err != nil {
		t.Fatal(err)
	}
	// If the old detached work survived the seek, its two whole blocks add
	// 8192 bytes. Give those requests time to expose that failure.
	time.Sleep(250 * time.Millisecond)
	want := int64(4*block + sub)
	if got := e.fake.ReadBytes(); got != want {
		t.Fatalf("seek transferred %d bytes, want %d; stale read-ahead survived", got, want)
	}
	if got := e.cache.Stats().Bytes; got > want {
		t.Fatalf("seek cached %d bytes, want at most %d", got, want)
	}
}

func TestReleaseDuringReadDoesNotRestartReadAhead(t *testing.T) {
	const block = 4096
	e := newEnv(t, envOpt{blockSize: block, subBlockSize: 512, readAheadBlocks: 8})
	ctx := context.Background()
	e.fake.Seed("movie.mkv", bytes.Repeat([]byte("m"), block*20))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, block)
	for i := int64(0); i < 2; i++ {
		if _, err := e.fs.Read(ctx, h, buf, i*block); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 200 * time.Millisecond })
	readDone := make(chan error, 1)
	go func() {
		_, err := e.fs.Read(ctx, h, make([]byte, block), 2*block)
		readDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for e.fake.Calls("ReadRange") < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := e.fake.Calls("ReadRange"); got != 3 {
		t.Fatalf("closed handle started read-ahead: %d provider reads", got)
	}
}

// TestFlushResumesACommitThatFailedHalfway: once the staging file has been
// renamed under its hash there is no going back. A failure after that (the
// metadata store was busy) used to leave the handle pointing at a staging
// file that no longer existed, so the kernel's next FLUSH failed too and the
// data was stranded with no journal row reachable from the file.
func TestFlushResumesACommitThatFailedHalfway(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, dir.Ino, "half.txt")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("survives a busy store")
	if _, err := e.fs.Write(ctx, h, payload, 0); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("meta: database is locked")
	e.fs.commitFault = func() error { return fault }
	if err := e.fs.Sync(ctx, h); !errors.Is(err, fault) {
		t.Fatalf("first flush: got %v, want the injected fault", err)
	}
	e.fs.commitFault = nil
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatalf("second flush did not resume the commit: %v", err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/half.txt")
	if err != nil {
		t.Fatal(err)
	}
	if node.Size != int64(len(payload)) {
		t.Fatalf("size after resumed commit = %d, want %d", node.Size, len(payload))
	}
	rows, err := e.j.ByIno(ctx, node.Ino)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, r := range rows {
		if r.State == journal.StatePending || r.State == journal.StateUploading {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("resumed commit left %d live journal rows, want exactly 1: %+v", live, rows)
	}
	rh, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, rh)
	got := make([]byte, len(payload))
	if _, err := e.fs.Read(ctx, rh, got, 0); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read back %q (%v), want %q", got, err, payload)
	}
}

// TestListingInvalidatesOnlyWhatChanged: the kernel caches a directory's
// stream while our first listing of it is in flight, so invalidating after
// every fetch discarded that cache and a warm walk re-read every directory.
// A first listing must not invalidate; a refresh that changes nothing must
// not either; a refresh that removes a name must drop the directory and that
// name.
func TestListingInvalidatesOnlyWhatChanged(t *testing.T) {
	e := newEnv(t, envOpt{dirTTL: time.Millisecond})
	ctx := context.Background()
	e.fake.Seed("proj/a.txt", []byte("a"))
	b := e.fake.Seed("proj/b.txt", []byte("b"))
	var mu sync.Mutex
	var inos []uint64
	var entries []string
	e.fs.SetInvalidate(func(ino uint64) { mu.Lock(); inos = append(inos, ino); mu.Unlock() })
	e.fs.SetInvalidateEntry(func(parent uint64, name string) { mu.Lock(); entries = append(entries, name); mu.Unlock() })
	reset := func() { mu.Lock(); inos, entries = nil, nil; mu.Unlock() }

	dir, err := e.fs.ReadDirPath(ctx, "/ali/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) != 2 {
		t.Fatalf("listing: %v", dir)
	}
	mu.Lock()
	first := len(inos) + len(entries)
	mu.Unlock()
	if first != 0 {
		t.Fatalf("the first listing of a directory sent %d invalidations; the kernel is caching it from this very call", first)
	}

	reset()
	e.clk.advance(time.Second) // past the TTL: the next read re-lists
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	unchanged := len(inos) + len(entries)
	mu.Unlock()
	if unchanged != 0 {
		t.Fatalf("a refresh that changed nothing sent %d invalidations", unchanged)
	}

	reset()
	if err := e.fake.Delete(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(time.Second)
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	node, _ := e.store.Resolve(ctx, "/ali/proj")
	if len(inos) != 1 || inos[0] != node.Ino {
		t.Fatalf("a refresh that removed an entry invalidated inodes %v, want just the directory %d", inos, node.Ino)
	}
	if len(entries) != 1 || entries[0] != "b.txt" {
		t.Fatalf("a refresh that removed b.txt invalidated names %v", entries)
	}
}

// TestShortReadIsNeverCached: a backend that answers a range with fewer
// bytes than asked (a server capping request sizes, a proxy cutting a
// stream) must not have its answer cached as a complete block, or the file
// would read as silently truncated until the cache was dropped.
func TestShortReadIsNeverCached(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 4096, subBlockSize: 1024})
	ctx := context.Background()
	data := bytes.Repeat([]byte("q"), 3*4096)
	e.fake.Seed("cut.bin", data)
	n, _ := e.store.Resolve(ctx, "/ali")
	if _, err := e.fs.ReadDir(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	node, _ := e.store.Resolve(ctx, "/ali/cut.bin")
	e.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.ShortRead = 512 })
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	// Whole-block path (sequential from offset 0) and sub-block path
	// (random offset) must both refuse the short answer.
	if _, err := e.fs.Read(ctx, h, buf, 0); err == nil {
		t.Fatal("a short whole-block read was accepted")
	}
	if _, err := e.fs.Read(ctx, h, buf[:64], 2*4096+100); err == nil {
		t.Fatal("a short sub-block read was accepted")
	}
	e.fs.Release(ctx, h)
	key := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	if have, _ := e.cache.Present(key); have != 0 {
		t.Fatalf("%d blocks were cached from short reads", have)
	}
	e.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.ShortRead = 0 })
	h, _ = e.fs.Open(ctx, node.Ino, false)
	defer e.fs.Release(ctx, h)
	if _, err := e.fs.Read(ctx, h, buf, 0); err != nil || !bytes.Equal(buf, data[:4096]) {
		t.Fatalf("read after the fault cleared: %v", err)
	}
}

// TestLostUploadIsRepairedInTheTree: when recovery dead-letters an upload,
// the node still pointed at the local-only key whose data is gone, so the
// file read as torn bytes or EIO for good. RepairLost drops the node and
// stales the listing: a file the remote had comes back as the remote
// version, a file it never had disappears (its remains are in the dead
// letter).
func TestLostUploadIsRepairedInTheTree(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("keep.txt", []byte("remote copy"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/keep.txt", []byte("local overwrite"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/brand-new.txt", []byte("never uploaded"), false); err != nil {
		t.Fatal(err)
	}
	// The blobs are lost before the uploader gets to them.
	var ids []string
	for _, name := range []string{"keep.txt", "brand-new.txt"} {
		n, err := e.store.Resolve(ctx, "/ali/"+name)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := e.j.ByIno(ctx, n.Ino)
		if err != nil || len(rows) == 0 {
			t.Fatalf("%s has no pending upload: %v", name, err)
		}
		if err := os.Truncate(rows[0].BlobPath, 3); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rows[0].ID)
	}
	rec, err := e.j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 2 {
		t.Fatalf("recovery lost %v, want both", rec.Lost)
	}
	e.fs.RepairLost(ctx, e.j, rec.Lost)

	entries, err := e.fs.ReadDirPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]Attr{}
	for _, a := range entries {
		names[a.Name] = a
	}
	if _, ok := names["brand-new.txt"]; ok {
		t.Fatal("a file the remote never had is still listed after its data was lost")
	}
	keep, ok := names["keep.txt"]
	if !ok || keep.LocalOnly {
		t.Fatalf("keep.txt should be back as the remote version: %+v", keep)
	}
	h, err := e.fs.Open(ctx, keep.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	buf := make([]byte, 64)
	n, err := e.fs.Read(ctx, h, buf, 0)
	if err != nil || string(buf[:n]) != "remote copy" {
		t.Fatalf("keep.txt reads %q (%v), want the remote copy", buf[:n], err)
	}
	for _, id := range ids {
		if u, _ := e.j.Get(ctx, id); u.State != journal.StateDead {
			t.Fatalf("%s is %s, want dead (its remains kept for inspection)", id, u.State)
		}
	}
}

// TestWriteAfterUnlinkDoesNotResurrectTheFile: a file unlinked while a
// descriptor is still open is gone when that descriptor closes — POSIX
// discards the data. Committing it by name used to insert a fresh node and
// queue an upload, so the deleted file came back with the last bytes
// written to it.
func TestWriteAfterUnlinkDoesNotResurrectTheFile(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, dir.Ino, "ghost.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("boo"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, dir.Ino, "ghost.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatalf("flush after unlink: %v", err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Resolve(ctx, "/ali/ghost.txt"); err == nil {
		t.Fatal("the unlinked file is back in the tree after its last close")
	}
	rows, err := e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rows {
		if u.Name == "ghost.txt" && !u.Tombstone {
			t.Fatalf("an upload of the unlinked file is queued without a tombstone: %+v", u)
		}
	}
}

// TestCommitAfterRenameKeepsTheNewName: a rename between open and flush
// must not make the flush recreate the old name.
func TestCommitAfterRenameKeepsTheNewName(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, dir.Ino, "draft.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("final"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, dir.Ino, "draft.txt", dir.Ino, "final.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("final!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	e.fs.Release(ctx, h)
	if _, err := e.store.Resolve(ctx, "/ali/draft.txt"); err == nil {
		t.Fatal("the flush after the rename recreated the old name")
	}
	n, err := e.store.Resolve(ctx, "/ali/final.txt")
	if err != nil {
		t.Fatal(err)
	}
	rh, err := e.fs.Open(ctx, n.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, rh)
	buf := make([]byte, 16)
	k, err := e.fs.Read(ctx, rh, buf, 0)
	if err != nil || string(buf[:k]) != "final!" {
		t.Fatalf("final.txt reads %q (%v)", buf[:k], err)
	}
}

// TestReadHandleSurvivesUploadCompletion: a handle opened while the file was
// local-only keeps that identity; when the upload lands the local-only cache
// entry is released, and the next read through the old handle used to fail
// with "missing from the cache". It must pick up the node's new identity.
func TestReadHandleSurvivesUploadCompletion(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/late.txt", []byte("read me later"), false); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/late.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocalOnly(n.RemoteID) {
		t.Fatalf("expected a local-only node before the upload: %+v", n)
	}
	h, err := e.fs.Open(ctx, n.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	buf := make([]byte, 32)
	if k, err := e.fs.Read(ctx, h, buf, 0); err != nil || string(buf[:k]) != "read me later" {
		t.Fatalf("before upload: %q %v", buf[:k], err)
	}
	e.clk.advance(10 * time.Second) // past the write-settle window
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, _ := e.store.Resolve(ctx, "/ali/late.txt")
	if IsLocalOnly(fresh.RemoteID) {
		t.Fatalf("the upload did not land: %+v", fresh)
	}
	if k, err := e.fs.Read(ctx, h, buf, 0); err != nil || string(buf[:k]) != "read me later" {
		t.Fatalf("after upload, through the old handle: %q %v", buf[:k], err)
	}
}

// TestTruncateThroughAnotherHandleIsNotUndone: the kernel turns O_TRUNC into
// a truncate through a handle of its own, so a write handle that is already
// open must not be holding a copy of the content that truncate removed —
// otherwise the write lands in a full-sized staging file and commits the old
// tail back, all the way to the backend.
func TestTruncateThroughAnotherHandleIsNotUndone(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("a"), 64<<10)
	ch, err := e.fs.Create(ctx, dir.Ino, "rewrite.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, ch, big, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, ch); err != nil {
		t.Fatal(err)
	}
	at, err := e.store.Resolve(ctx, "/ali/rewrite.bin")
	if err != nil {
		t.Fatal(err)
	}

	// Open for writing first, then truncate through a second handle, then
	// write through the first — the order the kernel uses.
	h, err := e.fs.Open(ctx, at.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	th, err := e.fs.Open(ctx, at.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Truncate(ctx, th, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, th); err != nil {
		t.Fatal(err)
	}
	small := bytes.Repeat([]byte("b"), 4<<10)
	if _, err := e.fs.Write(ctx, h, small, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Resolve(ctx, "/ali/rewrite.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != int64(len(small)) {
		t.Fatalf("the file is %d bytes after the rewrite, want %d", got.Size, len(small))
	}
}

// TestRewritingQueuesOneUploadNotTwo: the kernel truncates through a handle of
// its own, so a rewrite commits twice — an empty file, then the content. Only
// the content is worth sending, and the handle that wrote it cannot drop the
// other one, because it never saw it.
func TestRewritingQueuesOneUploadNotTwo(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := e.fs.Create(ctx, dir.Ino, "doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, ch, bytes.Repeat([]byte("a"), 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, ch); err != nil {
		t.Fatal(err)
	}
	at, err := e.store.Resolve(ctx, "/ali/doc.bin")
	if err != nil {
		t.Fatal(err)
	}

	th, err := e.fs.Open(ctx, at.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Truncate(ctx, th, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, th); err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, at.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, bytes.Repeat([]byte("b"), 2048), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}

	rows, err := e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, u := range rows {
		if u.Name == "doc.bin" {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d uploads queued for one rewrite, want the content one only", queued)
	}
}
