package pool

import (
	"sync"
	"sync/atomic"
	"time"
)

// Resolving a file id to the replicas that serve it costs two or three index
// queries (resolveFile), and VFS readahead resolves the same file once per
// block, several at a time. replicaCache keeps the answer briefly
// (docs/pool-v2.md §4.5).
//
// The TTL is not what keeps it correct. Every change to the index
// invalidates the whole cache: each index transaction does (Pool.tx), and a
// statement outside a transaction goes through execIndex, which does too.
// That covers finishUpload, relocate, Delete, upsertReplica, repair and
// listing merges — all transactional — and marking a replica missing, trim
// and drain, which are not. The TTL only bounds how long an answer lives
// while nothing changes.
const (
	replicaCacheTTL = 5 * time.Second
	replicaCacheMax = 4096
)

// resolved is what resolveFile answers.
type resolved struct {
	pth string
	row entryRow
	// reps is shared by every reader of the cached answer: a caller copies
	// it before reordering or trimming.
	reps []replicaRow
}

type resolveKey struct{ id, version string }

type cachedResolve struct {
	resolved
	gen     uint64
	expires time.Time
}

type replicaCache struct {
	ttl time.Duration
	max int
	now func() time.Time
	gen atomic.Uint64

	mu sync.Mutex
	m  map[resolveKey]cachedResolve
}

func newReplicaCache(ttl time.Duration, max int, now func() time.Time) *replicaCache {
	return &replicaCache{ttl: ttl, max: max, now: now, m: map[resolveKey]cachedResolve{}}
}

// generation is taken before the index is read. put stores an answer only
// if no invalidation happened since, so a lookup that raced a write never
// caches what it read before the write.
func (c *replicaCache) generation() uint64 { return c.gen.Load() }

func (c *replicaCache) get(id, version string) (resolved, bool) {
	k := resolveKey{id, version}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return resolved{}, false
	}
	if e.gen != c.gen.Load() || !c.now().Before(e.expires) {
		delete(c.m, k)
		return resolved{}, false
	}
	return e.resolved, true
}

func (c *replicaCache) put(id, version string, gen uint64, v resolved) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen.Load() {
		return
	}
	now := c.now()
	if len(c.m) >= c.max {
		for k, e := range c.m {
			if e.gen != gen || !now.Before(e.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			clear(c.m)
		}
	}
	c.m[resolveKey{id, version}] = cachedResolve{resolved: v, gen: gen, expires: now.Add(c.ttl)}
}

// invalidate forgets every answer; the index changed.
func (c *replicaCache) invalidate() {
	c.gen.Add(1)
	c.mu.Lock()
	clear(c.m)
	c.mu.Unlock()
}

func (c *replicaCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
