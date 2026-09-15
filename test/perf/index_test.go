package perf

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/index"
)

// Content indexing must not become a second downloader. Pinned mode reads
// what the cache already holds; rules mode downloads each file once per
// version. Both are call-count facts, so both are asserted here.

// newIndexer builds an index store and an Indexer over h with no
// background goroutines; the tests drive it with ReconcileNow.
func newIndexer(t *testing.T, h *harness, cfg config.Index) *index.Indexer {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := index.OpenStore(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x, err := index.New(index.Options{FS: h.fs, Store: st, Config: cfg, ReconcileEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	return x
}

func TestIndexPinnedFilesCostNoReads(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	const files = 500
	for i := 0; i < files; i++ {
		h.fake.Seed(fmt.Sprintf("pinned/note%03d.md", i), []byte(fmt.Sprintf("# note %d\n\nbody of note %d", i, i)))
	}
	if _, err := h.fs.ReadDirPath(ctx, "/pinned"); err != nil {
		t.Fatal(err)
	}
	// Pin fills the cache; every key must be complete before the index
	// looks, otherwise the test would measure the pin, not the indexer.
	if err := h.fs.Pin(ctx, "/pinned"); err != nil {
		t.Fatal(err)
	}
	kids, err := h.fs.Meta().Children(ctx, mustIno(t, h, "/pinned"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range kids {
		if !h.cache.Complete(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}) {
			t.Fatalf("%s is not complete in the cache after the pin", n.Name)
		}
	}
	x := newIndexer(t, h, config.Index{Enabled: true, Pinned: true})
	reads, lists := h.fake.Calls("ReadRange"), h.fake.Calls("List")
	rep, err := x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != files {
		t.Fatalf("extracted %d of %d pinned files: %s", rep.Extracted, files, rep)
	}
	if got := h.fake.Calls("ReadRange") - reads; got != 0 {
		t.Fatalf("indexing %d pinned files cost %d ReadRange calls, want 0", files, got)
	}
	if got := h.fake.Calls("List") - lists; got != 0 {
		t.Fatalf("indexing pinned files listed %d directories, want 0", got)
	}
	st, err := x.Store().Stats(ctx)
	if err != nil || st.DocsOK != files || st.DocsFailed != 0 || st.Pending != 0 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestIndexRulesFetchOncePerVersion(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	const files = 100
	for i := 0; i < files; i++ {
		h.fake.Seed(fmt.Sprintf("work/doc%03d.md", i), []byte(fmt.Sprintf("document %d", i)))
	}
	if _, err := h.fs.ReadDirPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	x := newIndexer(t, h, config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/work"}}})

	// Every file is at most one block, so the first pass costs one range
	// request per file and not one more.
	reads := h.fake.Calls("ReadRange")
	rep, err := x.ReconcileNow(ctx)
	if err != nil || rep.Extracted != files {
		t.Fatalf("%s %v", rep, err)
	}
	if got := h.fake.Calls("ReadRange") - reads; got != files {
		t.Fatalf("first pass over %d files cost %d ReadRange calls", files, got)
	}

	// Nothing changed: the second pass is free.
	reads = h.fake.Calls("ReadRange")
	rep, err = x.ReconcileNow(ctx)
	if err != nil || rep.Queued != 0 || rep.Extracted != 0 {
		t.Fatalf("%s %v", rep, err)
	}
	if got := h.fake.Calls("ReadRange") - reads; got != 0 {
		t.Fatalf("an unchanged tree cost %d ReadRange calls", got)
	}

	// One file at a new version: the third pass fetches that file only.
	h.fake.Seed("work/doc042.md", []byte("document 42, revised"))
	pastListingWindow()
	if err := h.fs.Refresh(ctx, mustIno(t, h, "/work")); err != nil {
		t.Fatal(err)
	}
	reads = h.fake.Calls("ReadRange")
	rep, err = x.ReconcileNow(ctx)
	if err != nil || rep.Queued != 1 || rep.Extracted != 1 {
		t.Fatalf("%s %v", rep, err)
	}
	if got := h.fake.Calls("ReadRange") - reads; got != 1 {
		t.Fatalf("one changed file cost %d ReadRange calls", got)
	}
	d, ok, err := x.Store().DocumentByPath(ctx, "/work/doc042.md")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if n, _ := h.fs.Meta().Resolve(ctx, "/work/doc042.md"); d.Version != n.Version {
		t.Fatalf("document version %s, tree version %s", d.Version, n.Version)
	}
}

// pastListingWindow waits for the wall clock to enter the next second: a
// listing keeps entries fetched within the second it started in, so a
// refresh in the same second as the first listing would keep the old
// version and the test would measure nothing.
func pastListingWindow() {
	next := time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)
	time.Sleep(time.Until(next))
}

func mustIno(t *testing.T, h *harness, p string) uint64 {
	t.Helper()
	a, err := h.fs.StatPath(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return a.Ino
}
