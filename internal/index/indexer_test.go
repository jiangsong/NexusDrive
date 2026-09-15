package index

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

func rulesAll(paths ...string) config.Index {
	x := config.Index{Enabled: true}
	for _, p := range paths {
		x.Rules = append(x.Rules, config.IndexRule{Path: p})
	}
	return x
}

func TestReconcileIndexesMatchingFilesOnly(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("# 标题\n正文"))
	h.fake.Seed("work/.env", []byte("SECRET=1"))
	h.fake.Seed("other/b.md", []byte("outside"))
	h.list(t, "/work")
	h.list(t, "/other")
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 1 || rep.Queued != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/work/.env"); ok {
		t.Fatal(".env was indexed")
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/other/b.md"); ok {
		t.Fatal("a file outside every rule was indexed")
	}
	d, ok, _ := h.store.DocumentByPath(ctx, "/work/a.md")
	if !ok || d.State != DocOK || d.Kind != "markdown" {
		t.Fatalf("%+v %v", d, ok)
	}
	// A second pass finds nothing to do.
	rep, err = h.x.ReconcileNow(ctx)
	if err != nil || rep.Queued != 0 || rep.Extracted != 0 || rep.Deleted != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestChangeEventIndexesAFreshFile(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fake.Seed("work/seed.md", []byte("seed"))
	h.list(t, "/work")
	h.x.Start(ctx)
	if _, err := h.fs.WriteFile(ctx, "/work/new.md", []byte("fresh marker"), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, ok, _ := h.store.DocumentByPath(ctx, "/work/new.md")
		return ok && d.State == DocOK
	}, "a written file was not indexed within 3s")
	// Publishing the upload moves the file to its remote identity; the
	// index follows it and holds one document for the path, not two.
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		d, ok, _ := h.store.DocumentByPath(ctx, "/work/new.md")
		return ok && d.State == DocOK && !vfs.IsLocalOnly(d.RemoteID)
	}, "the published file was not re-keyed to its remote id within 3s")
	if n, _ := h.store.DocumentsUnder(ctx, "/work"); n != 2 {
		t.Fatalf("documents under /work = %d, want 2", n)
	}
}

func TestRenameKeepsIndexedAtAndUpdatesThePath(t *testing.T) {
	h := newHarness(t, rulesAll("/"))
	ctx := context.Background()
	h.fake.Seed("dir/a.md", []byte("body"))
	h.list(t, "/dir")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	before, ok, _ := h.store.DocumentByPath(ctx, "/dir/a.md")
	if !ok {
		t.Fatal("not indexed")
	}
	reads := h.fake.Calls("ReadRange")
	if err := h.fs.Rename(ctx, meta.RootIno, "dir", meta.RootIno, "moved"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, ok, _ := h.store.DocumentByPath(ctx, "/moved/a.md")
	if !ok || !after.IndexedAt.Equal(before.IndexedAt) || after.ID != before.ID {
		t.Fatalf("%+v %+v", before, after)
	}
	if rep.Renamed != 1 || rep.Extracted != 0 {
		t.Fatalf("%+v", rep)
	}
	if got := h.fake.Calls("ReadRange"); got != reads {
		t.Fatalf("a directory rename re-fetched content: %d -> %d ReadRange calls", reads, got)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/dir/a.md"); ok {
		t.Fatal("the old path still has a document")
	}
}

func TestRenameEventMovesDocumentsWithoutRefetching(t *testing.T) {
	h := newHarness(t, rulesAll("/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fake.Seed("dir/a.md", []byte("body a"))
	h.fake.Seed("dir/sub/b.md", []byte("body b"))
	h.list(t, "/dir")
	h.list(t, "/dir/sub")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	reads := h.fake.Calls("ReadRange")
	h.x.Start(ctx)
	if err := h.fs.Rename(ctx, meta.RootIno, "dir", meta.RootIno, "moved"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, okA, _ := h.store.DocumentByPath(ctx, "/moved/a.md")
		_, okB, _ := h.store.DocumentByPath(ctx, "/moved/sub/b.md")
		return okA && okB
	}, "documents did not follow the rename within 3s")
	waitFor(t, 3*time.Second, func() bool { return h.x.Progress().Pending == 0 }, "queue did not drain")
	if got := h.fake.Calls("ReadRange"); got != reads {
		t.Fatalf("a rename event re-fetched content: %d -> %d", reads, got)
	}
}

// docxWith builds a minimal .docx whose text is body; extra entries change
// the bytes (and so the version) without changing the extracted text.
func docxWith(t *testing.T, body string, extra ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`+body+`</w:t></w:r></w:p></w:body></w:document>`)
	for _, name := range extra {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, "<x/>")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSameTextNewVersionDoesNotRechunk(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.docx", docxWith(t, "same text"))
	h.list(t, "/work")
	if rep, err := h.x.ReconcileNow(ctx); err != nil || rep.Extracted != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	d, _, _ := h.store.DocumentByPath(ctx, "/work/a.docx")
	chunkIDs := func() []int64 {
		rows, err := h.store.db.QueryContext(ctx, `SELECT id FROM chunks WHERE doc_id = ? ORDER BY seq`, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	before := chunkIDs()
	if len(before) == 0 {
		t.Fatal("no chunks")
	}
	// New bytes, new version, same words: the document's metadata changed.
	h.fake.Seed("work/a.docx", docxWith(t, "same text", "docProps/custom.xml"))
	h.advance(2 * time.Second) // past the listing's protection of just-fetched entries
	dir, _ := h.fs.StatPath(ctx, "/work")
	if err := h.fs.Refresh(ctx, dir.Ino); err != nil {
		t.Fatal(err)
	}
	n, _ := h.fs.Meta().Resolve(ctx, "/work/a.docx")
	if n.Version == d.Version {
		t.Fatal("the version did not change")
	}
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil || rep.Queued != 1 || rep.Extracted != 0 || rep.Skipped != 1 || rep.Failed != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	after, _, _ := h.store.DocumentByPath(ctx, "/work/a.docx")
	if after.Version != n.Version || after.State != DocOK {
		t.Fatalf("version not moved: %+v", after)
	}
	if got := chunkIDs(); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("chunks were rebuilt: %v -> %v", before, got)
	}
}

func TestBusyForegroundMakesTheWorkerYield(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 20; i++ {
		h.fake.Seed(fmt.Sprintf("work/f%02d.md", i), []byte("x"))
	}
	h.fake.Seed("hold/big.bin", bytes.Repeat([]byte("z"), 8192))
	h.list(t, "/work")
	h.list(t, "/hold")
	stop := holdForeground(t, h, "/hold/big.bin")
	defer stop()
	h.x.Start(ctx)
	waitFor(t, 5*time.Second, func() bool { return h.x.Yields() > 0 },
		"the extraction worker never yielded to foreground IO within 5s")
	if p := h.x.Progress(); p.Yields == 0 {
		t.Fatalf("progress does not show the yield: %+v", p)
	}
}

func TestRiskControlPausesTheWorker(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ {
		h.fake.Seed(fmt.Sprintf("work/f%d.md", i), []byte("text"))
	}
	h.list(t, "/work")
	total := h.fake.TotalCalls()
	h.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.RiskControlAfter = total })
	start := time.Now()
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 0 || rep.Failed != 0 {
		t.Fatalf("risk control was treated as a per-file failure: %+v", rep)
	}
	p := h.x.Progress()
	if p.Paused != PausedRiskControl {
		t.Fatalf("paused = %q, want risk_control (%+v)", p.Paused, p)
	}
	if p.ResumeAt.Before(start.Add(time.Second)) || p.ResumeAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("resume_at = %v, want ≈ now + RiskSleep", p.ResumeAt)
	}
	if p.Pending == 0 {
		t.Fatal("the refused file left the queue")
	}
	// Neither the worker nor another synchronous pass fetches during the pause.
	h.x.Start(ctx)
	reads := h.fake.Calls("ReadRange")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.fake.Calls("ReadRange"); got != reads {
		t.Fatalf("ReadRange calls kept coming during the pause: %d -> %d", reads, got)
	}
	// After the sleep the worker tries again on its own.
	h.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.RiskControlAfter = 0 })
	waitFor(t, 5*time.Second, func() bool {
		st, _ := h.store.Stats(ctx)
		return st.DocsOK == 5
	}, "the worker did not resume after RiskSleep")
}

func TestFetchBudgetPausesRules(t *testing.T) {
	cfg := rulesAll("/work")
	cfg.FetchBudget = "10B/h"
	h := newHarness(t, cfg)
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("12345678"))
	h.fake.Seed("work/b.md", []byte("abcdefgh"))
	h.list(t, "/work")
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 1 {
		t.Fatalf("%+v", rep)
	}
	p := h.x.Progress()
	if p.Paused != PausedBudget || p.ResumeAt.IsZero() || p.Pending != 1 {
		t.Fatalf("%+v", p)
	}
	if used, limit := h.x.budget.Used(); used != 8 || limit != 10 {
		t.Fatalf("budget used %d of %d", used, limit)
	}
}

func TestUnofficialRemoteIsChargedDouble(t *testing.T) {
	cfg := rulesAll("/work")
	cfg.FetchBudget = "20B/h"
	h := newHarness(t, cfg)
	ctx := context.Background()
	h.x.unofficial = func(string) bool { return true }
	h.fake.Seed("work/a.md", []byte("12345678"))
	h.fake.Seed("work/b.md", []byte("abcdefgh"))
	h.list(t, "/work")
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 1 || h.x.Progress().Paused != PausedBudget {
		t.Fatalf("%+v %+v", rep, h.x.Progress())
	}
}

func TestPinnedModeReadsOnlyCompleteCachedFiles(t *testing.T) {
	h := newHarness(t, config.Index{Enabled: true, Pinned: true})
	ctx := context.Background()
	h.fake.Seed("pinned/hot.md", []byte("hydrated text"))
	h.fake.Seed("pinned/cold.md", []byte("never downloaded"))
	h.fake.Seed("loose/other.md", []byte("not pinned"))
	h.list(t, "/pinned")
	h.list(t, "/loose")
	if err := h.fs.Pin(ctx, "/pinned"); err != nil {
		t.Fatal(err)
	}
	// Forget the cold file's blocks: pinned, but not complete in the cache.
	cold, _ := h.fs.Meta().Resolve(ctx, "/pinned/cold.md")
	h.fs.Cache().Forget(fileKey(cold))
	reads := h.fake.Calls("ReadRange")
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 1 || rep.Queued != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/pinned/hot.md"); !ok {
		t.Fatal("the hydrated pinned file was not indexed")
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/pinned/cold.md"); ok {
		t.Fatal("a cold pinned file was indexed")
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/loose/other.md"); ok {
		t.Fatal("an unpinned file was indexed in pinned mode")
	}
	if got := h.fake.Calls("ReadRange"); got != reads {
		t.Fatalf("pinned mode fetched from the remote: %d -> %d", reads, got)
	}
}

// bombDocx is a .docx whose archive has more entries than the extractor
// allows: small on disk, refused by the zip guard.
func bombDocx(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>hi</w:t></w:r></w:p></w:body></w:document>`)
	for i := 0; i < 4100; i++ {
		if _, err := zw.Create(fmt.Sprintf("j/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMaliciousArchiveFailsTheDocumentNotTheDaemon(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/bomb.docx", bombDocx(t))
	h.fake.Seed("work/fine.md", []byte("still indexed"))
	h.fake.Seed("work/photo.txt", append([]byte{0x89, 'P', 'N', 'G', 0, 0, 0}, bytes.Repeat([]byte{0}, 100)...))
	h.list(t, "/work")
	rep, err := h.x.ReconcileNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 2 || rep.Extracted != 1 {
		t.Fatalf("%+v", rep)
	}
	failed, _, err := h.store.Failed(ctx, "", 10)
	if err != nil || len(failed) != 2 {
		t.Fatalf("%+v %v", failed, err)
	}
	for _, f := range failed {
		switch f.Path {
		case "/work/bomb.docx":
			if f.Kind != "docx" || f.Error == "" {
				t.Fatalf("%+v", f)
			}
		case "/work/photo.txt":
			if f.Kind != "" || f.Error == "" {
				t.Fatalf("%+v", f)
			}
		default:
			t.Fatalf("unexpected failure %+v", f)
		}
	}
	st, _ := h.store.Stats(ctx)
	if st.DocsOK != 1 || st.DocsFailed != 2 || st.Pending != 0 {
		t.Fatalf("%+v", st)
	}
	// Retrying queues them again; the same failure is recorded, not a
	// panic and not a stall.
	if n, err := h.store.RetryFailed(ctx, "/work"); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if rep, err := h.x.ReconcileNow(ctx); err != nil || rep.Failed != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestDeletedFileLeavesTheIndex(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fake.Seed("work/a.md", []byte("a"))
	h.fake.Seed("work/b.md", []byte("b"))
	h.list(t, "/work")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	dir, _ := h.fs.StatPath(ctx, "/work")
	h.x.Start(ctx)
	if err := h.fs.Remove(ctx, dir.Ino, "a.md", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, ok, _ := h.store.DocumentByPath(ctx, "/work/a.md")
		return !ok
	}, "a removed file stayed in the index")
	if _, ok, _ := h.store.DocumentByPath(ctx, "/work/b.md"); !ok {
		t.Fatal("the sibling was removed too")
	}
}

func TestWatchReportsProgress(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("a"))
	h.list(t, "/work")
	ch, stop := h.x.Watch()
	defer stop()
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	var last Progress
	for done := false; !done; {
		select {
		case p := <-ch:
			last = p
		default:
			done = true
		}
	}
	if last.Extracted != 1 || last.LastReconcile.IsZero() {
		t.Fatalf("%+v", last)
	}
}
