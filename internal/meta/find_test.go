package meta

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

// seedTyped builds /src with files whose size, mtime and extension differ.
func seedTyped(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	src, err := s.Upsert(ctx, dir(RootIno, "src"))
	if err != nil {
		t.Fatal(err)
	}
	for i, spec := range []struct {
		name string
		size int64
		age  time.Duration
	}{
		{"main.go", 4096, 0}, {"tiny.go", 10, 48 * time.Hour}, {"notes.md", 2048, 24 * time.Hour}, {"README.md", 300, 72 * time.Hour}, {"big.bin", 5 << 20, 96 * time.Hour},
	} {
		n := file(src.Ino, spec.name, spec.size)
		n.MTime = base.Add(-spec.age)
		n.Remote, n.RemoteID, n.Version = "ali", fmt.Sprintf("id%d", i), "v1"
		if _, err := s.Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
}

func paths(rs []SearchResult) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return fmt.Sprint(out)
}

func find(t *testing.T, s *Store, raw, sort string) SearchReport {
	t.Helper()
	rep, err := s.Find(context.Background(), SearchQuery{Filter: mustParse(t, raw), Limit: 10, Sort: sort})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestFindFiltersByExtensionAndSize(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	rep := find(t, s, "ext:go size:>1k", "")
	if !rep.Complete || paths(rep.Results) != "[/src/main.go]" {
		t.Fatalf("%+v", rep)
	}
	if r := rep.Results[0]; r.Size != 4096 || r.Kind != provider.KindFile || r.MTime.IsZero() || r.RemoteID != "id0" || r.Remote != "ali" || r.Version != "v1" || r.Cached {
		t.Fatalf("thin result: %+v", r)
	}
	if got := paths(find(t, s, "size:<1k type:file", "name").Results); got != "[/src/README.md /src/tiny.go]" {
		t.Fatalf("size upper bound: %s", got)
	}
	if got := paths(find(t, s, "size:2048..4096", "name").Results); got != "[/src/main.go /src/notes.md]" {
		t.Fatalf("size range: %s", got)
	}
}

func TestFindFiltersByModifiedTime(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	// dm: is local-time calendar arithmetic; the seed is fixed in UTC, so
	// pick bounds a whole day away from any zone boundary.
	after := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	rep, err := s.Find(context.Background(), SearchQuery{Filter: Filter{Kind: "file", MinSize: -1, MaxSize: -1, ModifiedAfter: after}, Limit: 10, Sort: "name"})
	if err != nil || paths(rep.Results) != "[/src/main.go /src/notes.md /src/tiny.go]" {
		t.Fatalf("after: %s %v", paths(rep.Results), err)
	}
	rep, err = s.Find(context.Background(), SearchQuery{Filter: Filter{Kind: "file", MinSize: -1, MaxSize: -1, ModifiedBefore: after}, Limit: 10, Sort: "name"})
	if err != nil || paths(rep.Results) != "[/src/big.bin /src/README.md]" {
		t.Fatalf("before: %s %v", paths(rep.Results), err)
	}
	// The grammar in the same zone the store answers in.
	local := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).In(time.Local)
	got := find(t, s, fmt.Sprintf("type:file dm:%s", local.Format("2006-01-02")), "name")
	if len(got.Results) == 0 || got.Results[0].Path != "/src/main.go" {
		t.Fatalf("dm:day: %s", paths(got.Results))
	}
}

func TestFindSortsByModifiedTime(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	if got := paths(find(t, s, "type:file", "mtime").Results); got != "[/src/main.go /src/notes.md /src/tiny.go /src/README.md /src/big.bin]" {
		t.Fatalf("mtime order: %s", got)
	}
	if got := paths(find(t, s, "type:file", "size").Results); got != "[/src/big.bin /src/main.go /src/notes.md /src/README.md /src/tiny.go]" {
		t.Fatalf("size order: %s", got)
	}
	if got := paths(find(t, s, "type:file", "-size").Results); got != "[/src/tiny.go /src/README.md /src/notes.md /src/main.go /src/big.bin]" {
		t.Fatalf("reversed size order: %s", got)
	}
	if got := paths(find(t, s, "type:file", "name").Results); got != "[/src/big.bin /src/main.go /src/notes.md /src/README.md /src/tiny.go]" {
		t.Fatalf("folded name order: %s", got)
	}
	if got := paths(find(t, s, "type:file", "-path").Results); got != "[/src/tiny.go /src/notes.md /src/main.go /src/big.bin /src/README.md]" {
		t.Fatalf("reversed path order: %s", got)
	}
	if _, err := s.Find(context.Background(), SearchQuery{Filter: Filter{Kind: "file", MinSize: -1, MaxSize: -1}, Limit: 10, Sort: "bogus"}); err == nil {
		t.Fatal("an unknown sort key must be refused")
	}
	if _, err := s.Find(context.Background(), SearchQuery{Filter: Filter{Kind: "link", MinSize: -1, MaxSize: -1}, Limit: 10}); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
}

func TestFindKindDirReturnsOnlyDirectories(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	if got := paths(find(t, s, "type:dir", "").Results); got != "[/src]" {
		t.Fatalf("%s", got)
	}
	if got := paths(find(t, s, "src type:dir", "").Results); got != "[/src]" {
		t.Fatalf("anchored kind: %s", got)
	}
}

func TestGlobAndExtensionAgree(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	a, b := find(t, s, "*.md", ""), find(t, s, "ext:md", "")
	if paths(a.Results) != paths(b.Results) || len(a.Results) != 2 {
		t.Fatalf("glob %s vs ext %s", paths(a.Results), paths(b.Results))
	}
	if got := paths(find(t, s, "-ext:md type:file -big", "").Results); got != "[/src/main.go /src/tiny.go]" {
		t.Fatalf("negation: %s", got)
	}
	if got := paths(find(t, s, "REA*.md", "").Results); got != "[/src/README.md]" {
		t.Fatalf("folded glob: %s", got)
	}
	if got := paths(find(t, s, "?iny.go", "").Results); got != "[/src/tiny.go]" {
		t.Fatalf("single-character wildcard: %s", got)
	}
	if got := paths(find(t, s, "-*.go type:file", "name").Results); got != "[/src/big.bin /src/notes.md /src/README.md]" {
		t.Fatalf("negated glob: %s", got)
	}
	if got := paths(find(t, s, "path:src -path:src/main ext:go", "").Results); got != "[/src/tiny.go]" {
		t.Fatalf("path and negated path: %s", got)
	}
	if got := paths(find(t, s, `"main.go"`, "").Results); got != "[/src/main.go]" {
		t.Fatalf("quoted literal: %s", got)
	}
}

func TestGlobEscapesBrackets(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, name := range []string{"[draft] plan.md", "d plan.md", "plan.md"} {
		if _, err := s.Upsert(ctx, file(RootIno, name, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got := paths(find(t, s, "[draft]*", "").Results); got != "[/[draft] plan.md]" {
		t.Fatalf("a bracket is a literal, not a class: %s", got)
	}
	if got := paths(find(t, s, "*plan*", "name").Results); got != "[/[draft] plan.md /d plan.md /plan.md]" {
		t.Fatalf("%s", got)
	}
}

func TestFilteredQueryStillReportsItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("wide-tree budget")
	}
	s, _ := openTest(t)
	root, _ := s.Upsert(context.Background(), dir(RootIno, "wide"))
	seedWide(t, s, root.Ino, 200, 300) // 60 000 .txt leaves
	rep := find(t, s, "ext:txt", "size")
	if rep.Complete || len(rep.Results) != 10 {
		t.Fatalf("complete=%v n=%d", rep.Complete, len(rep.Results))
	}
	// No anchor at all: the whole tree is walked under the same budget.
	rep = find(t, s, "type:file", "")
	if rep.Complete || len(rep.Results) != 10 {
		t.Fatalf("unanchored: complete=%v n=%d", rep.Complete, len(rep.Results))
	}
}

func TestLegacySearchSignaturesStillWork(t *testing.T) {
	s, _ := openTest(t)
	seedTyped(t, s)
	if got, err := s.Search(context.Background(), "notes", 10); err != nil || len(got) != 1 || got[0].Size != 2048 {
		t.Fatalf("%+v %v", got, err)
	}
	if rep, err := s.SearchReport(context.Background(), "src/", []string{"/src"}, 10); err != nil || len(rep.Results) != 5 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got, err := s.SearchWithin(context.Background(), "src/", []string{"/other"}, 10); err != nil || len(got) != 0 {
		t.Fatalf("scope: %+v %v", got, err)
	}
	// Whitespace now separates AND-ed words; a space in a name needs quotes.
	if got, err := s.Search(context.Background(), "main notes", 10); err != nil || len(got) != 0 {
		t.Fatalf("two words AND: %+v %v", got, err)
	}
	if got, err := s.Search(context.Background(), "size:lots", 10); err == nil {
		t.Fatalf("a bad filter must be an error, not a literal: %+v", got)
	}
}
