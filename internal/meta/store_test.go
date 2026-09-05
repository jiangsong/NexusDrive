package meta

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openTest(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, c
}

func dir(parent uint64, name string) Node {
	return Node{ParentIno: parent, Name: name, Kind: provider.KindDir, TTL: time.Minute}
}

func file(parent uint64, name string, size int64) Node {
	return Node{ParentIno: parent, Name: name, Kind: provider.KindFile, Size: size, TTL: time.Minute}
}

func TestRootExists(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	r, err := s.Get(ctx, RootIno)
	if err != nil || !r.IsDir() {
		t.Fatalf("root = %+v, %v", r, err)
	}
	p, err := s.Path(ctx, RootIno)
	if err != nil || p != "/" {
		t.Fatalf("root path = %q, %v", p, err)
	}
}

func TestUpsertLookupPath(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "work"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Upsert(ctx, file(d.Ino, "notes.md", 42))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, d.Ino, "notes.md")
	if err != nil || got.Ino != f.Ino || got.Size != 42 {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	p, err := s.Path(ctx, f.Ino)
	if err != nil || p != "/work/notes.md" {
		t.Fatalf("path = %q, %v", p, err)
	}
	// Upsert again with a new size: same inode, updated attrs.
	f2 := file(d.Ino, "notes.md", 99)
	f2, err = s.Upsert(ctx, f2)
	if err != nil || f2.Ino != f.Ino {
		t.Fatalf("re-upsert changed inode: %d -> %d (%v)", f.Ino, f2.Ino, err)
	}
	got, _ = s.Get(ctx, f.Ino)
	if got.Size != 99 {
		t.Fatalf("size not updated: %d", got.Size)
	}
	if _, err := s.Lookup(ctx, d.Ino, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lookup = %v", err)
	}
}

func TestResolve(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, _ := s.Upsert(ctx, dir(RootIno, "a"))
	b, _ := s.Upsert(ctx, dir(a.Ino, "b"))
	c, _ := s.Upsert(ctx, file(b.Ino, "c.txt", 1))
	for _, p := range []string{"/a/b/c.txt", "a/b/c.txt", "/a/b/../b/c.txt"} {
		n, err := s.Resolve(ctx, p)
		if err != nil || n.Ino != c.Ino {
			t.Fatalf("Resolve(%q) = %+v, %v", p, n, err)
		}
	}
	if n, err := s.Resolve(ctx, "/"); err != nil || n.Ino != RootIno {
		t.Fatalf("Resolve(/) = %+v, %v", n, err)
	}
	if _, err := s.Resolve(ctx, "/a/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve missing = %v", err)
	}
}

func TestPutDirReplacesAndPreservesInodes(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "d"))
	if err := s.PutDir(ctx, d.Ino, []Node{file(0, "keep.txt", 1), file(0, "gone.txt", 2)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	keep, _ := s.Lookup(ctx, d.Ino, "keep.txt")
	gone, _ := s.Lookup(ctx, d.Ino, "gone.txt")
	if keep.Ino == 0 || gone.Ino == 0 {
		t.Fatal("children not created")
	}
	st, _ := s.DirState(ctx, d.Ino)
	if !st.Complete || !st.Fresh(c.now(), time.Minute) {
		t.Fatalf("dir state = %+v", st)
	}
	// Second listing drops gone.txt and adds new.txt; keep.txt keeps its inode.
	if err := s.PutDir(ctx, d.Ino, []Node{file(0, "keep.txt", 7), file(0, "new.txt", 3)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	keep2, err := s.Lookup(ctx, d.Ino, "keep.txt")
	if err != nil || keep2.Ino != keep.Ino || keep2.Size != 7 {
		t.Fatalf("keep = %+v, %v (want ino %d size 7)", keep2, err, keep.Ino)
	}
	if _, err := s.Lookup(ctx, d.Ino, "gone.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gone.txt should be deleted, got %v", err)
	}
	kids, _ := s.Children(ctx, d.Ino)
	if len(kids) != 2 || kids[0].Name != "keep.txt" || kids[1].Name != "new.txt" {
		t.Fatalf("children = %+v", kids)
	}
	// Freshness expires with the clock.
	c.advance(2 * time.Minute)
	st, _ = s.DirState(ctx, d.Ino)
	if st.Fresh(c.now(), time.Minute) {
		t.Fatal("listing should be stale after the TTL")
	}
}

func TestRemoveSubtreeAndNegativeCache(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	a, _ := s.Upsert(ctx, dir(RootIno, "a"))
	b, _ := s.Upsert(ctx, dir(a.Ino, "b"))
	f, _ := s.Upsert(ctx, file(b.Ino, "deep.txt", 1))
	if err := s.Remove(ctx, a.Ino); err != nil {
		t.Fatal(err)
	}
	for _, ino := range []uint64{a.Ino, b.Ino, f.Ino} {
		if _, err := s.Get(ctx, ino); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ino %d should be gone, got %v", ino, err)
		}
	}
	absent, err := s.IsAbsent(ctx, RootIno, "a")
	if err != nil || !absent {
		t.Fatalf("removed name should be negatively cached: %v %v", absent, err)
	}
	c.advance(defaultNegativeTTL + time.Second)
	if absent, _ := s.IsAbsent(ctx, RootIno, "a"); absent {
		t.Fatal("negative cache should expire")
	}
	// Creating the name again clears the entry immediately.
	if err := s.MarkAbsent(ctx, RootIno, "z", time.Hour); err != nil {
		t.Fatal(err)
	}
	if absent, _ := s.IsAbsent(ctx, RootIno, "z"); !absent {
		t.Fatal("MarkAbsent did not take")
	}
	if _, err := s.Upsert(ctx, file(RootIno, "z", 1)); err != nil {
		t.Fatal(err)
	}
	if absent, _ := s.IsAbsent(ctx, RootIno, "z"); absent {
		t.Fatal("creating a name must clear its negative cache entry")
	}
}

func TestRename(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, _ := s.Upsert(ctx, dir(RootIno, "a"))
	b, _ := s.Upsert(ctx, dir(RootIno, "b"))
	f, _ := s.Upsert(ctx, file(a.Ino, "f.txt", 1))
	victim, _ := s.Upsert(ctx, file(b.Ino, "f.txt", 2))

	if err := s.Rename(ctx, f.Ino, b.Ino, "f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, victim.Ino); !errors.Is(err, ErrNotFound) {
		t.Fatalf("overwritten target should be gone, got %v", err)
	}
	p, _ := s.Path(ctx, f.Ino)
	if p != "/b/f.txt" {
		t.Fatalf("path after rename = %q", p)
	}
	if absent, _ := s.IsAbsent(ctx, a.Ino, "f.txt"); !absent {
		t.Fatal("old name should be negatively cached")
	}
	if absent, _ := s.IsAbsent(ctx, b.Ino, "f.txt"); absent {
		t.Fatal("new name must not be negatively cached")
	}
}

func TestCursorsAndPins(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if c, err := s.Cursor(ctx, "gdrive"); err != nil || c != "" {
		t.Fatalf("unset cursor = %q, %v", c, err)
	}
	if err := s.SetCursor(ctx, "gdrive", "tok1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, "gdrive", "tok2"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(ctx, "gdrive"); c != "tok2" {
		t.Fatalf("cursor = %q", c)
	}

	if err := s.AddPin(ctx, Pin{Path: "/work", Recursive: true}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"/work":              true,
		"/work/sub/deep.txt": true,
		"/workspace":         false,
		"/other":             false,
	} {
		got, err := s.IsPinned(ctx, path)
		if err != nil || got != want {
			t.Errorf("IsPinned(%q) = %v, %v; want %v", path, got, err, want)
		}
	}
	if err := s.RemovePin(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Pins(ctx); len(pins) != 0 {
		t.Fatalf("pins = %+v", pins)
	}
}

func TestSearch(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "project"))
	sub, _ := s.Upsert(ctx, dir(d.Ino, "internal"))
	s.Upsert(ctx, file(d.Ino, "README.md", 1))
	s.Upsert(ctx, file(sub.Ino, "readme_test.go", 1))
	s.Upsert(ctx, file(sub.Ino, "store.go", 1))

	res, err := s.Search(ctx, "readme", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("search readme = %+v", res)
	}
	// Shallower path first.
	if res[0].Path != "/project/README.md" {
		t.Fatalf("expected shallowest match first, got %+v", res)
	}
	res, _ = s.Search(ctx, ".go", 10)
	if len(res) != 2 {
		t.Fatalf("search .go = %+v", res)
	}
	// Short queries use one/two-rune postings.
	res, _ = s.Search(ctx, "md", 10)
	if len(res) != 1 || res[0].Name != "README.md" {
		t.Fatalf("short search = %+v", res)
	}
	// Deleting a node drops it from the index.
	f, _ := s.Lookup(ctx, sub.Ino, "store.go")
	s.Remove(ctx, f.Ino)
	res, _ = s.Search(ctx, "store", 10)
	if len(res) != 0 {
		t.Fatalf("deleted node still indexed: %+v", res)
	}
}

func TestReopenPersists(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(dbPath, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "keep"))
	s.Upsert(ctx, file(d.Ino, "f.txt", 5))
	s.SetCursor(ctx, "r", "cur")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dbPath, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.Resolve(ctx, "/keep/f.txt")
	if err != nil || n.Size != 5 {
		t.Fatalf("after reopen = %+v, %v", n, err)
	}
	if cur, _ := s2.Cursor(ctx, "r"); cur != "cur" {
		t.Fatalf("cursor lost: %q", cur)
	}
	st, _ := s2.Stats(ctx)
	if st.Nodes != 2 || st.Dirs != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestInvalidateAndVacuum(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "d"))
	s.PutDir(ctx, d.Ino, []Node{file(0, "a", 1)}, time.Minute, nil)
	if st, _ := s.DirState(ctx, d.Ino); !st.Fresh(c.now(), time.Minute) {
		t.Fatal("should be fresh")
	}
	if err := s.Invalidate(ctx, d.Ino); err != nil {
		t.Fatal(err)
	}
	st, _ := s.DirState(ctx, d.Ino)
	if st.Fresh(c.now(), time.Minute) || !st.Dirty {
		t.Fatalf("after invalidate = %+v", st)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "d"))
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			for j := 0; j < 10; j++ {
				_, err := s.Upsert(ctx, file(d.Ino, string(rune('a'+i))+string(rune('0'+j)), int64(j)))
				if err != nil {
					errCh <- err
					return
				}
			}
			errCh <- nil
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	kids, _ := s.Children(ctx, d.Ino)
	if len(kids) != 80 {
		t.Fatalf("expected 80 children, got %d", len(kids))
	}
}

// TestPutDirKeepsProtectedChildren covers the case a directory refresh used to
// get wrong: a file whose upload is still queued is not in the backend's
// listing, and deleting it there loses a write the caller was told had
// succeeded.
func TestPutDirKeepsProtectedChildren(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "d"))
	if err != nil {
		t.Fatal(err)
	}

	pending := file(0, "pending.txt", 10)
	pending.RemoteID = "cloudfs-local:42"
	pending.ParentIno = d.Ino
	if _, err := s.Upsert(ctx, pending); err != nil {
		t.Fatal(err)
	}
	uploaded := file(0, "uploaded.txt", 20)
	uploaded.RemoteID = "remote-1"
	uploaded.ParentIno = d.Ino
	if _, err := s.Upsert(ctx, uploaded); err != nil {
		t.Fatal(err)
	}

	protect := func(n Node) bool { return strings.HasPrefix(n.RemoteID, "cloudfs-local:") }
	// The backend lists neither file: one was never uploaded, the other was
	// deleted remotely.
	if err := s.PutDir(ctx, d.Ino, nil, time.Minute, protect); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, d.Ino, "pending.txt"); err != nil {
		t.Fatalf("a queued write must survive a directory refresh: %v", err)
	}
	if _, err := s.Lookup(ctx, d.Ino, "uploaded.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a file gone from the backend should be dropped, got %v", err)
	}

	// A listing that does mention the name must not roll the local state back
	// to what the backend still believes.
	stale := file(0, "pending.txt", 3)
	stale.RemoteID = "remote-2"
	stale.Version = "stale"
	if err := s.PutDir(ctx, d.Ino, []Node{stale}, time.Minute, protect); err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, d.Ino, "pending.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "cloudfs-local:42" || got.Size != 10 {
		t.Fatalf("the local state was overwritten by the listing: %+v", got)
	}

	// Without a predicate the old behaviour stands: the listing is the truth.
	if err := s.PutDir(ctx, d.Ino, nil, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, d.Ino, "pending.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with no predicate the listing should win, got %v", err)
	}
}

func TestInvalidateAllStalesEveryListing(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	a, _ := s.Upsert(ctx, dir(RootIno, "a"))
	b, _ := s.Upsert(ctx, dir(RootIno, "b"))
	for _, d := range []Node{a, b} {
		if err := s.PutDir(ctx, d.Ino, []Node{file(0, "x", 1)}, time.Minute, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkAbsent(ctx, a.Ino, "ghost", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []Node{a, b} {
		st, err := s.DirState(ctx, d.Ino)
		if err != nil {
			t.Fatal(err)
		}
		if st.Fresh(c.now(), time.Minute) {
			t.Fatalf("dir %d still fresh after InvalidateAll", d.Ino)
		}
		// The nodes themselves survive: only the "complete" mark is gone.
		if _, err := s.Lookup(ctx, d.Ino, "x"); err != nil {
			t.Fatalf("InvalidateAll must not delete nodes: %v", err)
		}
	}
	if absent, _ := s.IsAbsent(ctx, a.Ino, "ghost"); absent {
		t.Fatal("negative entries must be forgotten too")
	}
}

// TestPutDirBatchMatchesRowByRow: the batched path must produce the same
// tree as inserting one node at a time, including inode reuse across
// re-listings and removal of names that vanished.
func TestPutDirBatchMatchesRowByRow(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "big"))
	const n = 5000
	first := make([]Node, 0, n)
	for i := 0; i < n; i++ {
		f := file(0, fmt.Sprintf("f%05d", i), int64(i))
		f.RemoteID = fmt.Sprintf("id-%d", i)
		f.Version = "v1"
		first = append(first, f)
	}
	if err := s.PutDir(ctx, d.Ino, first, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	kids, err := s.Children(ctx, d.Ino)
	if err != nil || len(kids) != n {
		t.Fatalf("children = %d, %v", len(kids), err)
	}
	inos := map[string]uint64{}
	for _, k := range kids {
		inos[k.Name] = k.Ino
		if k.Mode != 0o644 || k.TTL != time.Minute || k.ParentIno != d.Ino {
			t.Fatalf("row %s = %+v", k.Name, k)
		}
	}
	// Re-list: half the names go away, the rest change size, some are new.
	second := make([]Node, 0, n)
	for i := 0; i < n; i += 2 {
		f := file(0, fmt.Sprintf("f%05d", i), int64(i)*2)
		f.RemoteID = fmt.Sprintf("id-%d", i)
		f.Version = "v2"
		second = append(second, f)
	}
	for i := 0; i < 100; i++ {
		second = append(second, file(0, fmt.Sprintf("new%03d", i), 1))
	}
	if err := s.PutDir(ctx, d.Ino, second, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	kids, _ = s.Children(ctx, d.Ino)
	if len(kids) != n/2+100 {
		t.Fatalf("after relisting %d children, want %d", len(kids), n/2+100)
	}
	for _, k := range kids {
		if strings.HasPrefix(k.Name, "f") {
			if k.Ino != inos[k.Name] {
				t.Fatalf("%s changed inode %d -> %d", k.Name, inos[k.Name], k.Ino)
			}
			if k.Version != "v2" {
				t.Fatalf("%s not updated: %+v", k.Name, k)
			}
		}
	}
	if _, err := s.Lookup(ctx, d.Ino, "f00001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a vanished name survived: %v", err)
	}
}

// TestSearchSeesNamesBeforeAndAfterIndexing: deferring FTS work must not
// make a freshly listed name unfindable.
func TestSearchSeesNamesBeforeAndAfterIndexing(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, _ := s.Upsert(ctx, dir(RootIno, "docs"))
	if err := s.PutDir(ctx, d.Ino, []Node{file(0, "quarterly-report.pdf", 1), file(0, "notes.md", 1)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	found := func(q string) bool {
		res, err := s.Search(ctx, q, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Name == "quarterly-report.pdf" {
				return true
			}
		}
		return false
	}
	if !found("quarterly") {
		t.Fatal("a name still in the pending table was not found")
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	s.db.QueryRow(`SELECT COUNT(*) FROM name_index_pending`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d rows still pending after a flush", pending)
	}
	if !found("quarterly") {
		t.Fatal("a name that moved into the FTS index was not found")
	}
	// A vanished name leaves both tables.
	if err := s.PutDir(ctx, d.Ino, []Node{file(0, "notes.md", 1)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if found("quarterly") {
		t.Fatal("a removed name is still searchable")
	}
}
