package chaos

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/index"
	"cloudfs/test/fakeprovider"
)

// The content index (docs/DESIGN.md §4.12, TODO.md T-37) joins the
// reliability matrix here: a process that dies while the extractor is busy
// must leave an index.db the next start can open and finish, and a
// document built to break the extractor must fail alone.

// indexConfig is a rules-mode configuration over /work with the defaults
// filled in, the way config.Load leaves it.
func indexConfig(t *testing.T) config.Index {
	t.Helper()
	cfg := config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/work"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// openIndexer opens index.db in dir and binds an Indexer with test-sized
// delays to the rig's FS. Nothing is started.
func openIndexer(t *testing.T, r *rig, dir string) (*index.Store, *index.Indexer) {
	t.Helper()
	st, err := index.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	x, err := index.New(index.Options{
		FS: r.fs, Store: st, Config: indexConfig(t),
		StartDelay: time.Millisecond, ReconcileEvery: time.Hour,
		YieldMax: 100 * time.Millisecond, RiskSleep: time.Second,
	})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, x
}

// copyImage copies the SQLite files of an index directory as they are on
// disk right now: the database, its WAL and the shared-memory file. This
// is what a restart after kill -9 finds, torn frames included, so the copy
// is deliberately taken without stopping the writer.
func copyImage(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		src, err := os.ReadFile(filepath.Join(from, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, name), src, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIndexSurvivesAnUncleanStopMidExtraction covers "索引中 kill -9". Like
// TestKillDuringWriteLosesNothing, the crash is modelled by never closing
// anything: the database files are copied while the worker is in the
// middle of its queue, and the copy is opened the way a fresh process
// would open the survivor. The copy passes integrity_check, keeps what
// was committed, and a new indexer over it finishes the queue.
func TestIndexSurvivesAnUncleanStopMidExtraction(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	const files = 50
	for i := 0; i < files; i++ {
		r.fake.Seed(fmt.Sprintf("work/%02d.md", i), []byte(fmt.Sprintf("# note %02d\n%s", i, strings.Repeat("content ", 200))))
	}
	if _, err := r.fs.ReadDirPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	// Every fetch takes a while, so the worker is still busy when the
	// image is taken.
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 20 * time.Millisecond })

	dirA := filepath.Join(r.dir, "index")
	stA, xA := openIndexer(t, r, dirA)
	runCtx, cancelRun := context.WithCancel(ctx)
	xA.Start(runCtx)
	// The abandoned process still has to release its goroutines when the
	// test ends; a real kill needs none of this.
	t.Cleanup(func() { cancelRun(); xA.Close(); stA.Close() })

	var before index.Stats
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := stA.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.DocsOK >= 10 {
			before = st
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker indexed only %d documents in 20s", st.DocsOK)
		}
		time.Sleep(10 * time.Millisecond)
	}
	dirB := filepath.Join(r.dir, "index-after-crash")
	copyImage(t, dirA, dirB)

	// The crash: the old process is gone. Its goroutines are stopped so
	// they do not keep fetching against the same fake, but nothing is
	// checkpointed or closed on the image already taken.
	cancelRun()
	xA.Close()
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 0 })

	stB, xB := openIndexer(t, r, dirB)
	t.Cleanup(func() { xB.Close(); stB.Close() })
	if got, err := stB.IntegrityCheck(ctx); err != nil || got != "ok" {
		t.Fatalf("integrity_check on the crash image = %q, %v", got, err)
	}
	image, err := stB.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if image.DocsOK < before.DocsOK {
		t.Fatalf("the image lost committed documents: %d before the copy, %d in the image", before.DocsOK, image.DocsOK)
	}
	if image.DocsOK >= files {
		t.Fatalf("the image was taken after the queue drained (%d ok), so it proves nothing about pending work", image.DocsOK)
	}
	if image.DocsOK+image.Pending < files {
		t.Fatalf("the image does not account for every queued file: ok %d pending %d", image.DocsOK, image.Pending)
	}

	// The next start picks the queue up where the image left it.
	if _, err := xB.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := stB.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.DocsOK != files || after.Pending != 0 || after.DocsFailed != 0 {
		t.Fatalf("after the restart: %+v", after)
	}
	res, err := xB.Search(ctx, index.SearchQuery{Query: "note 49", Roots: []string{"/work"}, TopK: 5})
	if err != nil || len(res.Hits) == 0 || res.Hits[0].Path != "/work/49.md" {
		t.Fatalf("the last file is not searchable after the restart: %+v %v", res, err)
	}
}

// bombDocx is a .docx whose archive holds more entries than the extractor
// allows (textract.DefaultOptions().MaxZipEntries is 4096): small on disk,
// refused by the zip guard before anything is inflated.
func bombDocx(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>hi</w:t></w:r></w:p></w:body></w:document>`)
	for i := 0; i < 4097; i++ {
		if _, err := zw.Create(fmt.Sprintf("junk/%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestMaliciousArchiveFailsTheDocumentNotTheDaemon covers "恶意 zip" at the
// daemon's level: the archive arrives through the running indexer's own
// change feed and worker, is recorded as a failed document naming the
// archive limit, and the worker goes on to index the files written after
// it. A panic anywhere on that path would end the test binary.
func TestMaliciousArchiveFailsTheDocumentNotTheDaemon(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	r.fake.Seed("work/good.md", []byte("a sound document"))
	if _, err := r.fs.ReadDirPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	st, x := openIndexer(t, r, filepath.Join(r.dir, "index"))
	t.Cleanup(func() { x.Close(); st.Close() })
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	x.Start(runCtx)

	// The bomb is written the way an agent or a shell would write it.
	if _, err := r.fs.WriteFile(ctx, "/work/bomb.docx", bombDocx(t), false); err != nil {
		t.Fatal(err)
	}
	waitForIndex(t, st, "/work/bomb.docx", index.DocFailed)
	failed, _, err := st.Failed(ctx, "", 10)
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed documents = %+v, %v", failed, err)
	}
	if failed[0].Path != "/work/bomb.docx" || failed[0].Kind != "docx" || !strings.Contains(failed[0].Error, "archive") {
		t.Fatalf("the bomb is not recorded as an archive failure: %+v", failed[0])
	}
	waitForIndex(t, st, "/work/good.md", index.DocOK)

	// The worker survived: a file written after the bomb is indexed too.
	if _, err := r.fs.WriteFile(ctx, "/work/after.md", []byte("written after the bomb"), false); err != nil {
		t.Fatal(err)
	}
	waitForIndex(t, st, "/work/after.md", index.DocOK)
	stats, err := st.Stats(ctx)
	if err != nil || stats.DocsOK != 2 || stats.DocsFailed != 1 || stats.Pending != 0 {
		t.Fatalf("stats after the bomb = %+v, %v", stats, err)
	}
}

// waitForIndex polls until the document at p is in the given state.
func waitForIndex(t *testing.T, st *index.Store, p string, state index.DocState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d, ok, _ := st.DocumentByPath(context.Background(), p); ok && d.State == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	d, ok, _ := st.DocumentByPath(context.Background(), p)
	t.Fatalf("%s did not reach state %d within 10s (found %v: %+v)", p, state, ok, d)
}
