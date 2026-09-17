package agent

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/vfs"
)

func TestChangesOfMapsFeedEventsToRows(t *testing.T) {
	now := time.Unix(100, 0)
	rows := ChangesOf(vfs.Change{Paths: []string{"/a.txt"}, Kind: vfs.KindWrite, Origin: vfs.OriginAPI, OriginName: "mcp",
		Actor: vfs.Actor{SessionID: "s1", Principal: "p1"}}, now)
	if len(rows) != 1 || rows[0].Path != "/a.txt" || rows[0].Kind != "write" || rows[0].Origin != "mcp" || rows[0].SessionID != "s1" || rows[0].Principal != "p1" || !rows[0].Reliable {
		t.Fatalf("%+v", rows)
	}
	rows = ChangesOf(vfs.Change{Paths: []string{"/old", "/new"}, Kind: vfs.KindRename, Origin: vfs.OriginKernel}, now)
	if len(rows) != 1 || rows[0].Path != "/new" || rows[0].From != "/old" || rows[0].Origin != "kernel" {
		t.Fatalf("rename: %+v", rows)
	}
	rows = ChangesOf(vfs.Change{Rescan: true, Kind: vfs.KindRescan, Origin: vfs.OriginRemote}, now)
	if len(rows) != 1 || rows[0].Path != "/" || rows[0].Kind != "rescan" || rows[0].Reliable {
		t.Fatalf("rescan: %+v", rows)
	}
	rows = ChangesOf(vfs.Change{Paths: []string{"/d/x", "/d/y"}, Kind: vfs.KindRemote, Origin: vfs.OriginRemote}, now)
	if len(rows) != 2 || rows[1].Path != "/d/y" || rows[0].Origin != "remote" {
		t.Fatalf("listing: %+v", rows)
	}
	// An API change whose adapter set no name keeps the plain origin.
	rows = ChangesOf(vfs.Change{Paths: []string{"/a"}, Kind: vfs.KindWrite, Origin: vfs.OriginAPI}, now)
	if rows[0].Origin != "api" {
		t.Fatalf("unnamed api: %+v", rows)
	}
}

func TestChangesRecordQueryLastWriterAndHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	rows := []Change{
		{TS: base, Path: "/work/a.txt", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: base.Add(time.Second), Path: "/work/a.txt", Kind: "write", Origin: "mcp", SessionID: "s1", Principal: "p1", Reliable: true},
		{TS: base.Add(2 * time.Second), Path: "/", Kind: "rescan", Origin: "remote"},
		{TS: base.Add(3 * time.Second), Path: "/other/b.txt", Kind: "create", Origin: "webdav", Reliable: true},
		{TS: base.Add(4 * time.Second), Path: "/work/sub/c.txt", Kind: "remove", Origin: "control", Reliable: true},
		{TS: base.Add(5 * time.Second), Path: "/work/d.txt", From: "/work/a.txt", Kind: "rename", Origin: "kernel", Reliable: true},
	}
	last, err := s.RecordChanges(ctx, rows)
	if err != nil || last != 6 {
		t.Fatalf("record: last=%d err=%v", last, err)
	}
	if id, err := s.LastChangeID(ctx); err != nil || id != 6 {
		t.Fatalf("last id = %d %v", id, err)
	}

	got, more, err := s.Changes(ctx, ChangesQuery{})
	if err != nil || more || len(got) != 6 || got[0].ID != 1 || got[5].From != "/work/a.txt" {
		t.Fatalf("all: %d more=%v err=%v", len(got), more, err)
	}
	got, more, err = s.Changes(ctx, ChangesQuery{After: 2, Limit: 2})
	if err != nil || !more || len(got) != 2 || got[0].ID != 3 || got[1].ID != 4 {
		t.Fatalf("page: %+v more=%v err=%v", got, more, err)
	}
	// A prefix keeps rows under it and every rescan.
	got, _, err = s.Changes(ctx, ChangesQuery{Prefix: "/work"})
	if err != nil || len(got) != 5 {
		t.Fatalf("prefix: %+v %v", got, err)
	}
	for _, c := range got {
		if c.Path != "/" && c.Path != "/work/a.txt" && c.Path != "/work/sub/c.txt" && c.Path != "/work/d.txt" {
			t.Fatalf("prefix leaked %+v", c)
		}
	}
	got, _, err = s.Changes(ctx, ChangesQuery{Kinds: []string{"create"}})
	if err != nil || len(got) != 2 || got[0].Kind != "rescan" || got[1].Kind != "create" {
		t.Fatalf("kinds: %+v %v", got, err)
	}

	w, ok, err := s.LastWriter(ctx, "/work/a.txt")
	if err != nil || !ok || w.SessionID != "s1" || w.Origin != "mcp" || w.ID != 2 {
		t.Fatalf("last writer: %+v ok=%v err=%v", w, ok, err)
	}
	if _, ok, _ := s.LastWriter(ctx, "/nowhere"); ok {
		t.Fatal("unknown path has a last writer")
	}
	// The rescan row never answers a last-writer question.
	if _, ok, _ := s.LastWriter(ctx, "/"); ok {
		t.Fatal("the rescan row is a last writer")
	}

	h, err := s.History(ctx, "/work/a.txt", 10)
	if err != nil || len(h) != 3 || h[0].Kind != "rename" || h[1].ID != 2 || h[2].ID != 1 {
		t.Fatalf("history: %+v %v", h, err)
	}
	h, err = s.History(ctx, "/work", 10)
	if err != nil || len(h) != 4 {
		t.Fatalf("dir history: %+v %v", h, err)
	}
	h, err = s.History(ctx, "/", 2)
	if err != nil || len(h) != 2 || h[0].ID != 6 {
		t.Fatalf("root history: %+v %v", h, err)
	}

	// HistoryPage walks the same rows back in time by cursor: two pages of
	// two under /work (the second is the last), then nothing.
	page, more, err := s.HistoryPage(ctx, HistoryQuery{Path: "/work", Limit: 2})
	if err != nil || !more || len(page) != 2 || page[0].ID != 6 || page[1].ID != 5 {
		t.Fatalf("page 1: %+v more=%v %v", page, more, err)
	}
	page, more, err = s.HistoryPage(ctx, HistoryQuery{Path: "/work", Before: page[1].ID, Limit: 2})
	if err != nil || more || len(page) != 2 || page[0].ID != 2 || page[1].ID != 1 {
		t.Fatalf("page 2: %+v more=%v %v", page, more, err)
	}
	page, more, err = s.HistoryPage(ctx, HistoryQuery{Path: "/work", Before: page[1].ID, Limit: 2})
	if err != nil || more || len(page) != 0 {
		t.Fatalf("page 3: %+v more=%v %v", page, more, err)
	}
	if page, more, err = s.HistoryPage(ctx, HistoryQuery{Limit: 10}); err != nil || more || len(page) != 6 {
		t.Fatalf("root page: %d more=%v %v", len(page), more, err)
	}

	s.now = func() time.Time { return base.Add(40 * 24 * time.Hour) }
	n, err := s.PruneChanges(ctx, 0)
	if err != nil || n != 6 {
		t.Fatalf("prune: %d %v", n, err)
	}
}

// fakeFeed is a ChangeSource the recorder test drives by hand.
type fakeFeed struct {
	ch chan vfs.Change
}

func (f *fakeFeed) WatchChanges() (<-chan vfs.Change, func()) {
	return f.ch, func() {}
}

func TestChangeRecorderBatchesTheFeedIntoRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	feed := &fakeFeed{ch: make(chan vfs.Change, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.RunChangeRecorder(ctx, feed) }()
	feed.ch <- vfs.Change{Paths: []string{"/k.txt"}, Kind: vfs.KindWrite, Origin: vfs.OriginKernel}
	feed.ch <- vfs.Change{Paths: []string{"/m.txt"}, Kind: vfs.KindCreate, Origin: vfs.OriginAPI, OriginName: "mcp", Actor: vfs.Actor{SessionID: "s9"}}
	feed.ch <- vfs.Change{Rescan: true, Kind: vfs.KindRescan, Origin: vfs.OriginRemote}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _, err := s.Changes(context.Background(), ChangesQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 3 {
			if rows[0].Origin != "kernel" || rows[1].SessionID != "s9" || rows[1].Origin != "mcp" || rows[2].Kind != "rescan" || rows[2].Reliable {
				t.Fatalf("%+v", rows)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorder wrote %d rows", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Closing the feed ends the recorder after a final flush.
	feed.ch <- vfs.Change{Paths: []string{"/last"}, Kind: vfs.KindRemove, Origin: vfs.OriginKernel}
	close(feed.ch)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder did not stop when the feed closed")
	}
	cancel()
	rows, _, _ := s.Changes(context.Background(), ChangesQuery{})
	if len(rows) != 4 || rows[3].Path != "/last" {
		t.Fatalf("final flush: %+v", rows)
	}
}

func TestChangeRecorderRunsInTheOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Owner() {
		t.Fatal("second opener owns the store")
	}
	feed := &fakeFeed{ch: make(chan vfs.Change, 1)}
	done := make(chan struct{})
	go func() { defer close(done); other.RunChangeRecorder(context.Background(), feed) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a non-owner recorder kept running")
	}
}

// TestHookCursorsAreKeptPerSessionAndPrunedWithChanges: a hook cursor
// is unknown until set, reads back what was set, and goes with the
// change retention sweep once its session has been silent that long —
// a cursor from the older, time-less shape goes at the first sweep.
func TestHookCursorsAreKeptPerSessionAndPrunedWithChanges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if _, known, err := s.HookCursor(ctx, "claude", "old"); err != nil || known {
		t.Fatalf("fresh cursor: known=%v err=%v", known, err)
	}
	if err := s.SetHookCursor(ctx, "claude", "old", 7); err != nil {
		t.Fatal(err)
	}
	if id, known, err := s.HookCursor(ctx, "claude", "old"); err != nil || !known || id != 7 {
		t.Fatalf("set cursor: id=%d known=%v err=%v", id, known, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES ('hook_cursor:codex:legacy', '3')`); err != nil {
		t.Fatal(err)
	}
	if id, known, _ := s.HookCursor(ctx, "codex", "legacy"); !known || id != 3 {
		t.Fatalf("legacy value not read: %d %v", id, known)
	}
	now = now.Add(10 * 24 * time.Hour)
	if err := s.SetHookCursor(ctx, "claude", "recent", 9); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * 24 * time.Hour)
	n, err := s.PruneHookCursors(ctx, 30*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	if _, known, _ := s.HookCursor(ctx, "claude", "old"); known {
		t.Fatal("a cursor silent for 35 days survived")
	}
	if _, known, _ := s.HookCursor(ctx, "codex", "legacy"); known {
		t.Fatal("a time-less cursor survived")
	}
	if id, known, _ := s.HookCursor(ctx, "claude", "recent"); !known || id != 9 {
		t.Fatalf("a cursor set 25 days ago went: %d %v", id, known)
	}
}

// TestRescanMarksNextChangeUnreliable: an overflow of the feed becomes a
// rescan row with reliable false — the mark that rows before it may be
// missing — while the rows recorded after it are reliable again; and a
// recorder starting over a record that already has rows leaves the same
// mark for the time nothing recorded, so a crash with a batch unflushed
// is visible rather than silent. A first run leaves no mark.
func TestRescanMarksNextChangeUnreliable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	rows := ChangesOf(vfs.Change{Kind: vfs.KindWrite, Paths: []string{"/a"}, Origin: vfs.OriginKernel}, now)
	rows = append(rows, ChangesOf(vfs.Change{Kind: vfs.KindRescan, Rescan: true, Origin: vfs.OriginRemote}, now)...)
	rows = append(rows, ChangesOf(vfs.Change{Kind: vfs.KindWrite, Paths: []string{"/b"}, Origin: vfs.OriginKernel}, now)...)
	if _, err := s.RecordChanges(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Changes(ctx, ChangesQuery{})
	if err != nil || len(got) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if !got[0].Reliable || got[1].Kind != "rescan" || got[1].Reliable || !got[2].Reliable {
		t.Fatalf("reliability: %+v", got)
	}

	// A recorder over a record with rows marks its start; one over an
	// empty record does not.
	feed := &fakeFeed{ch: make(chan vfs.Change)}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.RunChangeRecorder(rctx, feed); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _, _ = s.Changes(ctx, ChangesQuery{})
		if len(got) == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(got) != 4 || got[3].Kind != "rescan" || got[3].Origin != "restart" || got[3].Reliable {
		t.Fatalf("no restart marker: %+v", got)
	}
	fresh, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	rctx, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() { fresh.RunChangeRecorder(rctx, feed); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if id, _ := fresh.LastChangeID(ctx); id != 0 {
		t.Fatalf("a first run left a marker: %d", id)
	}
}
