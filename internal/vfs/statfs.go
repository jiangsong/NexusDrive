package vfs

import (
	"context"
	"sync"
	"time"

	"cloudfs/internal/provider"
)

// Space is what df should say about the mount: the space of the backends
// behind it, when they can say. A pool reports its members' space divided
// by the replica count; a single drive reports its quota. When no backend
// can say, Known is false and the caller falls back to what it reported
// before — the cache budget, with a large notional capacity.
type Space struct {
	Total int64
	Used  int64
	Known bool
}

// Free is what remains.
func (s Space) Free() int64 {
	if s.Used > s.Total {
		return 0
	}
	return s.Total - s.Used
}

// spaceTTL bounds how often the backends are asked; df is called a lot.
const spaceTTL = time.Minute

type spaceCache struct {
	mu      sync.Mutex
	space   Space
	fetched time.Time
}

// Space sums the quotas of the distinct backends mounted here.
func (f *FS) Space(ctx context.Context) Space {
	f.space.mu.Lock()
	if !f.space.fetched.IsZero() && time.Since(f.space.fetched) < spaceTTL {
		s := f.space.space
		f.space.mu.Unlock()
		return s
	}
	f.space.mu.Unlock()

	var out Space
	seen := map[string]bool{}
	for _, m := range f.mounts {
		if seen[m.Remote] || m.Provider == nil {
			continue
		}
		seen[m.Remote] = true
		q, ok, err := provider.QuotaOf(ctx, m.Provider)
		if err != nil || !ok || q.Total <= 0 {
			continue
		}
		out.Known = true
		out.Total += q.Total
		out.Used += q.Used
	}
	f.space.mu.Lock()
	f.space.space, f.space.fetched = out, time.Now()
	f.space.mu.Unlock()
	return out
}
