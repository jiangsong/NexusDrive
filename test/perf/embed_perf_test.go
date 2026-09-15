package perf

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/embed"
	"cloudfs/internal/index"
)

// Embedding is metered per request, so the number of requests is the cost.
// The worker must send exactly ceil(chunks / batch) requests for a fresh
// index and none at all when nothing changed.

func TestEmbedCallsEqualCeilChunksOverBatch(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	const files, batch = 50, 8
	for i := 0; i < files; i++ {
		// One short paragraph each: one chunk per file.
		h.fake.Seed(fmt.Sprintf("work/doc%03d.md", i), []byte(fmt.Sprintf("document %d is about topic %d", i, i%7)))
	}
	if _, err := h.fs.ReadDirPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/work"}},
		Embedding: config.IndexEmbedding{Provider: "ollama", Model: "fake", Batch: batch}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := index.OpenStore(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := embed.NewFake(16)
	x, err := index.New(index.Options{FS: h.fs, Store: st, Config: cfg, ReconcileEvery: time.Hour, Embedder: fake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })

	rep, err := x.ReconcileNow(ctx)
	if err != nil || rep.Extracted != files {
		t.Fatalf("%s %v", rep, err)
	}
	stats, err := st.Stats(ctx)
	if err != nil || stats.Chunks != files {
		t.Fatalf("%+v %v: the seeded files were meant to be one chunk each", stats, err)
	}
	erep, err := x.EmbedNow(ctx)
	if err != nil || erep.Embedded != files {
		t.Fatalf("%+v %v", erep, err)
	}
	want := (files + batch - 1) / batch
	if got := fake.Calls(); got != want {
		t.Fatalf("embedding %d chunks in batches of %d cost %d calls, want %d", files, batch, got, want)
	}
	for i, n := range fake.Batches() {
		if n > batch || n == 0 {
			t.Fatalf("batch %d carried %d texts (batch size %d)", i, n, batch)
		}
	}
	if n, err := st.VectorCount(ctx); err != nil || n != files {
		t.Fatalf("vectors %d %v", n, err)
	}

	// Nothing changed: another reconcile and another embed turn cost no
	// request.
	if rep, err := x.ReconcileNow(ctx); err != nil || rep.Extracted != 0 {
		t.Fatalf("%s %v", rep, err)
	}
	if erep, err := x.EmbedNow(ctx); err != nil || erep.Embedded != 0 || erep.Calls != 0 {
		t.Fatalf("%+v %v", erep, err)
	}
	if got := fake.Calls(); got != want {
		t.Fatalf("an unchanged index cost %d more calls", got-want)
	}

	// One revised file costs one request for its one chunk.
	h.fake.Seed("work/doc007.md", []byte("document 7 was rewritten about topic 3"))
	pastListingWindow()
	if err := h.fs.Refresh(ctx, mustIno(t, h, "/work")); err != nil {
		t.Fatal(err)
	}
	if rep, err := x.ReconcileNow(ctx); err != nil || rep.Extracted != 1 {
		t.Fatalf("%s %v", rep, err)
	}
	if erep, err := x.EmbedNow(ctx); err != nil || erep.Embedded != 1 || erep.Calls != 1 {
		t.Fatalf("%+v %v", erep, err)
	}
	if got := fake.Calls(); got != want+1 {
		t.Fatalf("one changed file cost %d calls", got-want)
	}
	if n, err := st.VectorCount(ctx); err != nil || n != files {
		t.Fatalf("vectors %d after a re-extraction, want %d", n, files)
	}
}
