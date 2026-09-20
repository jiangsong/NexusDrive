package cache

import "os"

// Cached content is immutable for as long as it is cached: a block file is
// written to a temporary and renamed into place, and a hydrated file is
// published the same way, so nothing ever rewrites the bytes behind a name.
// That makes a descriptor worth keeping across reads. Opening and closing one
// per read is what the MCP read path (FS.ReadFileRange, which unlike the FUSE
// handle path holds no lease) paid for every range it served, and what
// hydrated reads paid twice over, since OpenWhole opens under the cache lock.
//
// A kept descriptor must never outlive the file it names. Both kinds are
// therefore retired at the single point that removes the index entry for the
// content — dropBlockLocked for a block, detachWholeLocked for a hydrated
// object — which every eviction, replacement, hydration and Forget already
// funnels through, and which is where blockMeta.f has always been closed.
// Retiring while a read is in flight defers the close to the last reader
// (readRefs), so a read can never see os.ErrClosed from our own retirement;
// the unlinked inode it finishes reading still holds exactly the bytes the
// cache promised.
//
// The two sets below bound how many descriptors are held and are what the LRU
// victim search walks, so the search costs the cap and not the cache size.
const (
	// maxOpenReads bounds descriptors held for whole block files.
	maxOpenReads = 256
	// maxOpenWholeReads bounds descriptors held for hydrated files. Hydrated
	// files are whole-file reads, so far fewer are hot at once than blocks.
	maxOpenWholeReads = 128
)

// readFD is the kept descriptor and its in-flight readers, embedded in the
// index entry whose lifetime governs it.
type readFD struct {
	f *os.File
	// refs counts reads currently using f; dead says f has been retired and
	// the last of those reads closes it.
	refs int
	dead bool
}

// open opens a cache file for reading through the injectable seam.
func (c *Cache) open(name string) (*os.File, error) {
	if c.opt.Open != nil {
		return c.opt.Open(name)
	}
	return os.Open(name)
}

// borrowBlockRead hands out the descriptor kept for a whole block on disk,
// holding it open for the duration of the read. It returns nil when the block
// has none, when it is no longer the entry the caller located, or when the
// block has since become partial (whose descriptor PutRange writes through).
func (c *Cache) borrowBlockRead(id blockID, m *blockMeta) *os.File {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocks[id] != m || m.read.f == nil || m.read.dead || m.partial != nil {
		return nil
	}
	m.read.refs++
	return m.read.f
}

// keepBlockRead installs a freshly opened descriptor on the block so the next
// read finds it, and holds it for this read. It reports whether the cache took
// ownership; when it did not, the caller closes the descriptor as before.
// The block must still be the entry the read located: a replacement in between
// named a different inode, and installing f on it would serve the old bytes.
func (c *Cache) keepBlockRead(id blockID, m *blockMeta, f *os.File) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocks[id] != m || m.read.f != nil || m.partial != nil {
		return false
	}
	if len(c.readOpen) >= maxOpenReads && !c.closeColdestBlockReadLocked() {
		return false
	}
	if c.readOpen == nil {
		c.readOpen = map[blockID]*blockMeta{}
	}
	// dead belongs to the descriptor being replaced, not to the entry. An
	// entry can be retired and then read again — a block whose object survived
	// the retirement because another file still references it — and leaving
	// the flag set would have releaseBlockRead close this descriptor the moment
	// the read ends, every time, so the entry never keeps one again.
	m.read.dead = false
	m.read.f, m.read.refs = f, 1
	c.readOpen[id] = m
	return true
}

// releaseBlockRead ends a read and closes a descriptor retired while it ran.
func (c *Cache) releaseBlockRead(m *blockMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m.read.refs--
	if m.read.dead && m.read.refs == 0 && m.read.f != nil {
		m.read.f.Close()
		m.read.f = nil
	}
}

// retireBlockReadLocked gives up the descriptor kept for a block, deferring
// the close to the last in-flight reader. c.mu must be held.
func (c *Cache) retireBlockReadLocked(id blockID, m *blockMeta) {
	// The entry leaves the set whether or not it still holds a descriptor:
	// one left behind would be a slot the victim search can pick and free
	// nothing from, which is how the cap leaks.
	if c.readOpen[id] == m {
		delete(c.readOpen, id)
	}
	if m.read.f == nil {
		return
	}
	if m.read.refs > 0 {
		m.read.dead = true
		return
	}
	m.read.f.Close()
	m.read.f = nil
}

// closeColdestBlockReadLocked frees one slot by retiring the least recently
// read block that holds a descriptor. It walks only the blocks holding one, so
// the scan costs maxOpenReads and not the size of the cache. c.mu must be held.
func (c *Cache) closeColdestBlockReadLocked() bool {
	var victim *blockMeta
	var victimID blockID
	for id, m := range c.readOpen {
		if m.read.refs > 0 {
			continue // a read is using it; taking it would not free anything
		}
		if victim == nil || m.lastAccess.Before(victim.lastAccess) {
			victim, victimID = m, id
		}
	}
	if victim == nil {
		return false
	}
	c.retireBlockReadLocked(victimID, victim)
	return true
}

// readWhole serves a read of the hydrated file for k, reusing one descriptor
// per object across reads. It is the internal counterpart of OpenWhole, whose
// WholeFile lease hands an *os.File to callers that seek and splice through
// it; those cannot share one descriptor, so this path keeps its own.
//
// The object's reader count is held for the read, so GC cannot retire the
// object — and with it the descriptor — underneath one.
func (c *Cache) readWhole(k FileKey, at int64, dst []byte) (int, error) {
	fh := k.hash()
	c.mu.Lock()
	fs := c.files[fh]
	if fs == nil || fs.whole == nil || !fs.hydrated {
		c.mu.Unlock()
		return 0, os.ErrNotExist
	}
	o := fs.whole
	o.readers++
	o.lastAccess, o.hot = c.opt.Now(), true
	if o.read.f != nil && !o.read.dead {
		o.read.refs++
		f := o.read.f
		c.mu.Unlock()
		n, err := f.ReadAt(dst, at)
		c.releaseWholeRead(o)
		return n, err
	}
	c.mu.Unlock()

	f, err := c.open(c.hydratedPath(fh))
	if err != nil {
		c.mu.Lock()
		o.readers--
		if os.IsNotExist(err) {
			c.detachWholeLocked(fh, fs)
		}
		c.releaseObjectLocked(o)
		c.mu.Unlock()
		return 0, err
	}
	keep := c.keepWholeRead(fh, o, f)
	n, err := f.ReadAt(dst, at)
	if keep {
		c.releaseWholeRead(o)
	} else {
		f.Close()
		c.mu.Lock()
		o.readers--
		c.releaseObjectLocked(o)
		c.mu.Unlock()
	}
	return n, err
}

// keepWholeRead installs a descriptor on the hydrated object, keeping this
// read's reference on it. The object must still be the one published for fh:
// a replacement published a different inode in between.
func (c *Cache) keepWholeRead(fh string, o *wholeObject, f *os.File) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if fs == nil || fs.whole != o || !fs.hydrated || o.read.f != nil {
		return false
	}
	if len(c.readOpenWhole) >= maxOpenWholeReads && !c.closeColdestWholeReadLocked() {
		return false
	}
	if c.readOpenWhole == nil {
		c.readOpenWhole = map[diskIdentity]*wholeObject{}
	}
	// See keepBlockRead: the flag describes the descriptor this call replaces.
	o.read.dead = false
	o.read.f, o.read.refs = f, 1
	c.readOpenWhole[o.id] = o
	return true
}

// releaseWholeRead ends a hydrated read, closing a descriptor retired while it
// ran and releasing the object reference the read held.
func (c *Cache) releaseWholeRead(o *wholeObject) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o.read.refs--
	if o.read.dead && o.read.refs == 0 && o.read.f != nil {
		o.read.f.Close()
		o.read.f = nil
	}
	o.readers--
	c.releaseObjectLocked(o)
}

// retireWholeReadLocked gives up the descriptor kept for a hydrated object.
// c.mu must be held.
func (c *Cache) retireWholeReadLocked(o *wholeObject) {
	// See retireBlockReadLocked: the set entry goes either way.
	if c.readOpenWhole[o.id] == o {
		delete(c.readOpenWhole, o.id)
	}
	if o.read.f == nil {
		return
	}
	if o.read.refs > 0 {
		o.read.dead = true
		return
	}
	o.read.f.Close()
	o.read.f = nil
}

// closeColdestWholeReadLocked frees one hydrated slot. c.mu must be held.
func (c *Cache) closeColdestWholeReadLocked() bool {
	var victim *wholeObject
	for _, o := range c.readOpenWhole {
		if o.read.refs > 0 {
			continue
		}
		if victim == nil || o.lastAccess.Before(victim.lastAccess) {
			victim = o
		}
	}
	if victim == nil {
		return false
	}
	c.retireWholeReadLocked(victim)
	return true
}

// closeReadFDs retires every kept descriptor. Close calls it so nothing is
// left open on a directory the daemon — or a test — is about to remove.
func (c *Cache) closeReadFDs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, m := range c.readOpen {
		c.retireBlockReadLocked(id, m)
	}
	for _, o := range c.readOpenWhole {
		c.retireWholeReadLocked(o)
	}
}
