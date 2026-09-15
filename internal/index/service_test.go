package index

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/meta"
)

func TestSearchResolvesHitsAgainstTheLiveTree(t *testing.T) {
	h := newHarness(t, rulesAll("/"))
	ctx := context.Background()
	h.fake.Seed("dir/a.md", []byte("the quick brown fox"))
	h.list(t, "/dir")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	// A rewrite meta knows about but the index has not seen is stale.
	h.fake.Seed("dir/a.md", []byte("the quick brown fox, again"))
	h.advance(2 * time.Second) // past the listing's protection of just-fetched entries
	dir, _ := h.fs.StatPath(ctx, "/dir")
	if err := h.fs.Refresh(ctx, dir.Ino); err != nil {
		t.Fatal(err)
	}
	res, err := h.x.Search(ctx, SearchQuery{Query: "brown fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !res.Hits[0].Stale {
		t.Fatalf("a newer version in meta must mark the hit stale: %+v", res.Hits)
	}
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	// Rename without a reconcile: the document still carries the old path
	// and the meta lookup by remote id must hand the search the new one.
	if err := h.fs.Rename(ctx, meta.RootIno, "dir", meta.RootIno, "moved"); err != nil {
		t.Fatal(err)
	}
	res, err = h.x.Search(ctx, SearchQuery{Query: "brown fox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "/moved/a.md" || res.Hits[0].Stale {
		t.Fatalf("%+v", res.Hits)
	}
}

func TestStatusReportsThePathAndTheWhole(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("hello"))
	h.fake.Seed("work/big.bin", []byte("binary"))
	h.fake.Seed("other/b.md", []byte("outside"))
	h.list(t, "/work")
	h.list(t, "/other")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	whole, err := h.x.Status(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !whole.Enabled || whole.Docs.OK != 1 || whole.ChunksTotal != 1 || whole.State != "" || whole.MaxTotalText <= 0 {
		t.Fatalf("%+v", whole)
	}
	cases := map[string]struct {
		state, covered string
		chunks         int
	}{
		"/work/a.md":    {StateOK, "/work", 1},
		"/work/big.bin": {StateUncovered, "/work", 0},
		"/other/b.md":   {StateUncovered, "", 0},
		"/work":         {"", "/work", 0},
		"/other":        {StateUncovered, "", 0},
		"/work/new.md":  {"", "/work", 0},
	}
	for p, want := range cases {
		st, err := h.x.Status(ctx, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.State != want.state || st.Covered != want.covered || st.Chunks != want.chunks {
			t.Fatalf("%s: got state=%q covered=%q chunks=%d, want %+v", p, st.State, st.Covered, st.Chunks, want)
		}
		if want.covered != "" && st.RuleSource != "config" {
			t.Fatalf("%s: rule source %q", p, st.RuleSource)
		}
	}
	// A file queued but not yet extracted is pending.
	h.fake.Seed("work/later.md", []byte("later"))
	h.advance(2 * time.Second)
	dir, _ := h.fs.StatPath(ctx, "/work")
	if err := h.fs.Refresh(ctx, dir.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := h.x.reconcile(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	st, err := h.x.Status(ctx, "/work/later.md")
	if err != nil || st.State != StatePending {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestAddAndRemoveRuleChangeWhatIsIndexed(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("in work"))
	h.fake.Seed("notes/n.md", []byte("in notes"))
	h.list(t, "/work")
	h.list(t, "/notes")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/notes/n.md"); ok {
		t.Fatal("indexed outside the rules")
	}
	if err := h.x.AddRule(ctx, Rule{Path: "/notes"}); err != nil {
		t.Fatal(err)
	}
	st, err := h.x.Status(ctx, "/notes/n.md")
	if err != nil || st.State != StatePending || st.Covered != "/notes" || st.RuleSource != "tool" {
		t.Fatalf("after AddRule: %+v %v", st, err)
	}
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/notes/n.md"); !ok {
		t.Fatal("the new rule's file was not indexed")
	}
	views, err := h.x.Rules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Path != "/notes" || views[0].Documents != 1 || views[1].Path != "/work" || views[1].Documents != 1 {
		t.Fatalf("%+v", views)
	}
	if err := h.x.RemoveRule(ctx, "/work"); !errors.Is(err, ErrConfigRule) {
		t.Fatalf("removing a config rule: %v", err)
	}
	if err := h.x.RemoveRule(ctx, "/notes"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/notes/n.md"); ok {
		t.Fatal("the removed rule's document survived")
	}
	if _, ok, _ := h.store.DocumentByPath(ctx, "/work/a.md"); !ok {
		t.Fatal("the config rule's document was dropped")
	}
	if err := h.x.RemoveRule(ctx, "/notes"); !errors.Is(err, ErrNoRule) {
		t.Fatalf("removing twice: %v", err)
	}
}

func TestRebuildAndRetry(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx := context.Background()
	h.fake.Seed("work/a.md", []byte("hello"))
	h.fake.Seed("work/bomb.docx", bombDocx(t))
	h.list(t, "/work")
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := h.x.Status(ctx, "/work/bomb.docx")
	if err != nil || st.State != StateFailed || st.Error == "" {
		t.Fatalf("%+v %v", st, err)
	}
	if len(st.Failed) != 1 || st.Failed[0].Path != "/work/bomb.docx" {
		t.Fatalf("%+v", st.Failed)
	}
	n, err := h.x.Retry(ctx, "/work")
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if err := h.x.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	whole, err := h.x.Status(ctx, "")
	if err != nil || whole.Docs.OK != 0 || whole.Docs.Failed != 0 || whole.Pending != 0 {
		t.Fatalf("after rebuild: %+v %v", whole, err)
	}
	if _, err := h.x.ReconcileNow(ctx); err != nil {
		t.Fatal(err)
	}
	whole, _ = h.x.Status(ctx, "")
	if whole.Docs.OK != 1 || whole.Docs.Failed != 1 || whole.FetchBytesTotal == 0 {
		t.Fatalf("after rebuild and reconcile: %+v", whole)
	}
}
