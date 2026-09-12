package cache

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The sparse whole-file layout (docs/pool-v2.md §4.4).
//
// A cold read of a large file used to write the local disk twice: once as
// one file per block under blocks/, and again when the hydration janitor
// copied those blocks into a single file under hydrated/. A 256 MiB movie
// cost 512 MiB of writes and twice the disk churn for one viewing.
//
// For a file of at least Options.WholeLayoutMin bytes a fetched block is
// written straight into hydrated/<fh>.part at its real offset. The file is
// created full-length and sparse, so a block's offset in it is the block's
// offset in the file and no index is needed to find one; a per-file bitmap
// of whole blocks says which of them are real. Once every block is present
// the file is fsynced and renamed to its final hydrated name — no second
// copy, no block files for that file, and no janitor.
//
// The bitmap is persisted to hydrated/<fh>.part.bitmap in the order the
// block sidecars use (subblock.go): the data reaches the disk before the
// claim that names it, so a crash can only lose a claim, never invent one.
// A lost claim costs a re-fetch; an invented one would be read as zeros,
// which is why reload drops a sparse file whose bitmap is missing, corrupt,
// or written at another block size rather than trusting it.

// partLayoutVersion tags the on-disk bitmap. A format change makes the old
// bitmaps unreadable rather than misread.
const partLayoutVersion = 1

// defaultWholeLayoutMin is the size from which a file is cached as one
// sparse file rather than as block files. Below it the second write is a
// copy of at most this much, made once and only when the mount is idle,
// and block files keep their sub-block granularity for random reads.
const defaultWholeLayoutMin = 64 << 20

// partClaimEvery is how many whole blocks may land before the bitmap is
// rewritten. A block is hundreds of times a sub-block, so the sidecar's own
// count threshold would leave 128 MiB of fetched data unclaimed.
const partClaimEvery = 8

// wholePart is the state of one file being filled in the sparse layout.
type wholePart struct {
	fh   string
	size int64
	// bits is the presence bitmap of whole blocks: sub is the block size,
	// so bit i covers block i.
	bits *partial
	// bytes is what the real blocks occupy, and what the cache is charged
	// for the half-written file.
	bytes int64
	// f is the open sparse file. It is nil until the first read or write
	// after a reload, so a cache that starts with many abandoned sparse
	// files does not open a descriptor for each of them.
	f *os.File
	// claimed and claimedAt are what the persisted bitmap last named; it is
	// rewritten every few blocks rather than on every one.
	claimed    int
	claimedAt  time.Time
	lastAccess time.Time
	// hot is true once the file has been read as well as written, the same
	// 2Q promotion blocks and complete objects get.
	hot bool
}

// hasBlock reports whether block idx is real in the sparse file.
func (p *wholePart) hasBlock(idx int64) bool {
	return idx >= 0 && idx < int64(p.bits.total) && p.bits.has(int(idx))
}

// snapshotLocked copies the bitmap so it can be encoded without the lock.
// Cache.mu must be held.
func (p *wholePart) snapshotLocked() *partial {
	return &partial{sub: p.bits.sub, total: p.bits.total, have: p.bits.have,
		bits: append([]uint64(nil), p.bits.bits...)}
}

// wholeLayout reports whether a file of this size keeps its fetched blocks
// in one sparse file instead of one file per block. A negative
// WholeLayoutMin disables the layout; zero takes the default.
func (c *Cache) wholeLayout(size int64) bool {
	min := c.opt.WholeLayoutMin
	switch {
	case min < 0:
		return false
	case min == 0:
		min = defaultWholeLayoutMin
	}
	return size > 0 && size >= min
}

// WholeLayoutMin reports the size from which files use the sparse layout,
// or 0 when it is disabled.
func (c *Cache) WholeLayoutMin() int64 {
	if c.opt.WholeLayoutMin < 0 {
		return 0
	}
	if c.opt.WholeLayoutMin == 0 {
		return defaultWholeLayoutMin
	}
	return c.opt.WholeLayoutMin
}

func (c *Cache) partPath(fileHash string) string { return c.hydratedPath(fileHash) + ".part" }

func (c *Cache) partBitmapPath(fileHash string) string {
	return c.hydratedPath(fileHash) + ".part.bitmap"
}

// partClaimDue reports whether the bitmap should be rewritten: either enough
// blocks have landed since it was last written, or enough time has.
func partClaimDue(have, claimed int, claimedAt, now time.Time) bool {
	return have-claimed >= partClaimEvery || now.Sub(claimedAt) >= sidecarMaxLag
}

func (c *Cache) encodePartBitmap(size int64, bits *partial) string {
	return fmt.Sprintf("part=%d block=%d size=%d bits=%s\n", partLayoutVersion, bits.sub, size, bits.packed())
}

// decodePartBitmap parses a persisted bitmap, rejecting one written for
// another block size: its bits would describe the wrong ranges of the file.
func decodePartBitmap(s string, blockSize int64) (*partial, int64, error) {
	var version int
	var block, size int64
	var bits string
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "part=%d block=%d size=%d bits=%s", &version, &block, &size, &bits); err != nil {
		return nil, 0, fmt.Errorf("cache: bad whole-file bitmap: %w", err)
	}
	if version != partLayoutVersion || block != blockSize || block <= 0 || size <= 0 {
		return nil, 0, errors.New("cache: whole-file bitmap written at another granularity")
	}
	total := int((size + block - 1) / block)
	p := newPartialTotal(block, total)
	if err := unpackBits(p, bits); err != nil {
		return nil, 0, err
	}
	return p, size, nil
}

func (c *Cache) writePartBitmap(fileHash string, size int64, bits *partial) error {
	path := c.partBitmapPath(fileHash)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.encodePartBitmap(size, bits)), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ensurePart returns the sparse file for k, creating it on first use.
func (c *Cache) ensurePart(k FileKey, size int64) (*wholePart, error) {
	fh := k.hash()
	if p := c.lookupPart(fh); p != nil {
		if p.size != size {
			return nil, errors.New("cache: sparse file size changed under the cache")
		}
		return p, nil
	}
	// One creation at a time: two write-behind workers filling the same
	// file would otherwise each make one, and the loser's bytes would go
	// into a file nothing reads.
	c.partOpenMu.Lock()
	defer c.partOpenMu.Unlock()
	if p := c.lookupPart(fh); p != nil {
		return p, nil
	}
	total := c.BlockCount(size)
	if total <= 0 {
		return nil, errors.New("cache: sparse layout needs a non-empty file")
	}
	now := c.opt.Now()
	p := &wholePart{fh: fh, size: size, bits: newPartialTotal(c.opt.BlockSize, int(total)),
		claimedAt: now, lastAccess: now}
	// The empty claim goes down before the file exists. A sparse file with
	// no bitmap is dropped on reload, so it must never be the other way
	// round; an empty claim costs a re-fetch and never a wrong byte.
	if err := c.writePartBitmap(fh, size, p.bits); err != nil {
		return nil, fmt.Errorf("cache: claim sparse file: %w", err)
	}
	f, err := os.OpenFile(c.partPath(fh), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Remove(c.partBitmapPath(fh))
		return nil, fmt.Errorf("cache: open sparse file: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(c.partPath(fh))
		os.Remove(c.partBitmapPath(fh))
		return nil, fmt.Errorf("cache: size sparse file: %w", err)
	}
	p.f = f
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if fs == nil {
		fs = &fileState{present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key, fs.size, fs.part = k, size, p
	c.parts[fh] = p
	return p, nil
}

func (c *Cache) lookupPart(fileHash string) *wholePart {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parts[fileHash]
}

// partFile returns the descriptor for a sparse file, opening it if a reload
// left it closed. It fails once the file has been evicted or published.
func (c *Cache) partFile(p *wholePart) (*os.File, error) {
	c.mu.Lock()
	f, live := p.f, c.parts[p.fh] == p
	c.mu.Unlock()
	if !live {
		return nil, os.ErrNotExist
	}
	if f != nil {
		return f, nil
	}
	c.partOpenMu.Lock()
	defer c.partOpenMu.Unlock()
	c.mu.Lock()
	f, live = p.f, c.parts[p.fh] == p
	c.mu.Unlock()
	if !live {
		return nil, os.ErrNotExist
	}
	if f != nil {
		return f, nil
	}
	opened, err := os.OpenFile(c.partPath(p.fh), os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.parts[p.fh] != p {
		c.mu.Unlock()
		opened.Close()
		return nil, os.ErrNotExist
	}
	p.f = opened
	c.mu.Unlock()
	return opened, nil
}

// putPart stores one whole block of a large file directly in its sparse
// file. admitMu must be held, as for put.
func (c *Cache) putPart(k FileKey, idx int64, data []byte, fileSize int64) error {
	fh := k.hash()
	_, blockLen := c.BlockRange(idx, fileSize)
	if blockLen <= 0 {
		return fmt.Errorf("cache: block %d is outside a %d-byte file", idx, fileSize)
	}
	if int64(len(data)) != blockLen {
		// The bitmap's unit is the whole block, so a short one cannot be
		// claimed. Leaving it out costs one re-fetch; claiming it would
		// leave a hole inside a block that reads as zeros.
		return nil
	}
	fs, remember := c.holdFile(k, fileSize)
	defer c.releaseFile(fs)
	if p := c.lookupPart(fh); p != nil && p.hasBlock(idx) {
		return nil
	}
	if remember {
		c.rememberKey(k)
	}
	if err := c.partRoom(fh, blockLen); err != nil {
		return err
	}
	p, err := c.ensurePart(k, fileSize)
	if err != nil {
		return err
	}
	f, err := c.partFile(p)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, idx*c.opt.BlockSize); err != nil {
		return fmt.Errorf("cache: write sparse block: %w", err)
	}
	claim, complete := c.notePartBlock(p, k, idx, blockLen)
	if complete {
		return c.finishPartLocked(k, p)
	}
	if claim != nil {
		if err := c.persistPart(p, claim); err != nil {
			return fmt.Errorf("cache: whole-file bitmap: %w", err)
		}
	}
	return nil
}

// flushMemToPart writes a block the write-behind workers hold in memory into
// its file's sparse file and moves the block's charge across with it. The
// bytes were admitted when the block entered memory, so nothing new is
// admitted here. The caller holds no lock and has marked the block flushing.
func (c *Cache) flushMemToPart(id blockID, m *blockMeta, data []byte) {
	c.mu.Lock()
	fs := c.files[id.file]
	if fs == nil || c.blocks[id] != m || m.mem == nil {
		c.mu.Unlock()
		return
	}
	k, size := fs.key, fs.size
	c.mu.Unlock()
	_, blockLen := c.BlockRange(id.index, size)
	drop := func() {
		c.mu.Lock()
		if cur := c.blocks[id]; cur == m && cur.mem != nil {
			c.bytes -= m.size
			c.dropBlockLocked(id)
			if fs := c.files[id.file]; fs != nil {
				delete(fs.present, id.index)
			}
		}
		c.mu.Unlock()
	}
	if int64(len(data)) != blockLen {
		drop()
		return
	}
	p, err := c.ensurePart(k, size)
	if err != nil {
		drop()
		return
	}
	f, err := c.partFile(p)
	if err != nil {
		drop()
		return
	}
	if _, err := f.WriteAt(data, id.index*c.opt.BlockSize); err != nil {
		drop()
		return
	}
	c.mu.Lock()
	if cur, ok := c.blocks[id]; !ok || cur != m || m.mem == nil {
		// Evicted or replaced while we wrote: the bytes stay in the sparse
		// file unclaimed, which is indistinguishable from never written.
		c.mu.Unlock()
		return
	}
	// Release the memory before dropping the entry, so dropBlockLocked does
	// not take this for a flush abandoned mid-write.
	m.mem = nil
	c.wb.pending -= int64(len(data))
	c.bytes -= m.size
	c.dropBlockLocked(id)
	c.mu.Unlock()

	claim, complete := c.notePartBlock(p, k, id.index, blockLen)
	if complete {
		c.finishPart(k)
		return
	}
	if claim != nil {
		_ = c.persistPart(p, claim)
	}
}

// adoptBlockIntoPart moves a block that a random reader filled sub-block by
// sub-block into the sparse file, so a large file read by a mix of random
// and sequential access still completes in one place. This is the one copy
// the layout does not avoid, and it is bounded by how much of the file was
// fetched in pieces. admitMu must be held.
func (c *Cache) adoptBlockIntoPart(k FileKey, idx int64) {
	fh := k.hash()
	id := blockID{fh, idx}
	c.mu.Lock()
	fs := c.files[fh]
	m, ok := c.blocks[id]
	if fs == nil || !ok || m.partial != nil || m.mem != nil {
		c.mu.Unlock()
		return
	}
	size := fs.size
	c.mu.Unlock()
	_, blockLen := c.BlockRange(idx, size)
	if blockLen <= 0 {
		return
	}
	data, err := readInto(nil, c.blockPath(fh, idx))
	if err != nil || int64(len(data)) != blockLen {
		return
	}
	p, err := c.ensurePart(k, size)
	if err != nil {
		return
	}
	f, err := c.partFile(p)
	if err != nil {
		return
	}
	if _, err := f.WriteAt(data, idx*c.opt.BlockSize); err != nil {
		return
	}
	c.mu.Lock()
	if cur, still := c.blocks[id]; !still || cur != m || m.partial != nil || m.mem != nil {
		c.mu.Unlock()
		return
	}
	c.bytes -= m.size
	c.dropBlockLocked(id)
	c.mu.Unlock()
	os.Remove(c.blockPath(fh, idx))
	os.Remove(c.sidecarPath(fh, idx))

	claim, complete := c.notePartBlock(p, k, idx, blockLen)
	if complete {
		_ = c.finishPartLocked(k, p)
		return
	}
	if claim != nil {
		_ = c.persistPart(p, claim)
	}
}

// holdFile pins a file's state against eviction while a block of it is
// admitted and written, the way blockRoom's exclusion protects a block. It
// reports whether the file's identity still has to be recorded on disk.
func (c *Cache) holdFile(k FileKey, size int64) (*fileState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fh := k.hash()
	fs := c.files[fh]
	if fs == nil {
		fs = &fileState{present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key, fs.size = k, size
	fs.busy++
	remember := !fs.keyWritten
	fs.keyWritten = true
	return fs, remember
}

func (c *Cache) releaseFile(fs *fileState) {
	c.mu.Lock()
	fs.busy--
	c.mu.Unlock()
}

// partRoom admits one block of a sparse file: its own bytes, and one object
// slot only for the file that does not have a sparse file yet.
func (c *Cache) partRoom(fileHash string, n int64) error {
	c.mu.Lock()
	entries := 1
	if c.parts[fileHash] != nil {
		entries = 0
	}
	c.mu.Unlock()
	return c.makeRoomFor(n, entries, n, nil)
}

// notePartBlock records that block idx is now real on disk. It returns the
// bitmap to persist (nil while the claim may keep lagging) and whether every
// block of the file is present.
func (c *Cache) notePartBlock(p *wholePart, k FileKey, idx, n int64) (*partial, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parts[p.fh] != p {
		return nil, false
	}
	if !p.hasBlock(idx) {
		p.bits.set(int(idx))
		p.bytes += n
		c.partBytes += n
	}
	now := c.opt.Now()
	p.lastAccess = now
	if fs := c.files[p.fh]; fs != nil {
		fs.key, fs.size = k, p.size
		fs.present[idx] = true
		fs.generation++
	}
	if p.bits.full() {
		return nil, true
	}
	if !partClaimDue(p.bits.have, p.claimed, p.claimedAt, now) {
		return nil, false
	}
	p.claimed, p.claimedAt = p.bits.have, now
	return p.snapshotLocked(), false
}

// persistPart syncs the data the bitmap is about to claim, then writes the
// claim. The order is what makes a crash cost a re-fetch rather than a hole
// read as zeros.
func (c *Cache) persistPart(p *wholePart, claim *partial) error {
	f, err := c.partFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // evicted under us; there is nothing left to claim
		}
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return c.writePartBitmap(p.fh, p.size, claim)
}

// finishPart publishes a sparse file whose blocks are all present, taking
// admitMu for the caller.
func (c *Cache) finishPart(k FileKey) {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	c.mu.Lock()
	p := c.parts[k.hash()]
	full := p != nil && p.bits.full()
	c.mu.Unlock()
	if !full {
		return
	}
	_ = c.finishPartLocked(k, p)
}

// finishPartLocked fsyncs a complete sparse file and renames it to its
// hydrated name. No copy happens: the file the reader filled is the file the
// cache publishes. admitMu must be held, as for installTemp.
func (c *Cache) finishPartLocked(k FileKey, p *wholePart) error {
	fh := p.fh
	f, err := c.partFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != p.size {
		return errors.New("cache: sparse file has the wrong size at completion")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if c.parts[fh] != p || fs == nil || fs.part != p || !p.bits.full() {
		return nil // superseded while we synced; whatever replaced it wins
	}
	if err := os.Rename(c.partPath(fh), c.hydratedPath(fh)); err != nil {
		return err
	}
	os.Remove(c.partBitmapPath(fh))
	// The rename kept the inode, so a read already in flight on this
	// descriptor sees the same bytes; one that arrives after the close is
	// reported as a miss and served from the published file instead.
	p.f = nil
	f.Close()
	c.forgetPartLocked(fs, p, false)
	fs.key = k
	c.attachWholeLocked(fh, fs, info)
	fs.whole.lastAccess = c.opt.Now()
	fs.generation++
	c.removeFileBlocksLocked(fh, fs)
	return nil
}

// forgetPartLocked takes a sparse file out of the index and its bytes out of
// the accounting. With remove set it also unlinks the file and forgets the
// blocks it held, which is what eviction and Forget want; publication passes
// false, because there the file survives under its hydrated name.
// Cache.mu must be held.
func (c *Cache) forgetPartLocked(fs *fileState, p *wholePart, remove bool) {
	if c.parts[p.fh] == p {
		delete(c.parts, p.fh)
	}
	c.partBytes -= p.bytes
	if fs != nil && fs.part == p {
		fs.part = nil
	}
	if !remove {
		return
	}
	if fs != nil {
		for i := 0; i < p.bits.total; i++ {
			if p.bits.has(i) {
				delete(fs.present, int64(i))
			}
		}
	}
	if p.f != nil {
		p.f.Close()
		p.f = nil
	}
	os.Remove(c.partPath(p.fh))
	os.Remove(c.partBitmapPath(p.fh))
}

// partProtectedLocked reports whether a half-written sparse file may not be
// evicted. Cache.mu must be held.
func (c *Cache) partProtectedLocked(fs *fileState) bool {
	return fs != nil && (fs.pinned || fs.userPinned || fs.busy > 0)
}

// evictPartLocked drops a half-written sparse file whole: its blocks are
// only useful together, and the file is what occupies the disk.
// Cache.mu must be held.
func (c *Cache) evictPartLocked(p *wholePart) bool {
	fs := c.files[p.fh]
	if c.partProtectedLocked(fs) {
		return false
	}
	c.forgetPartLocked(fs, p, true)
	c.evictions++
	return true
}

// closeParts writes every sparse file's bitmap and closes it: what is on
// disk when the process exits is what the next start reads.
func (c *Cache) closeParts() {
	c.mu.Lock()
	parts := make([]*wholePart, 0, len(c.parts))
	for _, p := range c.parts {
		parts = append(parts, p)
	}
	c.mu.Unlock()
	for _, p := range parts {
		c.mu.Lock()
		f, claim := p.f, p.snapshotLocked()
		current := p.claimed == p.bits.have
		p.claimed, p.claimedAt, p.f = p.bits.have, c.opt.Now(), nil
		c.mu.Unlock()
		if f != nil {
			f.Sync()
			f.Close()
		}
		if !current {
			_ = c.writePartBitmap(p.fh, p.size, claim)
		}
	}
}

// reloadParts re-mounts the sparse files in the hydrated directory. A file
// whose bitmap is missing, unreadable, or written at another block size is
// removed instead of trusted: unlike a block file, whose mere existence says
// what it holds, a sparse file cannot tell a hole from a block of zeros.
func (c *Cache) reloadParts(dir string, entries []os.DirEntry) {
	kept := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		fh := strings.TrimSuffix(name, ".part")
		if fh == name || !validFileHash(fh) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if c.adoptPart(fh, info) {
			kept[fh] = true
			continue
		}
		os.Remove(filepath.Join(dir, name))
		os.Remove(c.partBitmapPath(fh))
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".part.bitmap.tmp") {
			os.Remove(filepath.Join(dir, name)) // never the authoritative claim
			continue
		}
		fh := strings.TrimSuffix(name, ".part.bitmap")
		if fh == name || !validFileHash(fh) || kept[fh] {
			continue
		}
		os.Remove(filepath.Join(dir, name)) // a claim with no file to claim
	}
}

// adoptPart validates one sparse file against its bitmap and, when they
// agree, restores the blocks it holds. It reports whether the file was kept.
func (c *Cache) adoptPart(fileHash string, info os.FileInfo) bool {
	if fs := c.files[fileHash]; fs != nil && fs.hydrated {
		return false // the complete file is already there and wins
	}
	raw, err := os.ReadFile(c.partBitmapPath(fileHash))
	if err != nil {
		return false
	}
	bits, size, err := decodePartBitmap(string(raw), c.opt.BlockSize)
	if err != nil || size != info.Size() || bits.have == 0 {
		return false
	}
	fs := c.files[fileHash]
	if fs == nil {
		fs = &fileState{present: map[int64]bool{}}
		c.files[fileHash] = fs
	}
	fs.size = size
	if bits.full() {
		// A crash between the last block and the rename. Finish it now
		// rather than leave a complete file that nothing can pass through.
		if err := os.Rename(c.partPath(fileHash), c.hydratedPath(fileHash)); err == nil {
			os.Remove(c.partBitmapPath(fileHash))
			if done, err := os.Stat(c.hydratedPath(fileHash)); err == nil {
				c.attachWholeLocked(fileHash, fs, done)
				return true
			}
		}
		return false
	}
	p := &wholePart{fh: fileHash, size: size, bits: bits, claimed: bits.have,
		claimedAt: c.opt.Now(), lastAccess: info.ModTime()}
	for i := 0; i < bits.total; i++ {
		if !bits.has(i) {
			continue
		}
		_, n := c.BlockRange(int64(i), size)
		p.bytes += n
		fs.present[int64(i)] = true
	}
	fs.part = p
	c.parts[fileHash] = p
	c.partBytes += p.bytes
	return true
}

// unpackBits fills an empty bitmap from the hex form packed produces.
func unpackBits(p *partial, bits string) error {
	raw, err := hex.DecodeString(bits)
	if err != nil {
		return errors.New("cache: bad bitmap")
	}
	for i, b := range raw {
		if i/8 >= len(p.bits) {
			break
		}
		p.bits[i/8] |= uint64(b) << (uint(i%8) * 8)
	}
	p.have = 0
	for i := 0; i < p.total; i++ {
		if p.has(i) {
			p.have++
		}
	}
	return nil
}
