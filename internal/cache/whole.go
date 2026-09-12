package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// All hard-linked cache names for one inode share a charge and retention state.
// An unlinked inode remains charged until its last cache-managed reader closes.
type wholeObject struct {
	id         diskIdentity
	size       int64
	refs       map[string]*fileState
	readers    int
	lastAccess time.Time
	hot        bool
}

// WholeFile is a lease on immutable cached content. Call Close, not File.Close,
// to release its budget charge. GC never evicts an object with a live lease.
type WholeFile struct {
	*os.File
	once   sync.Once
	cache  *Cache
	object *wholeObject
	err    error
}

func (f *WholeFile) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
		f.cache.mu.Lock()
		f.object.readers--
		f.cache.releaseObjectLocked(f.object)
		f.cache.mu.Unlock()
	})
	return f.err
}

// OpenWhole acquires a descriptor before GC can remove its name. Unlike the
// diagnostic HydratedPath hint it is safe to keep across invalidation and GC.
func (c *Cache) OpenWhole(k FileKey) (*WholeFile, error) {
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if fs == nil || fs.whole == nil || !fs.hydrated {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(c.hydratedPath(fh))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.detachWholeLocked(fh, fs)
		}
		return nil, err
	}
	fs.whole.readers++
	fs.whole.lastAccess, fs.whole.hot = c.opt.Now(), true
	return &WholeFile{File: f, cache: c, object: fs.whole}, nil
}

func (c *Cache) releaseObjectLocked(o *wholeObject) {
	if len(o.refs) == 0 && o.readers == 0 {
		c.wholeBytes -= o.size
		delete(c.objects, o.id)
	}
}

func (c *Cache) detachWholeLocked(fh string, fs *fileState) {
	if fs.whole == nil {
		fs.hydrated = false
		return
	}
	o := fs.whole
	delete(o.refs, fh)
	fs.whole = nil
	fs.hydrated = false
	c.releaseObjectLocked(o)
}

// attachWholeLocked runs only after the complete file has been atomically
// published. A replacement never truncates an inode that a reader may hold.
func (c *Cache) attachWholeLocked(fh string, fs *fileState, info os.FileInfo) {
	id := identity(info)
	if fs.whole != nil && fs.whole.id != id {
		c.detachWholeLocked(fh, fs)
	}
	o := c.objects[id]
	if o == nil {
		o = &wholeObject{id: id, size: info.Size(), refs: map[string]*fileState{}, lastAccess: info.ModTime()}
		c.objects[id] = o
		c.wholeBytes += o.size
	}
	o.refs[fh] = fs
	fs.whole, fs.hydrated, fs.size = o, true, info.Size()
}

func (c *Cache) objectProtectedLocked(o *wholeObject) bool {
	if o.readers > 0 {
		return true
	}
	for _, fs := range o.refs {
		if fs.pinned || fs.userPinned || fs.busy > 0 {
			return true
		}
	}
	return false
}

func (c *Cache) evictObjectLocked(o *wholeObject) bool {
	if c.objectProtectedLocked(o) {
		return false
	}
	removed := false
	for fh, fs := range o.refs {
		if err := os.Remove(c.hydratedPath(fh)); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		c.detachWholeLocked(fh, fs)
		removed = true
	}
	if removed {
		c.evictions++
	}
	return removed
}

// LinkPinnedFile installs a reference to already durable data. It may exceed
// MaxBytes: refusing a zero-allocation hard link would hide committed writes.
// The excess is fully accounted and blocks new cache admission until reclaimed.
// A cross-filesystem copy still needs the normal budget and free-space checks.
func (c *Cache) LinkPinnedFile(k FileKey, src string, size int64) error {
	err := c.installWhole(k, src, size, false, true)
	if err == nil {
		c.rememberKey(k)
	}
	return err
}

func (c *Cache) installWhole(k FileKey, src string, size int64, adopt, pinned bool) error {
	if size < 0 {
		return errors.New("cache: negative complete-file size")
	}
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("cache: complete-file source has wrong type or size")
	}
	fh := k.hash()
	// Create a unique sibling first. Never unlink/truncate an old cache entry
	// before its replacement is ready, and never consume the source on failure.
	tmp, err := os.CreateTemp(filepath.Join(c.opt.Dir, "hydrated"), ".install-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	defer c.discardTemp(name)
	linked := os.Link(src, name) == nil
	c.mu.Lock()
	known := linked && c.objects[identity(info)] != nil
	c.mu.Unlock()
	if !known && !(linked && pinned) {
		diskNeed := size
		if linked {
			diskNeed = 0
		}
		if err := c.makeRoomFor(size, 1, diskNeed, nil); err != nil {
			return err
		}
	}
	if !linked {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		err = copyWhole(out, in, size)
		if err == nil {
			err = out.Sync()
		}
		cerr := out.Close()
		in.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		info, err = os.Stat(name)
		if err != nil {
			return err
		}
	}
	// The source must remain immutable across the import.
	if info.Size() != size {
		return errors.New("cache: source changed size during import")
	}
	if err := c.installTemp(k, name, size, pinned); err != nil {
		return err
	}
	if adopt && src != c.hydratedPath(fh) {
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("cache: installed copy but could not remove source: %w", err)
		}
	}
	return nil
}

// installTemp publishes tmpPath — which must already hold exactly size bytes
// of file content, created inside the "hydrated" directory so the rename
// below is same-filesystem and atomic — as the hydrated cache entry for k.
// It is the tail installWhole and PutWhole share: stat validation, the
// publishing rename, fileState upsert, whole-object attachment, and
// dropping (and unaccounting) any block-cache entries the whole file now
// supersedes. Pinned state is preserved (or applied) rather than overwritten.
//
// The caller must hold admitMu across both its admission check and this call
// so that a concurrent Put/installWhole/Hydrate for the same key cannot
// interleave with publication; installTemp itself takes c.mu only for the
// bookkeeping below.
func (c *Cache) installTemp(k FileKey, tmpPath string, size int64, pinned bool) error {
	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("cache: install source has wrong type or size")
	}
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Rename(tmpPath, c.hydratedPath(fh)); err != nil {
		return err
	}
	fs := c.files[fh]
	if fs == nil {
		fs = &fileState{present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key = k
	fs.pinned = fs.pinned || pinned
	c.attachWholeLocked(fh, fs, info)
	fs.whole.lastAccess = c.opt.Now()
	fs.generation++
	c.removeFileBlocksLocked(fh, fs)
	return nil
}

// ErrInstallSuperseded is returned by PutWhole when the key it was
// installing was replaced, forgotten, or the cache closed while the stream
// that would have published it was still in flight (see PutWhole). The
// already-fetched content is discarded cleanly: nothing is double-charged
// and no temporary file is left behind. Unlike Hydrate's analogous early
// checks (which return nil for "not applicable, try again later"), this is
// a genuine failure to install: the caller's fetch was not published, and
// whatever the competing operation left in place is authoritative instead.
var ErrInstallSuperseded = errors.New("cache: install superseded during stream")

// PutWhole streams r — which must yield exactly size bytes — directly into
// the cache as a hydrated object for k, skipping the block-file-then-hydrate
// double write that a fetch-then-Put-per-block-then-Hydrate sequence needs.
// A short (or long) read is an error and nothing is installed.
//
// Reserving room and publishing both need admitMu, but the copy in between
// does not hold it: r is typically a network fetch, and admitMu is the one
// mutex every Put, Hydrate, installWhole and PutWhole call serializes on, so
// holding it for the whole stream would stall every other file's cache
// admission for as long as this one fetch takes. Mirroring Hydrate, the
// reservation is carried across the gap in reservedBytes/reservedEntries
// (visible to concurrent admission checks) and fs.busy protects any
// already-published whole object for this key from eviction while the
// replacement is in flight. A concurrent Put of a block for the same key
// cannot corrupt this: after the copy, PutWhole re-takes admitMu and checks
// fs is still the current, same-generation state before publishing, the
// same guard Hydrate uses; if something else (Put, Hydrate, installWhole,
// another PutWhole, or Forget) touched the key meanwhile, this returns
// ErrInstallSuperseded instead of publishing stale content over a newer
// write.
func (c *Cache) PutWhole(k FileKey, r io.Reader, size int64) error {
	if size < 0 {
		return errors.New("cache: negative complete-file size")
	}
	fh := k.hash()

	c.admitMu.Lock()
	c.mu.Lock()
	if c.wb.closed {
		c.mu.Unlock()
		c.admitMu.Unlock()
		return os.ErrClosed
	}
	fs := c.files[fh]
	if fs == nil {
		fs = &fileState{present: map[int64]bool{}}
		c.files[fh] = fs
	}
	generation := fs.generation
	fs.busy++
	c.mu.Unlock()

	err := c.makeRoomFor(size, 1, size, nil)
	if err == nil {
		c.mu.Lock()
		c.reservedBytes += size
		c.reservedEntries++
		c.mu.Unlock()
	}
	c.admitMu.Unlock()
	if err != nil {
		c.mu.Lock()
		fs.busy--
		c.mu.Unlock()
		return err
	}

	reservationHeld := true
	defer func() {
		c.mu.Lock()
		fs.busy--
		if reservationHeld {
			c.reservedBytes -= size
			c.reservedEntries--
		}
		c.mu.Unlock()
	}()

	// Same naming convention as installWhole's temporaries: reload's orphan
	// sweep recognises the ".install-" prefix and reclaims it after a crash.
	out, err := os.CreateTemp(filepath.Join(c.opt.Dir, "hydrated"), ".install-*")
	if err != nil {
		return err
	}
	name := out.Name()
	defer func() { out.Close(); c.discardTemp(name) }()

	// The stream — which may block on the network for a long time — runs
	// with neither lock held. The reservation above already accounts for
	// it, so a concurrent Put/Hydrate/installWhole/PutWhole for any other
	// key proceeds without waiting on this fetch.
	if err := copyWhole(out, r, size); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	c.mu.Lock()
	stale := c.wb.closed || c.files[fh] != fs || fs.generation != generation
	c.mu.Unlock()
	if stale {
		return ErrInstallSuperseded
	}
	if err := c.installTemp(k, name, size, false); err != nil {
		return err
	}
	// Consume the reservation atomically with publication, before another
	// admission can observe both the new object and its temporary charge.
	// admitMu is still held here (its Unlock is deferred above), so this
	// happens before any other admission decision can be made — same as
	// Hydrate's own consumption of its reservation at publish.
	c.mu.Lock()
	c.reservedBytes -= size
	c.reservedEntries--
	c.mu.Unlock()
	reservationHeld = false
	c.rememberKey(k)
	return nil
}

func (c *Cache) removeFileBlocksLocked(fh string, fs *fileState) {
	if fs.part != nil {
		// A complete file was installed for this key from elsewhere; the
		// half-written sparse one it supersedes goes with the blocks.
		c.forgetPartLocked(fs, fs.part, true)
	}
	for id, m := range c.blocks {
		if id.file != fh {
			continue
		}
		c.bytes -= m.size
		c.dropBlockLocked(id)
		os.Remove(c.blockPath(fh, id.index))
		os.Remove(c.sidecarPath(fh, id.index))
	}
	fs.present = map[int64]bool{}
}

// copyWhole copies exactly the expected size, rejecting both truncation and
// trailing data. The caller controls and removes the unpublished temporary file.
func copyWhole(dst *os.File, src io.Reader, size int64) error {
	n, err := io.CopyN(dst, src, size)
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	var b [1]byte
	if n, err := src.Read(b[:]); n != 0 || err != io.EOF {
		return errors.New("cache: source changed size while copying")
	}
	return nil
}
