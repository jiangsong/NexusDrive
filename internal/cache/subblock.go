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

// DefaultSubBlockSize is the granularity at which a block may be partially
// present. A random 4 KiB read that misses fetches one sub-block, not the
// whole 4 MiB block. 16 KiB is what the kernel asks for on a random read
// (its initial read-ahead window), so a miss costs exactly one request for
// exactly the bytes the kernel wanted: 500 such reads move 8 MB, not the
// file.
const DefaultSubBlockSize = 16 << 10

// sidecarEvery is how many new sub-blocks a partial block collects before
// its sidecar is rewritten; sidecarMaxLag bounds the wait in time.
const (
	sidecarEvery  = 32
	sidecarMaxLag = 2 * time.Second
	// maxOpenPartials bounds the descriptors held for blocks being filled.
	maxOpenPartials = 256
)

// partial tracks which sub-blocks of a block are on disk. The block file is
// full-length and sparse; the bitmap says which parts of it are real.
type partial struct {
	sub   int64
	total int
	have  int
	bits  []uint64
}

func newPartial(sub, blockLen int64) *partial {
	return newPartialTotal(sub, int((blockLen+sub-1)/sub))
}

// newPartialTotal builds an empty bitmap for a block of total sub-blocks. It
// is the only place that knows how the bits are packed.
func newPartialTotal(sub int64, total int) *partial {
	return &partial{sub: sub, total: total, bits: make([]uint64, (total+63)/64)}
}

func (p *partial) has(i int) bool { return p.bits[i/64]&(1<<(uint(i)%64)) != 0 }

func (p *partial) set(i int) {
	if !p.has(i) {
		p.bits[i/64] |= 1 << (uint(i) % 64)
		p.have++
	}
}

func (p *partial) full() bool { return p.have == p.total }

// covers reports whether [off, off+n) lies entirely within present sub-blocks.
func (p *partial) covers(off, n int64) bool {
	if n <= 0 {
		return true
	}
	first, last := int(off/p.sub), int((off+n-1)/p.sub)
	if last >= p.total {
		return false
	}
	for i := first; i <= last; i++ {
		if !p.has(i) {
			return false
		}
	}
	return true
}

// encode renders the bitmap for the sidecar file.
func (p *partial) encode() string {
	buf := make([]byte, 0, len(p.bits)*8)
	for _, w := range p.bits {
		for s := 0; s < 64; s += 8 {
			buf = append(buf, byte(w>>uint(s)))
		}
	}
	return fmt.Sprintf("sub=%d total=%d bits=%s\n", p.sub, p.total, hex.EncodeToString(buf))
}

func decodePartial(s string) (*partial, error) {
	var sub int64
	var total int
	var bits string
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "sub=%d total=%d bits=%s", &sub, &total, &bits); err != nil {
		return nil, fmt.Errorf("cache: bad sidecar: %w", err)
	}
	raw, err := hex.DecodeString(bits)
	if err != nil || sub <= 0 || total <= 0 {
		return nil, errors.New("cache: bad sidecar bitmap")
	}
	p := &partial{sub: sub, total: total, bits: make([]uint64, (total+63)/64)}
	for i, b := range raw {
		if i/8 >= len(p.bits) {
			break
		}
		p.bits[i/8] |= uint64(b) << (uint(i%8) * 8)
	}
	for i := 0; i < total; i++ {
		if p.has(i) {
			p.have++
		}
	}
	return p, nil
}

func (c *Cache) sidecarPath(fileHash string, idx int64) string {
	return c.blockPath(fileHash, idx) + ".part"
}

// sweepAllClaims writes every lagging claim however recent. Shutdown uses it:
// what is on disk when the process exits is what the next start reads.
func (c *Cache) sweepAllClaims() { c.sweepClaims(0) }

// sweepClaims writes the claim of every partial block that has held more than
// it claims for at least minQuiet. Flushes leave the claim behind on purpose —
// writing it per miss is what a random-read pass would spend its local IO on —
// so this is where a block that has gone quiet catches up.
func (c *Cache) sweepClaims(minQuiet time.Duration) {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	now := c.opt.Now()
	c.mu.Lock()
	if c.lagging == 0 {
		c.mu.Unlock()
		return // nothing to reconcile: no scan of the whole cache
	}
	type staleBlock struct {
		id blockID
		m  *blockMeta
	}
	var stale []staleBlock
	for id, m := range c.blocks {
		if m.claimLagsLocked() && now.Sub(m.sidecarAt) >= minQuiet {
			stale = append(stale, staleBlock{id, m})
		}
	}
	c.mu.Unlock()
	for _, b := range stale {
		b.m.flushMu.Lock()
		c.mu.Lock()
		cur, ok := c.blocks[b.id]
		if !ok || cur != b.m || !b.m.claimLagsLocked() || now.Sub(b.m.sidecarAt) < minQuiet {
			c.mu.Unlock()
			b.m.flushMu.Unlock()
			continue
		}
		onDisk := c.onDiskLocked(b.m)
		was := b.m.claimLagsLocked()
		b.m.sidecarHave, b.m.sidecarAt = onDisk.have, c.opt.Now()
		c.noteClaimLocked(b.m, was)
		c.mu.Unlock()
		_ = c.writeSidecar(b.id.file, b.id.index, onDisk)
		b.m.flushMu.Unlock()
	}
}

// claimDue reports whether the sidecar should be rewritten: either enough
// sub-blocks have landed since it was last written, or enough time has. Both
// halves exist so a block that fills quickly and one that trickles in both
// end up claimed without a write per sub-block.
func claimDue(have, claimed int, claimedAt, now time.Time) bool {
	return have-claimed >= sidecarEvery || now.Sub(claimedAt) >= sidecarMaxLag
}

// onDiskLocked returns what the block file holds: every sub-block the meta
// has that is not still waiting in memory. c.mu must be held.
func (c *Cache) onDiskLocked(m *blockMeta) *partial {
	onDisk := newPartialTotal(m.partial.sub, m.partial.total)
	for i := 0; i < m.partial.total; i++ {
		if m.partial.has(i) {
			if _, mem := m.subs[i]; !mem {
				onDisk.set(i)
			}
		}
	}
	return onDisk
}

// noteClaimLocked keeps c.lagging in step after anything changed how much a
// block holds or how much its claim names. Callers pass what
// claimLagsLocked said before the change. c.mu must be held.
func (c *Cache) noteClaimLocked(m *blockMeta, was bool) {
	switch now := m.claimLagsLocked(); {
	case now && !was:
		c.lagging++
		c.armClaimSweepLocked()
	case !now && was:
		c.lagging--
	}
}

// armClaimSweepLocked schedules the janitor that writes the claims of blocks
// that have gone quiet. Reconciling claims with the disk is the cache's own
// duty: hanging it off the write-behind loop meant the last block flushed was
// never swept, and a cache taking only synchronous writes never swept at all.
// c.mu must be held.
func (c *Cache) armClaimSweepLocked() {
	if c.claimSweep != nil || c.wb.closed {
		return
	}
	c.claimSweep = time.AfterFunc(sidecarMaxLag, func() {
		c.sweepClaims(sidecarMaxLag)
		c.mu.Lock()
		c.claimSweep = nil
		if c.lagging > 0 {
			c.armClaimSweepLocked()
		}
		c.mu.Unlock()
	})
}

// openPartialBlock is the only way a partial block file comes into being: it
// writes the claim first, then creates and sizes the file. Reload reads a
// block file with no sidecar as a whole block, so the file must never exist
// without one, and an ordering kept per call site is one a later writer gets
// wrong. removePartialBlock is its inverse, in the inverse order.
func (c *Cache) openPartialBlock(id blockID, sub int64, total int, blockLen int64) (*os.File, error) {
	if err := c.claimBlock(id, sub, total); err != nil {
		return nil, err
	}
	p := c.blockPath(id.file, id.index)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil && os.IsNotExist(err) {
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			f, err = os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cache: open block: %w", err)
	}
	// A sparse file of the block's length keeps offsets direct.
	if st, err := f.Stat(); err == nil && st.Size() != blockLen && blockLen > 0 {
		if err := f.Truncate(blockLen); err != nil {
			f.Close()
			return nil, fmt.Errorf("cache: size block: %w", err)
		}
	}
	return f, nil
}

// removePartialBlock takes a partial block off disk, data before claim: a
// claim without its file is a miss, a file without its claim is read as a
// whole block.
func (c *Cache) removePartialBlock(id blockID) {
	os.Remove(c.blockPath(id.file, id.index))
	os.Remove(c.sidecarPath(id.file, id.index))
}

// claimBlock puts an empty sidecar next to a partial block. An empty claim
// costs a re-fetch after a crash and never a wrong byte.
func (c *Cache) claimBlock(id blockID, sub int64, total int) error {
	c.mu.Lock()
	m, ok := c.blocks[id]
	if ok && (m.sidecarOn || m.partial == nil) {
		c.mu.Unlock()
		return nil
	}
	if ok && m.partial != nil {
		sub, total = m.partial.sub, m.partial.total
	}
	c.mu.Unlock()
	if total <= 0 || sub <= 0 {
		return fmt.Errorf("cache: cannot claim block %d with %d sub-blocks of %d bytes", id.index, total, sub)
	}
	if _, err := os.Stat(c.sidecarPath(id.file, id.index)); err != nil {
		if err := os.MkdirAll(filepath.Dir(c.blockPath(id.file, id.index)), 0o700); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		empty := &partial{sub: sub, total: total, bits: make([]uint64, (total+63)/64)}
		if err := c.writeSidecar(id.file, id.index, empty); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if m, ok := c.blocks[id]; ok {
		m.sidecarOn, m.sidecarHave, m.sidecarAt = true, 0, c.opt.Now()
	}
	c.mu.Unlock()
	return nil
}

func (c *Cache) writeSidecar(fileHash string, idx int64, p *partial) error {
	path := c.sidecarPath(fileHash, idx)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.encode()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SubBlockSize is the partial-presence granularity in bytes.
func (c *Cache) SubBlockSize() int64 { return c.subSize() }

func (c *Cache) subSize() int64 {
	s := c.opt.SubBlockSize
	if s <= 0 {
		s = DefaultSubBlockSize
	}
	if s > c.opt.BlockSize || c.opt.BlockSize%s != 0 {
		return c.opt.BlockSize
	}
	return s
}

// SubRange aligns [off, off+n) within a block of length blockLen to
// sub-block boundaries, returning the aligned offset and length.
func (c *Cache) SubRange(off, n, blockLen int64) (int64, int64) {
	sub := c.subSize()
	start := off / sub * sub
	end := (off + n + sub - 1) / sub * sub
	if end > blockLen {
		end = blockLen
	}
	return start, end - start
}

// PutRange stores part of a block: data at offset off within block idx of a
// file of fileSize bytes. It must be sub-block aligned at the start and end
// unless it reaches the end of the block. Once every sub-block is present the
// block is promoted to a whole one and hydration is considered as for Put.
// PutRange stores part of a block: data at offset off within block idx of a
// file of fileSize bytes. It must be sub-block aligned at the start and end
// unless it reaches the end of the block. Once every sub-block is present the
// block is promoted to a whole one and hydration is considered as for Put.
//
// The bytes are kept in memory and written behind, like PutAsync: a random
// reader's miss must not wait for the disk, least of all a disk still
// absorbing the last sequential read. Past the memory bound, or once the
// cache is closing, the write is synchronous.
func (c *Cache) PutRange(k FileKey, idx, off int64, data []byte, fileSize int64) error {
	c.admitMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.admitMu.Unlock()
		}
	}()
	_, blockLen := c.BlockRange(idx, fileSize)
	if blockLen <= 0 {
		return fmt.Errorf("cache: block %d is outside a %d-byte file", idx, fileSize)
	}
	if off < 0 || off+int64(len(data)) > blockLen {
		return fmt.Errorf("cache: range [%d,%d) exceeds block length %d", off, off+int64(len(data)), blockLen)
	}
	sub := c.subSize()
	if off%sub != 0 || (int64(len(data))%sub != 0 && off+int64(len(data)) != blockLen) {
		return fmt.Errorf("cache: range [%d,%d) is not sub-block aligned (%d)", off, off+int64(len(data)), sub)
	}
	if len(data) == 0 {
		return nil
	}
	if err := c.blockRoom(k, idx, off, int64(len(data)), fileSize, true); err != nil {
		return err
	}
	limit := c.opt.WriteBehind
	if limit <= 0 {
		limit = writeBehindMax
	}
	fh := k.hash()
	id := blockID{fh, idx}
	c.mu.Lock()
	if c.wb.closed || c.wb.pending+int64(len(data)) > limit {
		c.mu.Unlock()
		return c.putRangeSync(k, idx, off, data, fileSize)
	}
	// Whole blocks stop short of the limit (see PutAsync) so a sequential
	// read that fills the memory never pushes a random reader's small
	// misses onto the disk it is saturating.
	m, ok := c.blocks[id]
	if ok && m.partial == nil {
		c.mu.Unlock()
		return nil // already whole; the write was redundant but harmless
	}
	if !ok {
		m = &blockMeta{lastAccess: c.opt.Now(), partial: newPartial(sub, blockLen)}
		c.blocks[id] = m
	}
	if m.subs == nil {
		m.subs = map[int][]byte{}
	}
	wasLagging := m.claimLagsLocked()
	first, last := int(off/sub), int((off+int64(len(data))-1)/sub)
	for i := first; i <= last; i++ {
		if m.partial.has(i) {
			continue
		}
		n := sub
		if int64(i+1)*sub > blockLen {
			n = blockLen - int64(i)*sub
		}
		chunk := data[int64(i-first)*sub : int64(i-first)*sub+n]
		held := append([]byte(nil), chunk...)
		m.subs[i] = held
		m.partial.set(i)
		m.size += n
		c.bytes += n
		c.wb.pending += n
	}
	c.noteClaimLocked(m, wasLagging)
	m.lastAccess = c.opt.Now()
	fs, ok := c.files[fh]
	if !ok {
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key, fs.size = k, fileSize
	fs.generation++
	remember := !fs.keyWritten
	fs.keyWritten = true
	if fs.pinned || fs.userPinned {
		m.pinned = true
	}
	c.wb.once.Do(func() {
		c.wb.queue = make(chan blockID, 4096)
		c.wb.wg.Add(writeBehindWorkers)
		for i := 0; i < writeBehindWorkers; i++ {
			go c.writeBehindWorker()
		}
	})
	queueIt := !m.subQueued
	m.subQueued = true
	c.mu.Unlock()
	if remember {
		c.rememberKey(k)
	}
	c.admitMu.Unlock()
	locked = false
	if queueIt && !c.enqueue(id) {
		// Closed under us: put the sub-blocks on disk here rather than
		// leave them in memory with no worker left to write them.
		c.flushSubs(id)
	}
	return nil
}

// readPartialLocked serves a read of a partial block whose sub-blocks may
// still be in memory. It returns done=false when some of the range is only
// on disk, in which case the caller reads the file. c.mu must be held.
func (c *Cache) readPartialLocked(m *blockMeta, off int64, dst []byte) (int, bool) {
	sub := m.partial.sub
	n := 0
	for n < len(dst) {
		pos := off + int64(n)
		i := int(pos / sub)
		b, inMem := m.subs[i]
		if !inMem {
			return 0, false
		}
		within := pos - int64(i)*sub
		if within >= int64(len(b)) {
			break
		}
		n += copy(dst[n:], b[within:])
	}
	return n, true
}

// flushSubs writes the in-memory sub-blocks of a partial block to its file,
// records the sidecar for what is now on disk, and promotes the block when
// it is whole. It runs on the write-behind workers.
func (c *Cache) flushSubs(id blockID) {
	c.admitMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.admitMu.Unlock()
		}
	}()
	c.mu.Lock()
	m, ok := c.blocks[id]
	if !ok || m.partial == nil {
		if ok {
			m.subQueued = false
		}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	c.mu.Lock()
	if cur, still := c.blocks[id]; !still || cur != m || m.partial == nil {
		if still && cur == m {
			m.subQueued = false
		}
		c.mu.Unlock()
		return
	}
	m.subQueued = false
	pending := make(map[int][]byte, len(m.subs))
	for i, b := range m.subs {
		pending[i] = b
	}
	sub, total := m.partial.sub, m.partial.total
	var blockLen int64
	var key FileKey
	if fs, ok := c.files[id.file]; ok {
		_, blockLen = c.BlockRange(id.index, fs.size)
		key = fs.key
	}
	f := m.f
	c.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	keep := false
	if f == nil {
		var err error
		if f, err = c.openPartialBlock(id, sub, total, blockLen); err != nil {
			c.dropPartial(id)
			return
		}
		keep = true
	}
	for i, b := range pending {
		if _, err := f.WriteAt(b, int64(i)*sub); err != nil {
			if keep {
				f.Close()
			}
			c.dropPartial(id)
			return
		}
	}
	c.mu.Lock()
	cur, ok := c.blocks[id]
	if !ok || cur != m || m.partial == nil {
		c.mu.Unlock()
		if keep {
			f.Close()
		}
		if !ok {
			// Evicted or forgotten while we wrote: the file must not be
			// left behind, or a reload would take a sparse partial block
			// for a whole one.
			c.removePartialBlock(id)
		}
		return
	}
	var released int64
	for i, b := range pending {
		if held, still := m.subs[i]; still && &held[0] == &b[0] {
			delete(m.subs, i)
			released += int64(len(b))
		}
	}
	if keep {
		if m.f == nil && c.openPartials < maxOpenPartials {
			m.f, c.openPartials = f, c.openPartials+1
			keep = false
		}
	}
	onDisk := c.onDiskLocked(m)
	promoted := onDisk.full()
	wasLagging := m.claimLagsLocked()
	var claim *partial
	var fs *fileState
	complete := false
	if promoted {
		m.partial = nil
		m.subs = nil
		m.sidecarOn, m.sidecarHave = false, 0
		if fs = c.files[id.file]; fs != nil {
			fs.present[id.index] = true
			complete = int64(len(fs.present)) == c.BlockCount(fs.size) && fs.size > 0 && !c.anyInMemoryLocked(id.file)
		}
		if m.f != nil {
			m.f.Close()
			m.f = nil
			c.openPartials--
		}
	} else if claimDue(onDisk.have, m.sidecarHave, m.sidecarAt, c.opt.Now()) {
		// The claim on disk is allowed to lag what the data does: it only
		// ever under-claims, so the cost of a stale one is a re-fetch.
		// Writing it per flush is what a random-read pass would pay most of
		// its local IO for.
		claim = onDisk
		m.sidecarHave, m.sidecarAt = onDisk.have, c.opt.Now()
	}
	c.noteClaimLocked(m, wasLagging)
	requeue := !promoted && len(m.subs) > 0 && !m.subQueued
	if requeue {
		m.subQueued = true
	}
	c.mu.Unlock()
	if keep {
		f.Close()
	}
	if promoted {
		os.Remove(c.sidecarPath(id.file, id.index))
	} else if claim != nil {
		_ = c.writeSidecar(id.file, id.index, claim)
	}
	// Only now is the block's bookkeeping on disk; releasing the memory
	// accounting earlier let a waiter see "flushed" before it was.
	c.mu.Lock()
	c.wb.pending -= released
	c.mu.Unlock()
	if promoted && complete {
		c.scheduleHydrate(key)
	}
	if requeue {
		c.admitMu.Unlock()
		locked = false
		c.enqueue(id)
	}
}

// dropPartial forgets a partial block whose flush failed; a later read
// fetches it again rather than reading a half-written file.
func (c *Cache) dropPartial(id blockID) {
	c.mu.Lock()
	if m, ok := c.blocks[id]; ok {
		c.bytes -= m.size
		c.dropBlockLocked(id)
	}
	c.mu.Unlock()
	// The file goes with the claim: a sparse block left behind without one
	// reloads as a whole block full of zeros.
	c.removePartialBlock(id)
}

func (c *Cache) putRangeSync(k FileKey, idx, off int64, data []byte, fileSize int64) error {
	blockStart, blockLen := c.BlockRange(idx, fileSize)
	_ = blockStart
	if blockLen <= 0 {
		return fmt.Errorf("cache: block %d is outside a %d-byte file", idx, fileSize)
	}
	if off < 0 || off+int64(len(data)) > blockLen {
		return fmt.Errorf("cache: range [%d,%d) exceeds block length %d", off, off+int64(len(data)), blockLen)
	}
	sub := c.subSize()
	if off%sub != 0 || (int64(len(data))%sub != 0 && off+int64(len(data)) != blockLen) {
		return fmt.Errorf("cache: range [%d,%d) is not sub-block aligned (%d)", off, off+int64(len(data)), sub)
	}
	if len(data) == 0 {
		return nil
	}
	if err := c.blockRoom(k, idx, off, int64(len(data)), fileSize, true); err != nil {
		return err
	}
	fh := k.hash()
	c.mu.Lock()
	km, known := c.blocks[blockID{fh, idx}]
	var f *os.File
	if known {
		f = km.f
	}
	c.mu.Unlock()
	keep := false
	if f == nil {
		var err error
		f, err = c.openPartialBlock(blockID{fh, idx}, sub, int((blockLen+sub-1)/sub), blockLen)
		if err != nil {
			return err
		}
		keep = true
	}
	if _, err := f.WriteAt(data, off); err != nil {
		if keep {
			f.Close()
		}
		return fmt.Errorf("cache: write range: %w", err)
	}

	c.mu.Lock()
	id := blockID{fh, idx}
	m, ok := c.blocks[id]
	if ok && m.partial == nil {
		// Already whole; the write was redundant but harmless.
		c.mu.Unlock()
		if keep {
			f.Close()
		}
		return nil
	}
	if !ok {
		m = &blockMeta{lastAccess: c.opt.Now(), partial: newPartial(sub, blockLen)}
		c.blocks[id] = m
	}
	if keep {
		if m.f == nil && c.openPartials < maxOpenPartials {
			m.f, c.openPartials = f, c.openPartials+1
		} else {
			f.Close()
		}
	}
	wasLagging := m.claimLagsLocked()
	first, last := int(off/sub), int((off+int64(len(data))-1)/sub)
	for i := first; i <= last; i++ {
		if !m.partial.has(i) {
			m.partial.set(i)
			n := sub
			if int64(i+1)*sub > blockLen {
				n = blockLen - int64(i)*sub
			}
			m.size += n
			c.bytes += n
		}
	}
	m.lastAccess = c.opt.Now()
	fs, ok := c.files[fh]
	if !ok {
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key, fs.size = k, fileSize
	fs.generation++
	remember := !fs.keyWritten
	fs.keyWritten = true
	if fs.pinned || fs.userPinned {
		m.pinned = true
	}
	promoted := m.partial.full()
	var claim *partial
	if promoted {
		m.partial = nil
		m.sidecarOn, m.sidecarHave = false, 0
		fs.present[idx] = true
		if m.f != nil {
			m.f.Close()
			m.f = nil
			c.openPartials--
		}
	} else if claimDue(m.partial.have, m.sidecarHave, m.sidecarAt, c.opt.Now()) {
		// The sidecar lags the data on purpose: data always lands before
		// the bookkeeping that claims it, so a crash can only lose the
		// claim, never invent one, and a claim written every few
		// sub-blocks costs a fraction of one per miss.
		claim = &partial{sub: m.partial.sub, total: m.partial.total, have: m.partial.have, bits: append([]uint64(nil), m.partial.bits...)}
		m.sidecarHave, m.sidecarAt = m.partial.have, c.opt.Now()
	}
	c.noteClaimLocked(m, wasLagging)
	// anyInMemory matters here as much as on the async path: hydration of a
	// file whose blocks are still being written fails, and the failure is
	// discarded, leaving the file in blocks until something re-queues it.
	complete := promoted && int64(len(fs.present)) == c.BlockCount(fileSize) && fileSize > 0 &&
		!c.anyInMemoryLocked(fh)
	c.mu.Unlock()
	if remember {
		c.rememberKey(k)
	}

	if promoted {
		os.Remove(c.sidecarPath(fh, idx))
		if complete {
			c.scheduleHydrate(k)
		}
		return nil
	}
	if claim == nil {
		return nil // the sidecar catches up on a later sub-block
	}
	if err := c.writeSidecar(fh, idx, claim); err != nil {
		return fmt.Errorf("cache: sidecar: %w", err)
	}
	return nil
}

// HasRange reports whether [off, off+n) of block idx can be served locally.
func (c *Cache) HasRange(k FileKey, idx, off, n int64) bool {
	_, _, ok := c.locateRange(k, idx, off, n)
	return ok
}

// locateRange is locate for a byte range: a whole or hydrated block serves
// anything; a partial block serves what its bitmap covers.
func (c *Cache) locateRange(k FileKey, idx, off, n int64) (blockLen, fileSize int64, ok bool) {
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, present := c.blocks[blockID{fh, idx}]
	var size int64
	hydrated := false
	if fs, fok := c.files[fh]; fok {
		hydrated, size = fs.hydrated, fs.size
		if fs.whole != nil {
			fs.whole.lastAccess = c.opt.Now()
			fs.whole.hot = true
		}
	}
	if present {
		if !meta.hot {
			meta.hot = true
		}
		meta.lastAccess = c.opt.Now()
		if meta.partial == nil {
			return meta.size, size, true
		}
		if !meta.partial.covers(off, n) {
			return 0, 0, false
		}
		_, bl := c.BlockRange(idx, size)
		if bl <= 0 {
			// A reload knows which sub-blocks are on disk but not how long
			// the file is until something states it. The bitmap says these
			// bytes are real, so answer with the range that was asked for
			// rather than a length of zero, which a reader takes for the
			// end of the file.
			bl = off + n
		}
		return bl, size, true
	}
	if !hydrated {
		return 0, 0, false
	}
	_, bl := c.BlockRange(idx, size)
	if bl <= 0 {
		return 0, 0, false
	}
	return bl, size, true
}
