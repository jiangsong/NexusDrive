package mcpsrv

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSearchAuthorizedResultsAreLimitedAfterScope(t *testing.T) {
	for _, allow := range [][]string{nil, {"/visible"}} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			e := newEnv(t, Options{Allow: allow})
			for i := 0; i < 100; i++ {
				e.fake.Seed(fmt.Sprintf("hidden-%03d.go", i), []byte("hidden"))
			}
			e.fake.Seed("visible/result.go", []byte("visible"))
			if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{"", "/visible"} {
				if root == "" && allow == nil {
					continue
				}
				var out searchOutput
				res := e.call(t, "search", searchInput{Path: root, Query: ".go", MaxResults: 1}, &out)
				if res.IsError || len(out.Hits) != 1 || out.Hits[0].Path != "/visible/result.go" || out.Truncated {
					t.Fatalf("scope=%q: %+v %+v", root, res, out)
				}
			}
		})
	}
}

func TestSearchGlobRespectsAllowlist(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/notes/plan.md", []byte("x"))
	e.fake.Seed("work/main.go", []byte("xx"))
	e.fake.Seed("private/secret.md", []byte("x"))
	if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
		t.Fatal(err)
	}
	var out searchOutput
	e.call(t, "search", searchInput{Glob: "*.md"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Path != "/work/notes/plan.md" || out.Hits[0].Size != 1 || out.Hits[0].Kind != "file" || out.Hits[0].MTime.IsZero() {
		t.Fatalf("%+v", out.Hits)
	}
	if res := e.call(t, "search", searchInput{Path: "/private", Glob: "*.md"}, nil); !res.IsError {
		t.Fatal("a scope outside the allow-list must fail, not answer empty")
	}
	e.call(t, "search", searchInput{Query: "plan", Ext: "go"}, &out)
	if len(out.Hits) != 0 {
		t.Fatalf("ext must AND with the query: %+v", out.Hits)
	}
	e.call(t, "search", searchInput{Kind: "file", Sort: "size", MinSize: 1}, &out)
	if len(out.Hits) != 2 || out.Hits[0].Path != "/work/main.go" || out.Coverage.Known == 0 || out.Coverage.Listed != out.Coverage.Known {
		t.Fatalf("%+v", out)
	}
	e.call(t, "search", searchInput{Kind: "dir"}, &out)
	if len(out.Hits) != 2 || out.Hits[0].Kind != "dir" || out.Hits[0].Cached {
		t.Fatalf("kind=dir under /work: %+v", out.Hits)
	}
	e.call(t, "search", searchInput{Query: "ext:go dm:>2000", Sort: "-name"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Path != "/work/main.go" {
		t.Fatalf("grammar in query: %+v", out.Hits)
	}
	for _, bad := range []searchInput{{}, {Query: "  "}, {Kind: "link"}, {Query: "a", Sort: "bogus"}, {Query: "a", ModifiedAfter: "yesterday"}, {Query: "size:lots"}} {
		if res := e.call(t, "search", bad, nil); !res.IsError {
			t.Fatalf("%+v should fail", bad)
		}
	}
}

func TestSearchCachedFlagFeedsContentSearch(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("src/a.go", []byte("needle here\n"))
	e.fake.Seed("src/b.go", []byte("needle there\n"))
	if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadFileRange(context.Background(), "/src/a.go", 0, 12); err != nil {
		t.Fatal(err)
	}
	var out searchOutput
	e.call(t, "search", searchInput{Ext: "go", Sort: "name"}, &out)
	if len(out.Hits) != 2 || !out.Hits[0].Cached || out.Hits[1].Cached {
		t.Fatalf("cached flags: %+v", out.Hits)
	}
	e.call(t, "search", searchInput{Ext: "go", Content: "needle"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Path != "/src/a.go" || !strings.Contains(out.Note, "skipped 1") {
		t.Fatalf("content search over the cached flag: %+v", out)
	}
}
