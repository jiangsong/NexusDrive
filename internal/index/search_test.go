package index

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"cloudfs/internal/textract"
)

// seedDoc indexes text at p under remote "d" at version v1.
func seedDoc(t *testing.T, s *Store, remoteID, p, text string) {
	t.Helper()
	seedKind(t, s, remoteID, p, text, textract.KindMarkdown)
}

func seedKind(t *testing.T, s *Store, remoteID, p, text string, kind textract.Kind) {
	t.Helper()
	d := textract.Doc{Text: text, OffsetKind: "file"}
	_, err := s.UpsertDocument(context.Background(),
		Document{Remote: "d", RemoteID: remoteID, Version: "v1", Path: p, Kind: string(kind)},
		text, textract.ChunkDoc(d, kind, textract.DefaultChunkOptions()))
	if err != nil {
		t.Fatal(err)
	}
}

// same is a Current that agrees with every seeded document.
func same(string, string) (string, string, bool) { return "", "v1", true }

func search(t *testing.T, s *Store, q SearchQuery, cur Current) SearchResult {
	t.Helper()
	r, err := s.Search(context.Background(), q, cur)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func paths(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}

func TestSearchRanksByBM25(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/once.md", "cloudfs appears once among many other words here")
	seedDoc(t, s, "2", "/w/many.md", "cloudfs cloudfs cloudfs cloudfs")
	seedDoc(t, s, "3", "/w/none.md", "nothing relevant in this one")
	r := search(t, s, SearchQuery{Query: "cloudfs", Roots: []string{"/"}}, same)
	if len(r.Hits) != 2 || r.Hits[0].Path != "/w/many.md" || r.Hits[1].Path != "/w/once.md" {
		t.Fatalf("%+v", r)
	}
	if r.ModeUsed != "keyword" || r.Degraded != "" || r.Truncated {
		t.Fatalf("%+v", r)
	}
	if r.Hits[0].Score <= r.Hits[1].Score || r.Hits[1].Score <= 0 {
		t.Fatalf("scores not descending and positive: %v %v", r.Hits[0].Score, r.Hits[1].Score)
	}
	if r.Docs != 3 || r.Pending != 0 {
		t.Fatalf("docs %d pending %d", r.Docs, r.Pending)
	}
	h := r.Hits[0]
	if h.Seq != 0 || h.StartOff != 0 || h.EndOff != int64(len("cloudfs cloudfs cloudfs cloudfs")) || h.OffsetKind != "file" || h.Stale {
		t.Fatalf("%+v", h)
	}
}

func TestSearchRequiresEveryTerm(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/both.md", "alpha beta gamma")
	seedDoc(t, s, "2", "/w/one.md", "alpha only")
	r := search(t, s, SearchQuery{Query: "alpha gamma"}, same)
	if got := paths(r.Hits); len(got) != 1 || got[0] != "/w/both.md" {
		t.Fatalf("%v", got)
	}
}

func TestSearchNeverLeaksOutsideRoots(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/work/a.md", "shared secret phrase")
	seedDoc(t, s, "2", "/private/b.md", "shared secret phrase")
	seedDoc(t, s, "3", "/workshop/c.md", "shared secret phrase")
	seedDoc(t, s, "4", "/work", "shared secret phrase")
	// Equal scores fall back to path order, so results are deterministic.
	r := search(t, s, SearchQuery{Query: "secret phrase", Roots: []string{"/work"}, TopK: 50}, same)
	if got := paths(r.Hits); len(got) != 2 || got[0] != "/work" || got[1] != "/work/a.md" {
		t.Fatalf("%v", got)
	}
	r = search(t, s, SearchQuery{Query: "secret phrase", Roots: []string{"/work", "/private"}, TopK: 50}, same)
	if got := paths(r.Hits); len(got) != 3 || got[0] != "/private/b.md" || got[2] != "/work/a.md" {
		t.Fatalf("%v", got)
	}
	// An empty non-nil scope permits nothing; nil is unrestricted (the
	// meta.SearchWithin convention the MCP server already relies on).
	if r := search(t, s, SearchQuery{Query: "secret phrase", Roots: []string{}}, same); len(r.Hits) != 0 {
		t.Fatalf("empty roots returned %v", paths(r.Hits))
	}
	if r := search(t, s, SearchQuery{Query: "secret phrase", Roots: nil, TopK: 50}, same); len(r.Hits) != 4 {
		t.Fatalf("nil roots should be unrestricted: %v", paths(r.Hits))
	}
	if r := search(t, s, SearchQuery{Query: "secret phrase", Roots: []string{"/private", "/"}, TopK: 50}, same); len(r.Hits) != 4 {
		t.Fatalf("a root of / should lift the restriction: %v", paths(r.Hits))
	}
	// The short-query scan applies the same filter.
	r = search(t, s, SearchQuery{Query: "se", Roots: []string{"/work"}, TopK: 50}, same)
	if got := paths(r.Hits); len(got) != 2 || got[0] != "/work" || got[1] != "/work/a.md" {
		t.Fatalf("short query leaked: %v", got)
	}
	if r := search(t, s, SearchQuery{Query: "se", Roots: []string{}}, same); len(r.Hits) != 0 || r.Truncated {
		t.Fatalf("short query with empty roots: %+v", r)
	}
}

func TestTwoRuneChineseQueryReturnsHitsOrTruncated(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "这是关于网盘挂载的说明")
	seedDoc(t, s, "2", "/w/b.md", "这一篇与之无关")
	r, err := s.Search(context.Background(), SearchQuery{Query: "网盘", Roots: []string{"/"}}, same)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hits) == 0 && !r.Truncated {
		t.Fatal("a two-rune query answered 'no match' without scanning")
	}
	if len(r.Hits) != 1 || r.Hits[0].Path != "/w/a.md" || !strings.Contains(r.Hits[0].Snippet, "网盘") {
		t.Fatalf("%+v", r.Hits)
	}
	if r.Hits[0].Score != 1 || r.Truncated || r.ModeUsed != "keyword" {
		t.Fatalf("%+v", r)
	}
	// Case folds like the trigram index does.
	seedDoc(t, s, "3", "/w/c.md", "Mixed Case AB here")
	if r := search(t, s, SearchQuery{Query: "ab"}, same); len(r.Hits) != 1 || r.Hits[0].Path != "/w/c.md" {
		t.Fatalf("%+v", r.Hits)
	}
}

func TestShortQueryScanStopsAtItsBudget(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 30; i++ {
		seedDoc(t, s, fmt.Sprint(i), fmt.Sprintf("/w/%02d.md", i), "无关内容")
	}
	r, err := s.searchWithRowBudget(context.Background(), SearchQuery{Query: "网盘", Roots: []string{"/"}}, same, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Hits) != 0 {
		t.Fatalf("%+v", r)
	}
	// A budget that covers every row answers definitively.
	r, err = s.searchWithRowBudget(context.Background(), SearchQuery{Query: "网盘", Roots: []string{"/"}}, same, 30)
	if err != nil {
		t.Fatal(err)
	}
	if r.Truncated || len(r.Hits) != 0 {
		t.Fatalf("%+v", r)
	}
	// Enough hits inside the budget is not a truncation either.
	seedDoc(t, s, "hit", "/a/hit.md", "网盘 first in path order")
	r, err = s.searchWithRowBudget(context.Background(), SearchQuery{Query: "网盘", TopK: 1}, same, 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.Truncated || len(r.Hits) != 1 || r.Hits[0].Path != "/a/hit.md" {
		t.Fatalf("%+v", r)
	}
}

func TestHybridDegradesToKeyword(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "cloudfs index")
	for _, mode := range []string{"hybrid", "vector"} {
		r := search(t, s, SearchQuery{Query: "index", Roots: []string{"/"}, Mode: mode}, same)
		if r.ModeUsed != "keyword" || r.Degraded != DegradedNoEmbedding || len(r.Hits) != 1 {
			t.Fatalf("%s: %+v", mode, r)
		}
	}
	r := search(t, s, SearchQuery{Query: "index", Mode: "keyword"}, same)
	if r.Degraded != "" || len(r.Hits) != 1 {
		t.Fatalf("%+v", r)
	}
	if _, err := s.Search(context.Background(), SearchQuery{Query: "index", Mode: "quantum"}, same); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := s.Search(context.Background(), SearchQuery{Query: "  \t "}, same); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("blank query: %v", err)
	}
}

func TestStaleWhenMetaHasMovedOn(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "cloudfs")
	r := search(t, s, SearchQuery{Query: "cloudfs", Roots: []string{"/"}}, func(string, string) (string, string, bool) { return "", "v2", true })
	if len(r.Hits) != 1 || !r.Hits[0].Stale {
		t.Fatalf("hit not marked stale: %+v", r.Hits)
	}
	r = search(t, s, SearchQuery{Query: "cloudfs"}, func(string, string) (string, string, bool) { return "", "", false })
	if len(r.Hits) != 1 || !r.Hits[0].Stale {
		t.Fatalf("a vanished node should be stale: %+v", r.Hits)
	}
	r = search(t, s, SearchQuery{Query: "cloudfs"}, same)
	if len(r.Hits) != 1 || r.Hits[0].Stale {
		t.Fatalf("same version marked stale: %+v", r.Hits)
	}
	r = search(t, s, SearchQuery{Query: "cloudfs"}, nil)
	if len(r.Hits) != 1 || r.Hits[0].Stale {
		t.Fatalf("nil Current marked stale: %+v", r.Hits)
	}
}

func TestHitPathComesFromTheLiveTree(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/old/a.md", "cloudfs renamed")
	seedDoc(t, s, "2", "/work/b.md", "cloudfs renamed")
	moved := func(_, id string) (string, string, bool) {
		switch id {
		case "1":
			return "/new/a.md", "v1", true
		case "2":
			return "/private/b.md", "v1", true
		}
		return "", "", false
	}
	r := search(t, s, SearchQuery{Query: "renamed", Roots: []string{"/old", "/new"}}, moved)
	if got := paths(r.Hits); len(got) != 1 || got[0] != "/new/a.md" || r.Hits[0].Stale {
		t.Fatalf("%+v", r.Hits)
	}
	// A document whose live path left the scope before the indexer
	// caught up must not leak through the stale SQL filter.
	r = search(t, s, SearchQuery{Query: "renamed", Roots: []string{"/work"}}, moved)
	if len(r.Hits) != 0 {
		t.Fatalf("leaked %v", paths(r.Hits))
	}
	r = search(t, s, SearchQuery{Query: "renamed", Roots: nil}, moved)
	if got := paths(r.Hits); len(got) != 2 || got[0] != "/new/a.md" || got[1] != "/private/b.md" {
		t.Fatalf("%v", got)
	}
}

func TestSnippetBudgetIsRuneSafeAndResponseBudgetTruncates(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 20; i++ {
		seedDoc(t, s, fmt.Sprint(i), fmt.Sprintf("/w/%02d.md", i), strings.Repeat("云端", 300)+" 关键词 "+strings.Repeat("文件", 300))
	}
	r := search(t, s, SearchQuery{Query: "关键词", Roots: []string{"/"}, TopK: 20, MaxSnippetBytes: 100, MaxBytes: 800}, same)
	for _, h := range r.Hits {
		if len(h.Snippet) > 100 || !utf8.ValidString(h.Snippet) || !strings.Contains(h.Snippet, "关键词") {
			t.Fatalf("%q", h.Snippet)
		}
	}
	if !r.Truncated || len(r.Hits) == 0 || len(r.Hits) >= 20 {
		t.Fatalf("budget ignored: %d hits truncated=%v", len(r.Hits), r.Truncated)
	}
	// Without a response budget every hit comes back, none truncated.
	r = search(t, s, SearchQuery{Query: "关键词", TopK: 100, MaxSnippetBytes: 100}, same)
	if r.Truncated || len(r.Hits) < 20 {
		t.Fatalf("%d hits truncated=%v", len(r.Hits), r.Truncated)
	}
	// A snippet never exceeds the chunk it came from and defaults to 1 KiB.
	r = search(t, s, SearchQuery{Query: "关键词", TopK: 1}, same)
	if len(r.Hits) != 1 || len(r.Hits[0].Snippet) > defaultSnippetBytes || !utf8.ValidString(r.Hits[0].Snippet) {
		t.Fatalf("%d bytes", len(r.Hits[0].Snippet))
	}
}

func TestSnippetWindowKeepsTheMatchAtEitherEnd(t *testing.T) {
	// The match near the start: the window cannot be centred, so it
	// starts at the text's beginning; near the end it is pulled back.
	text := "关键词" + strings.Repeat("云端", 200)
	if got := snippet(text, []string{"关键词"}, 40); !strings.HasPrefix(got, "关键词") || len(got) > 40 || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
	text = strings.Repeat("云端", 200) + "关键词"
	if got := snippet(text, []string{"关键词"}, 40); !strings.HasSuffix(got, "关键词") || len(got) > 40 {
		t.Fatalf("%q", got)
	}
	// Case-insensitive location of an ASCII term.
	text = strings.Repeat("x", 500) + "NeEdLe" + strings.Repeat("y", 500)
	if got := snippet(text, []string{"needle"}, 20); !strings.Contains(got, "NeEdLe") || len(got) != 20 {
		t.Fatalf("%q", got)
	}
	// A term the fold cannot find (heading-only match) yields the start.
	if got := snippet(text, []string{"absent"}, 8); got != "xxxxxxxx" {
		t.Fatalf("%q", got)
	}
	if got := snippet("short", []string{"short"}, 100); got != "short" {
		t.Fatalf("%q", got)
	}
}

func TestSearchSkipsDirtyAndFailedDocuments(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "ok", "/w/ok.md", "cloudfs works")
	seedDoc(t, s, "bad", "/w/bad.md", "cloudfs works")
	if err := s.MarkFailed(context.Background(), Document{Remote: "d", RemoteID: "bad", Version: "v1", Path: "/w/bad.md"}, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	r := search(t, s, SearchQuery{Query: "cloudfs works"}, same)
	if got := paths(r.Hits); len(got) != 1 || got[0] != "/w/ok.md" {
		t.Fatalf("%v", got)
	}
	if r.Docs != 1 {
		t.Fatalf("docs %d", r.Docs)
	}
	r = search(t, s, SearchQuery{Query: "cl"}, same)
	if got := paths(r.Hits); len(got) != 1 || got[0] != "/w/ok.md" {
		t.Fatalf("short query: %v", got)
	}
}

func TestHitCarriesHeadingAndOffsetKind(t *testing.T) {
	s := openTestStore(t)
	md := "# Plan\n\nintro\n\n## Scope\n\nthe needle sentence lives here\n"
	doc := textract.Doc{Text: md, OffsetKind: "file", Headings: []textract.Heading{
		{Level: 1, Title: "Plan", Offset: 0},
		{Level: 2, Title: "Scope", Offset: int64(strings.Index(md, "## Scope"))},
	}}
	_, err := s.UpsertDocument(context.Background(),
		Document{Remote: "d", RemoteID: "md", Version: "v1", Path: "/w/plan.md", Kind: string(textract.KindMarkdown)},
		md, textract.ChunkDoc(doc, textract.KindMarkdown, textract.DefaultChunkOptions()))
	if err != nil {
		t.Fatal(err)
	}
	r := search(t, s, SearchQuery{Query: "needle sentence"}, same)
	if len(r.Hits) != 1 || r.Hits[0].OffsetKind != "file" || r.Hits[0].Heading != "Plan > Scope" {
		t.Fatalf("%+v", r.Hits)
	}
	if h := r.Hits[0]; md[h.StartOff:h.EndOff] != h.Snippet || !strings.HasPrefix(md[h.StartOff:], "## Scope") {
		t.Fatalf("offsets do not address the file: %+v", h)
	}
	text := "Chapter\nthe needle sentence lives here"
	_, err = s.UpsertDocument(context.Background(),
		Document{Remote: "d", RemoteID: "docx", Version: "v1", Path: "/w/plan.docx", Kind: string(textract.KindDocx)},
		text, textract.ChunkDoc(textract.Doc{Text: text, OffsetKind: "text"}, textract.KindDocx, textract.DefaultChunkOptions()))
	if err != nil {
		t.Fatal(err)
	}
	r = search(t, s, SearchQuery{Query: "needle sentence", Roots: []string{"/w"}, TopK: 5}, same)
	kinds := map[string]string{}
	for _, h := range r.Hits {
		kinds[h.Path] = h.OffsetKind
	}
	if kinds["/w/plan.md"] != "file" || kinds["/w/plan.docx"] != "text" {
		t.Fatalf("%v", kinds)
	}
}

func TestTopKDefaultsAndCaps(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 15; i++ {
		seedDoc(t, s, fmt.Sprint(i), fmt.Sprintf("/w/%02d.md", i), "cloudfs everywhere")
	}
	if r := search(t, s, SearchQuery{Query: "cloudfs"}, same); len(r.Hits) != defaultTopK || r.Truncated {
		t.Fatalf("%d hits truncated=%v", len(r.Hits), r.Truncated)
	}
	if r := search(t, s, SearchQuery{Query: "cloudfs", TopK: 3}, same); len(r.Hits) != 3 {
		t.Fatalf("%d hits", len(r.Hits))
	}
	if r := search(t, s, SearchQuery{Query: "cl", TopK: 3}, same); len(r.Hits) != 3 || r.Truncated {
		t.Fatalf("short: %d hits truncated=%v", len(r.Hits), r.Truncated)
	}
}
