package vfs

import (
	"context"
	"testing"

	"cloudfs/internal/meta"
)

func TestSearchFillsCachedFromTheBlockCache(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/hot.txt", []byte("0123456789"))
	e.fake.Seed("docs/cold.txt", []byte("0123456789"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadFileRange(ctx, "/ali/docs/hot.txt", 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.meta.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := meta.ParseQuery("ext:txt")
	if err != nil {
		t.Fatal(err)
	}
	ans, err := e.fs.Search(ctx, meta.SearchQuery{Filter: f, Limit: 10, Sort: "name"})
	if err != nil || len(ans.Results) != 2 {
		t.Fatalf("%+v %v", ans, err)
	}
	if ans.Results[0].Name != "cold.txt" || ans.Results[0].Cached || !ans.Results[1].Cached {
		t.Fatalf("cached flags: %+v", ans.Results)
	}
	if ans.Coverage.Known == 0 || ans.Coverage.Listed == 0 || ans.Coverage.Listed > ans.Coverage.Known {
		t.Fatalf("coverage: %+v", ans.Coverage)
	}
	if ans.Crawling {
		t.Fatal("no crawler is running")
	}
	// A directory is never "cached", and a file written locally reports
	// its staged content the same way a downloaded one does.
	dirs, err := e.fs.Search(ctx, meta.SearchQuery{Filter: meta.Filter{Kind: "dir", MinSize: -1, MaxSize: -1}, Limit: 10})
	if err != nil || len(dirs.Results) == 0 {
		t.Fatalf("%+v %v", dirs, err)
	}
	for _, r := range dirs.Results {
		if r.Cached {
			t.Fatalf("a directory reports cached: %+v", r)
		}
	}
}
