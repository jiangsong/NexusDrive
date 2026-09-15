package vfs

import (
	"context"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// SearchAnswer is a name search with the two facts meta cannot know: whether
// each file is in the block cache, and how much of the tree the index holds.
type SearchAnswer struct {
	meta.SearchReport
	Coverage meta.Coverage
	// Crawling says a crawler pass is running, so the coverage is moving.
	Crawling bool
}

// Search answers a parsed query from the metadata index. Cached is asked of
// the block cache by content key; a file written locally and not yet
// uploaded carries a cloudfs-local: id, and that is the key write.go
// registered its blocks under, so it reports cached like any other.
func (f *FS) Search(ctx context.Context, q meta.SearchQuery) (SearchAnswer, error) {
	report, err := f.meta.Find(ctx, q)
	if err != nil {
		return SearchAnswer{}, err
	}
	for i := range report.Results {
		r := &report.Results[i]
		if r.Kind == provider.KindDir || r.RemoteID == "" {
			continue
		}
		// An empty file has no blocks to hold, the same answer Attr gives.
		r.Cached = r.Size == 0 || f.cache.Complete(cache.FileKey{Remote: r.Remote, RemoteID: r.RemoteID, Version: r.Version})
	}
	cov, err := f.meta.Coverage(ctx)
	if err != nil {
		return SearchAnswer{}, err
	}
	return SearchAnswer{SearchReport: report, Coverage: cov, Crawling: f.CrawlProgress().Running}, nil
}
