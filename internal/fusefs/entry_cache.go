//go:build !windows

package fusefs

import (
	"sync"
	"time"

	"cloudfs/internal/vfs"
)

// entryCache answers a lookup from the directory listing that was just read,
// for kernels that cannot answer it themselves.
//
// With CAP_READDIRPLUS the kernel asks for a name and its attributes in one
// request per directory, and dirHandle.Lookup replies out of the listing it
// already holds (fs.go). Without it — macFUSE speaks FUSE 7.19, and
// READDIRPLUS arrived in 7.21 — the kernel sends a separate LOOKUP per entry
// instead, each one a meta query, and without CAP_PARALLEL_DIROPS those
// lookups inside one directory are serialised. A cold `ls -l` over a
// 200-entry directory is then 200 serial round trips where Linux costs one.
//
// This holds the same listing dirHandle already built, keyed by directory, so
// the lookups that follow a readdir are answered from it. It is a second
// layer below the kernel's own dentry cache, never a longer-lived one: the
// TTL is bounded by the mount's EntryTimeout, and every invalidation that
// drops a kernel dentry drops the matching entry here first (see the calls in
// kernel_nodes.go, mount.go and the tree-changing operations in fs.go).
type entryCache struct {
	ttl time.Duration
	// maxDirs and maxEntries bound the cache: a walk of a 100k-file tree
	// must not grow it. The oldest listing is evicted first, which is also
	// the least useful one — a listing earns its keep in the burst of
	// lookups that immediately follows its readdir.
	maxDirs, maxEntries int

	mu      sync.Mutex
	byDir   map[uint64]*listingBatch
	order   []uint64 // directory inodes, oldest listing first
	entries int
	// byChild finds the name an inode is cached under, so a change to the
	// inode alone can drop it. There are no hard links here, so an inode
	// has at most one live name.
	byChild map[uint64]entryName
}

// listingBatch is one directory's listing, taken whole.
type listingBatch struct {
	expires time.Time
	byName  map[string]vfs.Attr
}

// entryName is a name in a directory.
type entryName struct {
	parent uint64
	name   string
}

const (
	// entryCacheTTL is the ceiling on how long a listed entry may answer.
	// It only has to cover the lookup burst that follows one readdir, which
	// is milliseconds; anything longer widens the window in which a change
	// this process did not make could be missed.
	entryCacheTTL = time.Second
	// entryCacheDirs and entryCacheEntries bound the cache. A tree walk
	// needs the directory it is in and the ones it is unwinding through,
	// not every directory it has ever seen.
	entryCacheDirs    = 64
	entryCacheEntries = 8192
)

// newEntryCache builds a cache whose entries never outlive the kernel's own
// dentries: ttl is the mount's EntryTimeout, and the cache takes the shorter
// of that and entryCacheTTL.
func newEntryCache(entryTimeout time.Duration) *entryCache {
	ttl := entryCacheTTL
	if entryTimeout > 0 && entryTimeout < ttl {
		ttl = entryTimeout
	}
	return &entryCache{
		ttl:        ttl,
		maxDirs:    entryCacheDirs,
		maxEntries: entryCacheEntries,
		byDir:      map[uint64]*listingBatch{},
		byChild:    map[uint64]entryName{},
	}
}

// putListing records a directory's listing, replacing whatever was held for
// it. A listing larger than the whole budget is not worth evicting everything
// else for, and is dropped instead.
func (c *entryCache) putListing(parent uint64, entries []vfs.Attr, now time.Time) {
	if c == nil || len(entries) == 0 || len(entries) > c.maxEntries {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictDirLocked(parent)
	batch := &listingBatch{expires: now.Add(c.ttl), byName: make(map[string]vfs.Attr, len(entries))}
	for _, at := range entries {
		batch.byName[at.Name] = at
		// An inode has one name here, so seeing it under a new one means it
		// moved. Retire the old entry rather than letting the index point
		// away from it: an entry the index cannot reach is one a later
		// invalidation cannot drop.
		if held, ok := c.byChild[at.Ino]; ok && (held.parent != parent || held.name != at.Name) {
			c.dropEntryLocked(held.parent, held.name)
		}
		c.byChild[at.Ino] = entryName{parent: parent, name: at.Name}
	}
	c.byDir[parent] = batch
	c.order = append(c.order, parent)
	c.entries += len(batch.byName)
	for len(c.order) > c.maxDirs || c.entries > c.maxEntries {
		c.evictDirLocked(c.order[0])
	}
}

// lookup answers a name from the listing its directory was read with.
func (c *entryCache) lookup(parent uint64, name string, now time.Time) (vfs.Attr, bool) {
	if c == nil {
		return vfs.Attr{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, ok := c.byDir[parent]
	if !ok {
		return vfs.Attr{}, false
	}
	if now.After(batch.expires) {
		c.evictDirLocked(parent)
		return vfs.Attr{}, false
	}
	at, ok := batch.byName[name]
	return at, ok
}

// dropEntry forgets one name, for the same reasons the kernel is told to drop
// its dentry for it.
func (c *entryCache) dropEntry(parent uint64, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropEntryLocked(parent, name)
}

func (c *entryCache) dropEntryLocked(parent uint64, name string) {
	batch, ok := c.byDir[parent]
	if !ok {
		return
	}
	at, ok := batch.byName[name]
	if !ok {
		return
	}
	delete(batch.byName, name)
	c.entries--
	if held, ok := c.byChild[at.Ino]; ok && held.parent == parent && held.name == name {
		delete(c.byChild, at.Ino)
	}
	if len(batch.byName) == 0 {
		c.evictDirLocked(parent)
	}
}

// dropIno forgets everything held about one inode: the listing it holds if it
// is a directory, and the name it is cached under in its parent.
func (c *entryCache) dropIno(ino uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictDirLocked(ino)
	if held, ok := c.byChild[ino]; ok {
		c.dropEntryLocked(held.parent, held.name)
	}
}

// evictDirLocked removes a directory's listing whole.
func (c *entryCache) evictDirLocked(parent uint64) {
	batch, ok := c.byDir[parent]
	if !ok {
		return
	}
	for name, at := range batch.byName {
		if held, ok := c.byChild[at.Ino]; ok && held.parent == parent && held.name == name {
			delete(c.byChild, at.Ino)
		}
	}
	c.entries -= len(batch.byName)
	delete(c.byDir, parent)
	for i, ino := range c.order {
		if ino == parent {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// clear forgets everything. DropKernelCaches turns a live mount back into a
// cold one; a second cache that survived it would make "cold" a lie.
func (c *entryCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byDir = map[uint64]*listingBatch{}
	c.byChild = map[uint64]entryName{}
	c.order = nil
	c.entries = 0
}

// size reports how many entries are held, for the bound test.
func (c *entryCache) size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries
}

// The Root side. entries is nil unless the kernel failed to offer
// CAP_READDIRPLUS, so on Linux none of this runs at all.

// cacheListing records a listing the directory handle just built.
func (r *Root) cacheListing(parent uint64, entries []vfs.Attr) {
	r.entries.Load().putListing(parent, entries, time.Now())
}

// cachedEntry answers a lookup from the listing its directory was read with.
func (r *Root) cachedEntry(parent uint64, name string) (vfs.Attr, bool) {
	return r.entries.Load().lookup(parent, name, time.Now())
}

// forgetEntry drops one name.
func (r *Root) forgetEntry(parent uint64, name string) { r.entries.Load().dropEntry(parent, name) }

// forgetIno drops everything held about one inode.
func (r *Root) forgetIno(ino uint64) { r.entries.Load().dropIno(ino) }

// forgetEntries drops the whole cache.
func (r *Root) forgetEntries() { r.entries.Load().clear() }
