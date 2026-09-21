package index

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/embed"
)

// embedRules is rulesAll with an embedding block the config accepts; the
// harness hands in a fake embedder in place of the client the daemon
// would build from it.
func embedRules(paths ...string) config.Index {
	cfg := rulesAll(paths...)
	cfg.Embedding = config.IndexEmbedding{Provider: "ollama", Model: "fake", Batch: 4}
	return cfg
}

func withEmbedder(e embed.Embedder) func(*Options) {
	return func(o *Options) { o.Embedder = e }
}

// seedNotes seeds n one-chunk markdown files under dir and lists it.
func seedNotes(t *testing.T, h *harness, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		h.fake.Seed(fmt.Sprintf("%s/note%02d.md", dir, i), []byte(fmt.Sprintf("note %d says something distinct", i)))
	}
	h.list(t, "/"+dir)
}

// seedLater adds one more file to a listed directory and refreshes the
// listing past its protection window so meta sees it.
func seedLater(t *testing.T, h *harness, p string, body string) {
	t.Helper()
	h.fake.Seed(strings.TrimPrefix(p, "/"), []byte(body))
	h.advance(2 * time.Second)
	dir, err := h.fs.Meta().Resolve(context.Background(), path.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.fs.Refresh(context.Background(), dir.Ino); err != nil {
		t.Fatal(err)
	}
}

func TestChangingTheModelReembedsEverything(t *testing.T) {
	h := newHarnessOpt(t, embedRules("/work"), withEmbedder(embed.NewFake(8)))
	ctx := context.Background()
	seedNotes(t, h, "work", 6)
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := h.x.EmbedNow(ctx)
	if err != nil || rep.Embedded != 6 || rep.Calls != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	st, err := h.x.Status(ctx, "")
	if err != nil || st.Vectors != 6 || st.Embedding.Embedded != 6 || st.Embedding.Pending != 0 || st.Embedding.Model != "fake" || st.Embedding.Dim != 8 {
		t.Fatalf("%+v %v", st, err)
	}
	if st.Embedding.CharsThisMonth == 0 || !st.Embedding.Healthy || st.Embedding.Provider != "ollama" {
		t.Fatalf("%+v", st.Embedding)
	}
	if model, _ := h.store.Meta(ctx, metaEmbeddingModel); model != "fake" {
		t.Fatalf("embedding_model %q", model)
	}
	fts := ftsCount(t, h.store.db, "distinct")

	// The configuration now names another model: on its first turn the
	// worker throws the vectors away, queues every chunk again and leaves
	// the keyword index alone.
	other := newStub("other-model", 8)
	x := h.reopenIndexer(t, withEmbedder(other))
	// Until the worker has run, a search must not score the old model's
	// vectors against a query embedded by the new one.
	if r, err := x.Search(ctx, SearchQuery{Query: "distinct", Mode: "vector"}); err != nil || r.ModeUsed != "keyword" || !strings.Contains(r.Degraded, "other-model") {
		t.Fatalf("search across models: %+v %v", r, err)
	}
	if other.Calls() != 0 {
		t.Fatal("the query was embedded although the vectors could not be used")
	}
	if err := x.embedPrepare(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, h.store.db, "vectors") != 0 {
		t.Fatal("vectors of the old model survived")
	}
	if count(t, h.store.db, "embed_pending") != count(t, h.store.db, "chunks") || count(t, h.store.db, "chunks") != 6 {
		t.Fatalf("embed_pending %d chunks %d", count(t, h.store.db, "embed_pending"), count(t, h.store.db, "chunks"))
	}
	if n := ftsCount(t, h.store.db, "distinct"); n != fts {
		t.Fatalf("chunks_fts rows %d, were %d", n, fts)
	}
	if model, _ := h.store.Meta(ctx, metaEmbeddingModel); model != "other-model" {
		t.Fatalf("embedding_model %q", model)
	}
	if dim, _ := h.store.Meta(ctx, metaEmbeddingDim); dim != "" {
		t.Fatalf("embedding_dim %q should be cleared with the vectors", dim)
	}
	if r, err := x.Search(ctx, SearchQuery{Query: "distinct", Mode: "vector"}); err != nil || r.ModeUsed != "keyword" || r.Degraded == "" {
		t.Fatalf("search while nothing is embedded: %+v %v", r, err)
	}
	// Draining re-embeds everything with the new model, and a second
	// prepare is a no-op.
	rep, err = x.EmbedNow(ctx)
	if err != nil || rep.Embedded != 6 {
		t.Fatalf("%+v %v", rep, err)
	}
	if other.Calls() != 2 {
		t.Fatalf("calls %d for 6 chunks in batches of 4", other.Calls())
	}
	if dim, _ := h.store.Meta(ctx, metaEmbeddingDim); dim != "8" {
		t.Fatalf("embedding_dim %q after the probe", dim)
	}
	if err := x.embedPrepare(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, h.store.db, "vectors") != 6 || count(t, h.store.db, "embed_pending") != 0 {
		t.Fatal("a repeated prepare with the same model touched the vectors")
	}
	if r, err := x.Search(ctx, SearchQuery{Query: "distinct", Mode: "vector"}); err != nil || r.ModeUsed != "vector" || r.Degraded != "" || len(r.Hits) == 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestChangingEmbeddingInputRebuildsDerivedVectors(t *testing.T) {
	h := newHarnessOpt(t, embedRules("/work"), withEmbedder(embed.NewFake(8)))
	ctx := context.Background()
	seedNotes(t, h, "work", 2)
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.x.EmbedNow(ctx); err != nil || rep.Embedded != 2 {
		t.Fatalf("initial embed: %+v %v", rep, err)
	}
	if err := h.store.SetMeta(ctx, metaEmbeddingInputVersion, "1"); err != nil {
		t.Fatal(err)
	}
	x := h.reopenIndexer(t, withEmbedder(embed.NewFake(8)))
	if err := x.embedPrepare(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, h.store.db, "vectors") != 0 || count(t, h.store.db, "embed_pending") != 2 {
		t.Fatalf("vectors=%d pending=%d", count(t, h.store.db, "vectors"), count(t, h.store.db, "embed_pending"))
	}
	if got, _ := h.store.Meta(ctx, metaEmbeddingInputVersion); got != embedInputVersion {
		t.Fatalf("embedding input version = %q", got)
	}
}

func TestEmbedWorkerSleepsWhileTheBreakerIsOpen(t *testing.T) {
	fake := embed.NewFake(8)
	h := newHarnessOpt(t, embedRules("/work"), withEmbedder(fake))
	ctx := context.Background()
	seedNotes(t, h, "work", 5)
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(400 * time.Millisecond)
	fake.SetBreakerOpenUntil(until)
	rep, err := h.x.EmbedNow(ctx)
	if err != nil || rep.Embedded != 0 || rep.Paused != PausedBreaker || !rep.ResumeAt.Equal(until) {
		t.Fatalf("%+v %v", rep, err)
	}
	if fake.Calls() != 0 {
		t.Fatalf("%d calls were made into an open breaker", fake.Calls())
	}
	st, err := h.x.Status(ctx, "")
	if err != nil || st.Embedding.Healthy || !st.Embedding.BreakerOpenUntil.Equal(until) || st.Embedding.Pending != 5 {
		t.Fatalf("%+v %v", st.Embedding, err)
	}
	// The background worker sleeps until the breaker closes and then
	// drains without a wasted call: two batches of four for five chunks.
	h.x.Start(ctx)
	waitFor(t, 10*time.Second, func() bool {
		n, _ := h.store.VectorCount(ctx)
		return n == 5
	}, "the worker never resumed after the breaker closed")
	if time.Now().Before(until) {
		t.Fatal("vectors appeared before the breaker closed")
	}
	if fake.Calls() != 2 {
		t.Fatalf("%d calls, want exactly the two batches", fake.Calls())
	}
}

func TestMaxChunksIsAHardCap(t *testing.T) {
	fake := embed.NewFake(8)
	cfg := embedRules("/work")
	cfg.MaxChunks = 3
	h := newHarnessOpt(t, cfg, withEmbedder(fake))
	ctx := context.Background()
	seedNotes(t, h, "work", 5)
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, h.store.db, "embed_pending"); n != 3 {
		t.Fatalf("%d chunks queued past a cap of 3", n)
	}
	rep, err := h.x.EmbedNow(ctx)
	if err != nil || rep.Embedded != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	st, err := h.x.Status(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Vectors != 3 || st.MaxChunks != 3 || st.ChunksTotal != 5 || !st.Embedding.Capped || st.Embedding.Pending != 0 {
		t.Fatalf("%+v %+v", st, st.Embedding)
	}
	// Later documents are extracted for keyword search but never queued,
	// and a prepare pass does not sneak them in either.
	seedLater(t, h, "/work/late.md", "a late arrival")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.x.embedPrepare(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, h.store.db, "embed_pending"); n != 0 {
		t.Fatalf("%d chunks queued past the cap", n)
	}
	if r, err := h.x.Search(ctx, SearchQuery{Query: "late arrival", Mode: "keyword"}); err != nil || len(r.Hits) != 1 {
		t.Fatalf("keyword search of an uncapped chunk: %+v %v", r, err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("calls %d", fake.Calls())
	}
}

func TestDimensionMismatchStopsTheEmbedWorker(t *testing.T) {
	h := newHarnessOpt(t, embedRules("/work"), withEmbedder(embed.NewFake(8)))
	ctx := context.Background()
	seedNotes(t, h, "work", 2)
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.x.EmbedNow(ctx); err != nil || rep.Embedded != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	// Same model name, different size (a re-deployed endpoint): the
	// stored vectors cannot be compared with what the endpoint returns
	// now, so the worker records the mismatch and stops.
	wide := embed.NewFake(16)
	x := h.reopenIndexer(t, withEmbedder(wide))
	seedLater(t, h, "/work/more.md", "one more note")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := x.EmbedNow(ctx)
	if err != nil || rep.Embedded != 0 || rep.Paused != PausedStopped {
		t.Fatalf("%+v %v", rep, err)
	}
	st, err := x.Status(ctx, "")
	if err != nil || st.Embedding.Healthy || !strings.Contains(st.Embedding.LastError, "16") || !strings.Contains(st.Embedding.LastError, "8") {
		t.Fatalf("%+v %v", st.Embedding, err)
	}
	if st.Vectors != 2 || st.Embedding.Pending != 1 {
		t.Fatalf("%+v", st)
	}
	if wide.Calls() != 0 {
		t.Fatalf("%d calls after the mismatch was known up front", wide.Calls())
	}
	// Searches degrade rather than compare vectors of two sizes.
	r, err := x.Search(ctx, SearchQuery{Query: "note", Mode: "hybrid"})
	if err != nil || r.ModeUsed != "keyword" || !strings.Contains(r.Degraded, "16") {
		t.Fatalf("%+v %v", r, err)
	}
}
