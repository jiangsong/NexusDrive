package index

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/embed"
)

// stubEmbedder is an embed.Fake whose vectors for chosen texts are fixed,
// so a test can decide what cosine says about them. Every other text gets
// the fake's hash-derived vector. Like the real client, it does not know
// its dimension before the first call.
type stubEmbedder struct {
	*embed.Fake
	name   string
	vecs   map[string][]float32
	probed atomic.Bool
}

func newStub(name string, dim int) *stubEmbedder {
	return &stubEmbedder{Fake: embed.NewFake(dim), name: name, vecs: map[string][]float32{}}
}

func (s *stubEmbedder) Model() string { return s.name }

func (s *stubEmbedder) Dim() int {
	if !s.probed.Load() {
		return 0
	}
	return s.Fake.Dim()
}

func (s *stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out, err := s.Fake.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	s.probed.Store(true)
	for i, t := range texts {
		if v, ok := s.vecs[t]; ok {
			out[i] = unit(append([]float32(nil), v...))
		}
	}
	return out, nil
}

// embedAll runs the store half of the embed worker: queue every chunk,
// embed it through e and store the vectors.
func embedAll(t *testing.T, s *Store, e embed.Embedder) {
	t.Helper()
	ctx := context.Background()
	s.SetEmbedCap(1 << 20)
	if _, err := s.QueueEmbeds(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		pend, err := s.PendingEmbeds(ctx, 64, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(pend) == 0 {
			return
		}
		texts := make([]string, len(pend))
		ids := make([]int64, len(pend))
		for i, p := range pend {
			texts[i], ids[i] = embedText(p), p.ChunkID
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutVectors(ctx, e.Model(), "int8", ids, vecs, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHybridRRFOrdersByFusedRank(t *testing.T) {
	s := openTestStore(t)
	// bm25 ranks by term density: many > some > once. The stub makes
	// cosine say the opposite of that for the first two and puts the
	// sparse document in the middle, so the three orders differ.
	const (
		many = "cloudfs cloudfs cloudfs cloudfs cloudfs"
		some = "cloudfs cloudfs and a few more words here"
		once = "cloudfs appears once inside a much longer sentence about other things"
	)
	seedDoc(t, s, "1", "/w/many.md", many)
	seedDoc(t, s, "2", "/w/some.md", some)
	seedDoc(t, s, "3", "/w/once.md", once)
	e := newStub("stub", 3)
	e.vecs[many] = []float32{1, 0, 0}
	e.vecs[some] = []float32{0, 1, 0}
	e.vecs[once] = []float32{0, 0, 1}
	e.vecs["cloudfs"] = []float32{0.1, 0.9, 0.5}
	embedAll(t, s, e)

	ctx := context.Background()
	run := func(mode string) SearchResult {
		t.Helper()
		r, err := s.SearchWith(ctx, SearchQuery{Query: "cloudfs", Roots: []string{"/"}, Mode: mode}, same, e)
		if err != nil {
			t.Fatal(mode, err)
		}
		return r
	}
	keyword := run("keyword")
	if got := paths(keyword.Hits); strings.Join(got, " ") != "/w/many.md /w/some.md /w/once.md" || keyword.ModeUsed != "keyword" {
		t.Fatalf("keyword: %v %s", got, keyword.ModeUsed)
	}
	vector := run("vector")
	if got := paths(vector.Hits); strings.Join(got, " ") != "/w/some.md /w/once.md /w/many.md" || vector.ModeUsed != "vector" || vector.Degraded != "" {
		t.Fatalf("vector: %v %+v", got, vector)
	}
	if vector.Hits[0].Score <= vector.Hits[1].Score || vector.Hits[1].Score <= vector.Hits[2].Score {
		t.Fatalf("vector scores not descending: %+v", vector.Hits)
	}
	// RRF with k=60: many 1/61+1/63, some 1/62+1/61, once 1/63+1/62.
	hybrid := run("hybrid")
	if got := paths(hybrid.Hits); strings.Join(got, " ") != "/w/some.md /w/many.md /w/once.md" || hybrid.ModeUsed != "hybrid" || hybrid.Degraded != "" {
		t.Fatalf("hybrid: %v %+v", got, hybrid)
	}
	want := []float64{1.0/62 + 1.0/61, 1.0/61 + 1.0/63, 1.0/63 + 1.0/62}
	for i, h := range hybrid.Hits {
		if d := h.Score - want[i]; d > 1e-9 || d < -1e-9 {
			t.Fatalf("hit %d score %v, want %v", i, h.Score, want[i])
		}
	}
	// The default mode is hybrid when an embedder is present.
	if r := run(""); r.ModeUsed != "hybrid" || r.Degraded != "" {
		t.Fatalf("default mode: %+v", r)
	}
	// Every hit is snippeted, scoped and offset like a keyword hit.
	for _, h := range hybrid.Hits {
		if h.Snippet == "" || h.OffsetKind != "file" || h.Stale {
			t.Fatalf("%+v", h)
		}
	}
	r, err := s.SearchWith(ctx, SearchQuery{Query: "cloudfs", Roots: []string{"/w/once.md"}, Mode: "hybrid"}, same, e)
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Path != "/w/once.md" {
		t.Fatalf("scoped hybrid: %+v %v", r, err)
	}
	// The query is embedded once per search; the chunks were embedded
	// once each (three documents, one batch).
	if e.Calls() != 1+4 {
		t.Fatalf("embed calls %d", e.Calls())
	}
}

func TestVectorModeNeedsAnEmbedder(t *testing.T) {
	// provider none at the indexer level: hybrid and vector run as keyword
	// and say so, without an error.
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("semantic marker"))
	h.list(t, "/work")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"hybrid", "vector"} {
		r, err := h.x.Search(ctx, SearchQuery{Query: "semantic marker", Mode: mode})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if r.ModeUsed != "keyword" || r.Degraded == "" || len(r.Hits) != 1 {
			t.Fatalf("%s: %+v", mode, r)
		}
	}
	// Nothing was queued for an embedder that does not exist.
	st, err := h.x.Status(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Embedding.Provider != "none" || st.Embedding.Healthy || st.Embedding.Pending != 0 || st.Vectors != 0 {
		t.Fatalf("%+v", st.Embedding)
	}
	if st.MaxChunks == 0 {
		t.Fatal("max_chunks not reported")
	}

	// An embedder that is present but unhealthy degrades the same way,
	// with the reason.
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "semantic marker")
	e := newStub("stub", 4)
	embedAll(t, s, e)
	e.SetError(errors.New("endpoint exploded"))
	r, err := s.SearchWith(ctx, SearchQuery{Query: "semantic marker", Mode: "vector"}, same, e)
	if err != nil || r.ModeUsed != "keyword" || !strings.Contains(r.Degraded, "endpoint exploded") || len(r.Hits) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	// A healthy embedder over an index with no vectors yet is honest too.
	s2 := openTestStore(t)
	seedDoc(t, s2, "1", "/w/a.md", "semantic marker")
	r, err = s2.SearchWith(ctx, SearchQuery{Query: "semantic marker", Mode: "hybrid"}, same, newStub("stub", 4))
	if err != nil || r.ModeUsed != "keyword" || r.Degraded == "" || len(r.Hits) != 1 {
		t.Fatalf("no vectors: %+v %v", r, err)
	}
}

func TestShortQueryUsesVectorsWhenPresent(t *testing.T) {
	s := openTestStore(t)
	const (
		about = "这份文档介绍网盘挂载与缓存"
		other = "another document about something else entirely"
	)
	seedDoc(t, s, "1", "/w/about.md", about)
	seedDoc(t, s, "2", "/w/other.md", other)
	e := newStub("stub", 3)
	e.vecs[about] = []float32{1, 0, 0}
	e.vecs[other] = []float32{0, 1, 0}
	e.vecs["网盘"] = []float32{0.9, 0.1, 0}
	ctx := context.Background()

	// Before any vector exists a two-rune query scans the chunks.
	r, err := s.SearchWith(ctx, SearchQuery{Query: "网盘", Mode: "hybrid"}, same, e)
	if err != nil || r.ModeUsed != "keyword" || len(r.Hits) != 1 || r.Hits[0].Path != "/w/about.md" {
		t.Fatalf("scan: %+v %v", r, err)
	}
	embedAll(t, s, e)
	// With vectors it goes to the embedder instead: the trigram index
	// cannot see a two-rune term, and the scan is the slow path.
	r, err = s.SearchWith(ctx, SearchQuery{Query: "网盘", Mode: "hybrid"}, same, e)
	if err != nil || r.ModeUsed != "vector" || r.Degraded != "" || r.Truncated {
		t.Fatalf("%+v %v", r, err)
	}
	if got := paths(r.Hits); len(got) != 2 || got[0] != "/w/about.md" {
		t.Fatalf("%v", got)
	}
	// An explicit keyword request still scans.
	r, err = s.SearchWith(ctx, SearchQuery{Query: "网盘", Mode: "keyword"}, same, e)
	if err != nil || r.ModeUsed != "keyword" || len(r.Hits) != 1 {
		t.Fatalf("keyword: %+v %v", r, err)
	}
	// Without an embedder there is nothing else to do.
	r, err = s.Search(ctx, SearchQuery{Query: "网盘"}, same)
	if err != nil || r.ModeUsed != "keyword" || len(r.Hits) != 1 {
		t.Fatalf("no embedder: %+v %v", r, err)
	}
}
