// Package cache is the on-disk block cache: fixed-size blocks keyed by
// (remote, remote id, version, index), a per-file presence bitmap, 2Q
// eviction with byte/inode/age/free-space caps, and promotion of a fully
// present file to a single hydrated file (docs/DESIGN.md §4.4).
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// FileKey identifies one version of one remote file.
type FileKey struct {
	Remote   string
	RemoteID string
	Version  string
}

// String renders the key for logs and paths.
func (k FileKey) String() string { return k.Remote + "/" + k.RemoteID + "@" + k.Version }

func (k FileKey) hash() string {
	sum := sha256.Sum256([]byte(k.Remote + "\x00" + k.RemoteID + "\x00" + k.Version))
	return hex.EncodeToString(sum[:16])
}

// Options configures a Cache.
type Options struct {
	// Dir is the cache root. Blocks, hydrated files and staging live below it.
	Dir string
	// BlockSize is the fixed block size in bytes. Must be > 0.
	BlockSize int64
	// SubBlockSize is the granularity of partial blocks (0 = 64 KiB). It must
	// divide BlockSize; otherwise partial blocks are disabled.
	SubBlockSize int64
	// MaxBytes caps total cached bytes (0 = unlimited).
	MaxBytes int64
	// MaxBlocks caps cached payload objects (blocks plus unique complete files).
	MaxBlocks int
	// MaxAge evicts payload objects untouched for longer than this (0 = never).
	MaxAge time.Duration
	// MinFree keeps at least this many bytes free on the cache filesystem.
	MinFree int64
	// HydrateAfter is how long a complete file must go unread before its
	// blocks are merged into one file for passthrough (0 = 10 s). Merging
	// right at completion copied the whole file again while the reader that
	// had just fetched it was still busy on the same disk.
	HydrateAfter time.Duration
	// WholeLayoutMin is the size from which a file is cached as one sparse
	// file whose blocks are written at their real offsets, instead of one
	// file per block that hydration later copies into a whole file
	// (0 = 64 MiB, negative = never). Above it a cold read writes the file
	// to the local disk once rather than twice; below it block files keep
	// their sub-block granularity for random reads, and the copy hydration
	// makes is bounded by this size.
	WholeLayoutMin int64

	// Busy reports that the foreground is waiting on IO. Whether a given file
	// may be merged is decided per file, from how long its own blocks have
	// gone untouched (see hydrateDue); this only rate-limits how many files
	// one pass merges while the mount is under load, so a burst does not put
	// a queue of whole-file copies in front of it.
	Busy func() bool
	// WriteBehind bounds the bytes of fetched blocks held in memory while
	// the background writers put them on disk (0 = 256 MiB).
	WriteBehind int64
	// Now is injectable for tests.
	Now func() time.Time
	// FreeSpace reports free bytes on the cache filesystem; nil uses statfs.
	FreeSpace func(dir string) (int64, error)
	// Open opens a cache file on the read path; nil uses os.Open. It is the
	// seam tests count descriptor churn through.
	Open func(name string) (*os.File, error)
}

// ErrNoSpace is returned when the cache cannot admit data without violating
// MinFree. Callers degrade to uncached reads or fail writes with ENOSPC.
var ErrNoSpace = fmt.Errorf("cache: not enough free space: %w", syscall.ENOSPC)

type blockID struct {
	file  string // FileKey.hash()
	index int64
}

type blockMeta struct {
	// flushMu serialises write-behind flushes of this block: two workers
	// flushing the same partial block would race over its sidecar.
	flushMu sync.Mutex
	// subs holds sub-blocks of a partial block that are present in memory
	// but not yet on disk, by sub index; subQueued says a flush is queued.
	subs      map[int][]byte
	subQueued bool
	// mem holds the block's bytes until the write-behind worker has put
	// them on disk; reads are served from it meanwhile. The slice is never
	// reused, so a reader may keep copying from it after it is dropped.
	mem           []byte
	flushing      bool
	flushDetached bool
	// f is the block file, kept open while the block is partial: a random
	// reader hits the same block file for every sub-block it fetches, and
	// opening and closing it around each 16 KiB read and write was a third
	// of the miss path. PutRange writes through it, so it is read-write and
	// distinct from read, which serves whole blocks; see readfd.go.
	f *os.File
	// read is the read-only descriptor kept for a whole block on disk.
	read readFD
	// sidecarHave and sidecarAt record what the on-disk sidecar last said;
	// it is rewritten every few sub-blocks, not on every one.
	sidecarHave int
	sidecarAt   time.Time
	// sidecarOn says a sidecar for this block exists on disk. A partial
	// block file without one reloads as a whole block, so the claim is
	// written before the first byte of data and only then allowed to lag.
	sidecarOn  bool
	size       int64
	lastAccess time.Time
	// hot is true once the block has been hit a second time; 2Q keeps hot
	// blocks when a sequential scan floods the probation queue.
	hot    bool
	pinned bool
	// partial is non-nil while only some sub-blocks are on disk.
	partial *partial
}

// Stats reports cache counters for /metrics and `cloudfs cache stats`.
type Stats struct {
	Blocks             int
	Bytes              int64
	Hits               int64
	Misses             int64
	Evictions          int64
	HydratedFiles      int
	PinnedBlocks       int
	WholeBytes         int64
	SparseBytes        int64
	ReservedBytes      int64
	WriteReservedBytes int64
	LeasedBytes        int64
	OrphanBytes        int64
}

// HitRatio returns hits/(hits+misses), or 0 when there is no traffic yet.
func (s Stats) HitRatio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Cache stores fixed-size blocks on disk. It is safe for concurrent use.
type Cache struct {
	// maxBytes and minFree are the budget: Options.MaxBytes and
	// Options.MinFree as configured, or as SetBudget changed them since.
	// Atomics because the admission paths read them without c.mu.
	maxBytes atomic.Int64
	minFree  atomic.Int64
	// hydrateWant is the set of complete files waiting to be merged, and
	// hydrateTimer the single janitor that merges them.
	hydrateWant  map[string]hydrateWish
	hydrateTimer *time.Timer
	busyFn       atomic.Pointer[func() bool]
	// lagging counts partial blocks holding more than their sidecar claims,
	// so the sweep can return without walking the cache when there is
	// nothing to reconcile.
	lagging int
	// claimSweep is the janitor that catches those claims up.
	claimSweep *time.Timer
	wb         writeBehind
	// openPartials counts blockMeta.f descriptors currently held.
	openPartials int
	// readOpen and readOpenWhole are the blocks and hydrated objects holding a
	// kept read descriptor, bounded by maxOpenReads / maxOpenWholeReads; see
	// readfd.go.
	readOpen      map[blockID]*blockMeta
	readOpenWhole map[diskIdentity]*wholeObject
	// makeRoom housekeeping throttles; see makeRoom.
	lastExpire       time.Time
	lastFreeCheck    time.Time
	bytesAtFreeCheck int64
	// ReserveDisk's own free-space throttle; see ReserveDisk. It is separate
	// from the three above because journal reservations name their own staging
	// directory, which need not be the cache filesystem.
	diskCheckAt    time.Time
	diskCheckDir   string
	diskFreeAt     int64
	diskSinceCheck int64
	opt            Options

	mu        sync.Mutex
	blocks    map[blockID]*blockMeta
	bytes     int64
	files     map[string]*fileState // FileKey.hash() -> presence
	hits      int64
	misses    int64
	evictions int64
	// Admission serialises capacity checks and publication, not cache reads.
	admitMu    sync.Mutex
	objects    map[diskIdentity]*wholeObject
	wholeBytes int64
	// parts holds the files being filled in the sparse whole-file layout
	// (sparse.go), and partBytes what their real blocks occupy.
	parts                map[string]*wholePart
	partBytes            int64
	partOpenMu           sync.Mutex
	reservedBytes        int64
	writeReserved        int64 // in-flight journal writes, not cache payload quota
	detachedFlushBytes   int64
	detachedFlushEntries int
	reservedEntries      int
	hydrateWG            sync.WaitGroup
	orphanTemps          map[string]int64
	orphanBytes          int64
}

type fileState struct {
	key FileKey
	// keyWritten says the identity is already recorded on disk, so the
	// write happens once per file and never under the cache lock.
	keyWritten bool
	size       int64
	present    map[int64]bool
	hydrated   bool
	pinned     bool
	// userPinned is independent of temporary protection for pending writes.
	userPinned bool
	whole      *wholeObject
	// part is the sparse whole file this file is being filled into, for a
	// file at or above Options.WholeLayoutMin that is not complete yet.
	part       *wholePart
	busy       int
	generation uint64
}

// New creates the cache directories and loads any existing index from disk.
func New(opt Options) (*Cache, error) {
	if opt.BlockSize <= 0 {
		return nil, errors.New("cache: BlockSize must be positive")
	}
	if opt.Dir == "" {
		return nil, errors.New("cache: Dir must be set")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.FreeSpace == nil {
		opt.FreeSpace = FreeSpace
	}
	for _, d := range []string{opt.Dir, filepath.Join(opt.Dir, "blocks"), filepath.Join(opt.Dir, "hydrated"), filepath.Join(opt.Dir, "staging")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
	}
	c := &Cache{opt: opt, blocks: map[blockID]*blockMeta{}, files: map[string]*fileState{}, objects: map[diskIdentity]*wholeObject{}, parts: map[string]*wholePart{}, orphanTemps: map[string]int64{}}
	c.SetBudget(opt.MaxBytes, opt.MinFree)
	if opt.Busy != nil {
		c.SetBusy(opt.Busy)
	}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// BlockSize returns the configured block size.
func (c *Cache) BlockSize() int64 { return c.opt.BlockSize }

// Dir returns the cache root.
func (c *Cache) Dir() string { return c.opt.Dir }

// Budget reports the byte cap and the disk headroom the cache keeps.
func (c *Cache) Budget() (maxBytes, minFree int64) { return c.maxBytes.Load(), c.minFree.Load() }

// SetBudget changes the byte cap (0 = unlimited) and the disk headroom (0 =
// none) for every admission from now on. Nothing is evicted here: the next
// admission makes room against the new figures, and a lower headroom lets
// writes a full disk was refusing through at once.
func (c *Cache) SetBudget(maxBytes, minFree int64) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if minFree < 0 {
		minFree = 0
	}
	c.maxBytes.Store(maxBytes)
	if c.minFree.Swap(minFree) != minFree {
		// ReserveDisk's remembered free-space reading was judged against the
		// old headroom; a new one has to be measured, not inferred.
		c.mu.Lock()
		c.diskCheckAt, c.diskCheckDir = time.Time{}, ""
		c.mu.Unlock()
	}
}

// StagingDir returns the directory for in-progress writes.
func (c *Cache) StagingDir() string { return filepath.Join(c.opt.Dir, "staging") }

// reload rebuilds the in-memory index by walking the blocks directory, so a
// restart keeps the cache instead of refetching everything.
func (c *Cache) reload() error {
	blocksDir := filepath.Join(c.opt.Dir, "blocks")
	err := filepath.WalkDir(blocksDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished mid-walk
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if cacheTempName(d.Name()) {
			c.orphanTemps[p] = info.Size()
			c.orphanBytes += info.Size()
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil // handled with its block below
		}
		if strings.HasSuffix(d.Name(), ".key") {
			if k, ok := readKey(p); ok && strings.TrimSuffix(d.Name(), ".key") == k.hash() {
				fh := strings.TrimSuffix(d.Name(), ".key")
				fs, ok := c.files[fh]
				if !ok {
					fs = &fileState{present: map[int64]bool{}}
					c.files[fh] = fs
				}
				fs.key = k
			}
			return nil
		}
		fileHash, idx, ok := parseBlockName(d.Name())
		if !ok {
			return nil
		}
		id := blockID{file: fileHash, index: idx}
		fs, ok := c.files[fileHash]
		if !ok {
			fs = &fileState{present: map[int64]bool{}}
			c.files[fileHash] = fs
		}
		if raw, err := os.ReadFile(p + ".part"); err == nil {
			// A partial block: the sidecar says which parts are real.
			pt, err := decodePartial(string(raw))
			if err != nil || pt.sub != c.subSize() {
				// Unreadable, or written at another granularity: the bitmap
				// would describe the wrong ranges. Drop it; it is a cache.
				os.Remove(p)
				os.Remove(p + ".part")
				return nil
			}
			size := int64(pt.have) * pt.sub
			c.blocks[id] = &blockMeta{size: size, lastAccess: info.ModTime(), partial: pt,
				sidecarOn: true, sidecarHave: pt.have, sidecarAt: c.opt.Now()}
			c.bytes += size
			return nil
		}
		c.blocks[id] = &blockMeta{size: info.Size(), lastAccess: info.ModTime()}
		c.bytes += info.Size()
		fs.present[idx] = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: reload: %w", err)
	}
	hydratedDir := filepath.Join(c.opt.Dir, "hydrated")
	entries, err := os.ReadDir(hydratedDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cache: reload hydrated: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if cacheTempName(e.Name()) {
			p := filepath.Join(hydratedDir, e.Name())
			c.orphanTemps[p] = info.Size()
			c.orphanBytes += info.Size()
			continue
		}
		if !validFileHash(e.Name()) {
			continue
		}
		fs, ok := c.files[e.Name()]
		if !ok {
			fs = &fileState{present: map[int64]bool{}}
			c.files[e.Name()] = fs
		}
		c.attachWholeLocked(e.Name(), fs, info)
	}
	c.reloadParts(hydratedDir, entries)
	return nil
}

func parseBlockName(name string) (string, int64, bool) {
	i := strings.LastIndexByte(name, '-')
	if i != 32 || i == len(name)-1 || !validFileHash(name[:i]) {
		return "", 0, false
	}
	idx, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return name[:i], idx, true
}

func (c *Cache) blockPath(fileHash string, idx int64) string {
	return filepath.Join(c.opt.Dir, "blocks", fileHash[:2], fileHash[2:4], fmt.Sprintf("%s-%d", fileHash, idx))
}

// keyPath is where a file's identity is recorded next to its blocks. A
// restart can hash a key into a directory name but not a directory name back
// into a key, and without the key the cache cannot say which file a block
// belongs to — so `cloudfs bench --cold` would drop nothing and measure a
// warm cache, and `cache drop` would report files it did not touch.
func (c *Cache) keyPath(fileHash string) string {
	return filepath.Join(c.opt.Dir, "blocks", fileHash[:2], fileHash[2:4], fileHash+".key")
}

// rememberKey records a file's identity beside its blocks. Callers decide
// under the lock whether it is needed and call this after releasing it.
func (c *Cache) rememberKey(k FileKey) {
	fh := k.hash()
	p := c.keyPath(fh)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	body := strings.Join([]string{k.Remote, k.RemoteID, k.Version}, "\n")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
	}
}

// readKey restores a file's identity written by rememberKey.
func readKey(path string) (FileKey, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return FileKey{}, false
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) != 3 || parts[1] == "" {
		return FileKey{}, false
	}
	return FileKey{Remote: parts[0], RemoteID: parts[1], Version: parts[2]}, true
}

func (c *Cache) hydratedPath(fileHash string) string {
	return filepath.Join(c.opt.Dir, "hydrated", fileHash)
}

// BlockIndex returns the block index containing offset.
func (c *Cache) BlockIndex(off int64) int64 { return off / c.opt.BlockSize }

// BlockRange returns the byte range covered by block idx of a file of the
// given size.
func (c *Cache) BlockRange(idx, size int64) (off, n int64) {
	off = idx * c.opt.BlockSize
	n = c.opt.BlockSize
	if off+n > size {
		n = size - off
	}
	if n < 0 {
		n = 0
	}
	return off, n
}

// BlockCount returns how many blocks a file of the given size occupies.
func (c *Cache) BlockCount(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return (size-1)/c.opt.BlockSize + 1
}

// Get returns the cached bytes of one block, or false when absent.
func (c *Cache) Get(k FileKey, idx int64) ([]byte, bool) {
	n, size, ok := c.locate(k, idx)
	if !ok {
		c.countMiss()
		return nil, false
	}
	buf := make([]byte, n)
	read, err := c.readBlock(k, idx, 0, buf, size)
	if err != nil {
		c.countMiss()
		return nil, false
	}
	c.countHit()
	return buf[:read], true
}

// ReadAt copies at most len(dst) bytes starting at off within block idx.
//
// It exists because Get materialises a whole block: a 4 KiB read of a cached
// file would otherwise read and allocate four megabytes, so the cost of a read
// would be set by the block size rather than by how much was asked for. That
// turns a warm sequential scan into the slowest thing the filesystem does.
func (c *Cache) ReadAt(k FileKey, idx, off int64, dst []byte) (int, bool) {
	n, size, ok := c.locateRange(k, idx, off, int64(len(dst)))
	if !ok {
		c.countMiss()
		return 0, false
	}
	if off >= n {
		// Past the end of this block, which is not a cache miss.
		c.countHit()
		return 0, true
	}
	if int64(len(dst)) > n-off {
		dst = dst[:n-off]
	}
	read, err := c.readBlock(k, idx, off, dst, size)
	if err != nil {
		c.countMiss()
		return 0, false
	}
	c.countHit()
	return read, true
}

// Has reports whether a block can be served without a provider request. It
// does no IO, so callers testing presence do not pay for a full block read.
func (c *Cache) Has(k FileKey, idx int64) bool {
	_, _, ok := c.locate(k, idx)
	return ok
}

// locate resolves a block to its length and the file size, marking it used.
// It reports whether the block can be served locally, either as its own file
// or from within a hydrated one.
func (c *Cache) locate(k FileKey, idx int64) (n, fileSize int64, ok bool) {
	fh := k.hash()
	now := c.opt.Now() // outside the lock: it is a vdso call, not free
	c.mu.Lock()
	meta, present := c.blocks[blockID{fh, idx}]
	whole := false
	var blockSize int64
	if present {
		if !meta.hot {
			meta.hot = true // second sighting promotes out of probation
		}
		meta.lastAccess = now
		// Read under the lock: a write-behind flush promotes the block
		// (clearing partial) concurrently with lookups.
		whole, blockSize = meta.partial == nil, meta.size
	}
	hydrated := false
	var size int64
	if fs, fok := c.files[fh]; fok {
		hydrated, size = fs.hydrated, fs.size
		if fs.whole != nil {
			fs.whole.lastAccess = now
			fs.whole.hot = true
		}
		if fs.part != nil && fs.part.hasBlock(idx) {
			// Present at its real offset inside the half-written sparse
			// file, which is served exactly like a hydrated one.
			hydrated = true
			fs.part.lastAccess, fs.part.hot = now, true
		}
	}
	c.mu.Unlock()

	if whole {
		return blockSize, size, true
	}
	if !hydrated {
		return 0, 0, false
	}
	_, blockLen := c.BlockRange(idx, size)
	if blockLen <= 0 {
		return 0, 0, false
	}
	return blockLen, size, true
}

// readBlock fills dst from block idx starting at off, from the block file when
// it exists and otherwise from the hydrated file.
func (c *Cache) readBlock(k FileKey, idx, off int64, dst []byte, fileSize int64) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	fh := k.hash()
	c.mu.Lock()
	var part *wholePart
	if fs := c.files[fh]; fs != nil && fs.part != nil && fs.part.hasBlock(idx) {
		// The sparse file holds the whole block; a block file for the same
		// index can only be an older partial one, so it is not consulted.
		part = fs.part
	}
	meta, present := c.blocks[blockID{fh, idx}]
	var open *os.File
	var mem []byte
	if present && part == nil {
		open, mem = meta.f, meta.mem
		if meta.partial != nil && len(meta.subs) > 0 {
			pieces := c.partialPiecesLocked(meta, off, dst)
			c.mu.Unlock()
			return c.readPieces(open, c.blockPath(fh, idx), off, dst, pieces)
		}
	}
	c.mu.Unlock()
	if part != nil {
		base, _ := c.BlockRange(idx, fileSize)
		f, err := c.partFile(part)
		if err != nil {
			return 0, err
		}
		n, err := f.ReadAt(dst, base+off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return n, nil
	}
	if mem != nil {
		// Not on disk yet: serve the copy the write-behind worker holds.
		if off >= int64(len(mem)) {
			return 0, nil
		}
		return copy(dst, mem[off:]), nil
	}
	if open != nil {
		// A partial block being filled: read through the descriptor
		// PutRange keeps open. A close racing this read makes ReadAt fail
		// with os.ErrClosed, which is reported as a miss like any error.
		n, err := open.ReadAt(dst, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return n, nil
	}

	path := c.blockPath(fh, idx)
	at := off
	if !present {
		// Blocks merged into the hydrated file are addressed by their offset
		// in the whole file.
		base, _ := c.BlockRange(idx, fileSize)
		n, err := c.readWhole(k, base+off, dst)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return n, nil
	}
	// A whole block on disk, read through the descriptor kept for it; see
	// readfd.go for why keeping one is safe and where it is given up.
	id := blockID{fh, idx}
	if f := c.borrowBlockRead(id, meta); f != nil {
		n, err := f.ReadAt(dst, at)
		c.releaseBlockRead(meta)
		if err != nil && !errors.Is(err, io.EOF) {
			c.forget(fh, idx, present)
			return 0, err
		}
		return n, nil
	}
	f, err := c.open(path)
	if err != nil {
		c.forget(fh, idx, present)
		return 0, err
	}
	kept := c.keepBlockRead(id, meta, f)
	n, err := f.ReadAt(dst, at)
	if kept {
		c.releaseBlockRead(meta)
	} else {
		f.Close()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		c.forget(fh, idx, present)
		return 0, err
	}
	return n, nil
}

// forget drops the index entry for a block whose file vanished underneath us.
func (c *Cache) forget(fh string, idx int64, wasPresent bool) {
	if !wasPresent {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, still := c.blocks[blockID{fh, idx}]; still {
		c.bytes -= m.size
		c.dropBlockLocked(blockID{fh, idx})
		if fs, fok := c.files[fh]; fok {
			delete(fs.present, idx)
		}
	}
}

func (c *Cache) countHit()  { c.mu.Lock(); c.hits++; c.mu.Unlock() }
func (c *Cache) countMiss() { c.mu.Lock(); c.misses++; c.mu.Unlock() }

// Put stores one block. fileSize is the full file size, used for the presence
// bitmap and hydration. It returns ErrNoSpace when admitting the block would
// break MinFree and eviction cannot recover enough room.
func (c *Cache) Put(k FileKey, idx int64, data []byte, fileSize int64) error {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	return c.put(k, idx, data, fileSize)
}

func (c *Cache) put(k FileKey, idx int64, data []byte, fileSize int64) error {
	if int64(len(data)) > c.opt.BlockSize {
		return fmt.Errorf("cache: block of %d bytes exceeds block size %d", len(data), c.opt.BlockSize)
	}
	if c.wholeLayout(fileSize) {
		return c.putPart(k, idx, data, fileSize)
	}
	fh := k.hash()
	if err := c.blockRoom(k, idx, 0, int64(len(data)), fileSize, false); err != nil {
		return err
	}
	p := c.blockPath(fh, idx)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		c.discardTemp(tmp)
		return fmt.Errorf("cache: write block: %w", err)
	}
	c.mu.Lock()
	if err := os.Rename(tmp, p); err != nil {
		c.mu.Unlock()
		c.discardTemp(tmp)
		return fmt.Errorf("cache: commit block: %w", err)
	}
	id := blockID{fh, idx}
	hadPartial := false
	if old, ok := c.blocks[id]; ok {
		c.bytes -= old.size
		hadPartial = old.partial != nil
		// Replacing the entry is not enough: the old one may hold an open
		// descriptor, memory counted against write-behind, and a lagging
		// claim, and overwriting the map entry loses all three. A whole
		// block landing on top of a partial one is the ordinary promotion
		// path, so this leaks on every one of them.
		c.dropBlockLocked(id)
	}
	c.blocks[id] = &blockMeta{size: int64(len(data)), lastAccess: c.opt.Now()}
	c.bytes += int64(len(data))
	fs, ok := c.files[fh]
	if !ok {
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key = k
	fs.size = fileSize
	fs.generation++
	fs.present[idx] = true
	remember := !fs.keyWritten
	fs.keyWritten = true
	if fs.pinned || fs.userPinned {
		c.blocks[id].pinned = true
	}
	complete := int64(len(fs.present)) == c.BlockCount(fileSize) && fileSize > 0
	c.mu.Unlock()

	if remember {
		c.rememberKey(k)
	}
	if hadPartial {
		os.Remove(c.sidecarPath(fh, idx))
	}
	if complete {
		// Hydration failure is not fatal: the blocks still serve reads.
		c.scheduleHydrate(k)
	}
	return nil
}

// defaultHydrateAfter is how long a complete file must go untouched before its
// blocks are merged. Merging is a whole-file copy, so it is worth doing only
// once that file has really gone quiet, and a couple of seconds is not quiet:
// a build, a directory walk, or a benchmark phase all pause for longer than
// that between reads, and the copy then lands on top of the next burst.
const defaultHydrateAfter = 10 * time.Second

// hydrateWhileBusy caps how many files one pass merges while the mount has
// foreground IO outstanding. The per-file quiet window (hydrateStateLocked)
// already says the copy interrupts no reader of the file it merges, so the cap
// is about disk bandwidth and nothing else: a burst can leave dozens of files
// individually idle at the same moment, and copying all of them at once is the
// storm the old mount-wide check prevented by never merging at all. A quiet
// mount drains the whole queue in one pass as before.
const hydrateWhileBusy = 4

// hydrateDelay is HydrateAfter, or the default when it is unset.
func (c *Cache) hydrateDelay() time.Duration {
	if d := c.opt.HydrateAfter; d > 0 {
		return d
	}
	return defaultHydrateAfter
}

// hydrateWish is a file waiting to be merged, and the moment it started
// waiting. Anything that touches the file after that restarts its wait, which
// is how "untouched for HydrateAfter" is enforced without a second clock: the
// janitor's timer supplies the interval, this stamp supplies the "untouched".
type hydrateWish struct {
	key   FileKey
	since time.Time
}

// scheduleHydrate puts a complete file in line to have its blocks merged
// once that file has gone untouched for HydrateAfter.
//
// One timer serves the whole queue rather than one per file: a file that keeps
// being read defers its own hydration indefinitely, and a timer per complete
// file re-arming forever is thousands of wakeups a second on a large cache,
// all of them taking the lock the read path needs.
func (c *Cache) scheduleHydrate(k FileKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hydrateWant == nil {
		c.hydrateWant = map[string]hydrateWish{}
	}
	c.hydrateWant[k.hash()] = hydrateWish{key: k, since: c.opt.Now()}
	c.armHydrateLocked()
}

// armHydrateLocked starts the hydration janitor if it is not already running.
// c.mu must be held.
func (c *Cache) armHydrateLocked() {
	if c.hydrateTimer != nil || c.wb.closed || len(c.hydrateWant) == 0 {
		return
	}
	c.hydrateTimer = time.AfterFunc(c.hydrateDelay(), c.hydrateDue)
}

// hydrateDue merges the queued files that have gone quiet, and leaves the rest
// queued for the next pass.
//
// Readiness is asked of each file, not of the mount. Waiting for the whole
// mount to fall idle sounds right and is not: a burst of reads keeps the
// foreground counter above zero continuously for far longer than HydrateAfter,
// so nothing merged until the burst was over, and every read in it kept paying
// the block layout's cost — one open per block, and no passthrough file for
// the kernel. A file nothing has touched since it was queued is one this copy
// cannot land on top of, whatever the rest of the mount is doing. The
// mount-wide signal survives only as the rate limit hydrateWhileBusy.
//
// A file that can never be merged — incomplete again after an eviction, or
// already hydrated — leaves the queue, so the janitor does not re-arm for it
// forever.
func (c *Cache) hydrateDue() {
	c.mu.Lock()
	c.hydrateTimer = nil
	var want []FileKey
	waiting := map[string]hydrateWish{}
	for fh, w := range c.hydrateWant {
		switch ready, keep := c.hydrateStateLocked(fh, w.since); {
		case ready:
			want = append(want, w.key)
		case keep:
			// Its wait restarts: whatever touched it has to stop for a whole
			// interval before the file is merged.
			waiting[fh] = hydrateWish{key: w.key, since: c.opt.Now()}
		}
	}
	c.hydrateWant = waiting
	c.armHydrateLocked()
	c.mu.Unlock()

	for i, k := range want {
		if i >= hydrateWhileBusy && c.busy() {
			// Idle in itself, but the mount is not: the rest go in the next
			// pass rather than queueing the disk behind a burst.
			c.scheduleHydrate(k)
			continue
		}
		_ = c.Hydrate(k)
	}
}

// hydrateStateLocked reports whether a queued file can be merged now, and
// whether it is worth keeping in the queue if not. since is when the file
// started waiting. c.mu must be held.
//
// Idleness is read from the blocks' own lastAccess, which both reads (locate,
// locateRange) and writes (put, PutRange) already maintain, rather than from a
// per-file timestamp some future read path could forget to update. The walk
// covers this file's blocks, not the cache's.
func (c *Cache) hydrateStateLocked(fh string, since time.Time) (ready, keep bool) {
	fs := c.files[fh]
	if fs == nil || fs.hydrated || fs.part != nil {
		// Merged already, or in the sparse layout, which publishes itself.
		return false, false
	}
	total := c.BlockCount(fs.size)
	if total == 0 || int64(len(fs.present)) != total {
		// Evicted back to incomplete: nothing will make this file mergeable
		// again except a fresh fill, which queues it anew.
		return false, false
	}
	if fs.busy > 0 {
		return false, true
	}
	for idx := range fs.present {
		m := c.blocks[blockID{fh, idx}]
		if m == nil {
			return false, false
		}
		if m.mem != nil || m.partial != nil {
			// Not all on disk yet; Hydrate reads the block files.
			return false, true
		}
		if m.lastAccess.After(since) {
			return false, true
		}
	}
	return true, false
}

// readSince reports whether any block of a file has been touched since t.
// locate and locateRange stamp lastAccess on every read, so this is how the
// cache sees a reader arrive for a file it is in the middle of merging.
func (c *Cache) readSince(fh string, t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if fs == nil {
		return false
	}
	for idx := range fs.present {
		if m := c.blocks[blockID{fh, idx}]; m != nil && m.lastAccess.After(t) {
			return true
		}
	}
	return false
}

// busy reports whether the foreground is waiting on IO.
func (c *Cache) busy() bool {
	f := c.busyFn.Load()
	return f != nil && *f != nil && (*f)()
}

// SetBusy installs the foreground-activity hook after construction, which is
// where the daemon learns it: the cache is built before the filesystem that
// reads from it.
func (c *Cache) SetBusy(f func() bool) { c.busyFn.Store(&f) }

// cancelHydrateLocked drops a pending hydration; c.mu must be held.
func (c *Cache) cancelHydrateLocked(fh string) {
	delete(c.hydrateWant, fh)
}

// Present reports which blocks of a file are cached.
func (c *Cache) Present(k FileKey) (n int64, total int64) {
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	fs, ok := c.files[fh]
	if !ok {
		return 0, 0
	}
	if fs.hydrated {
		return c.BlockCount(fs.size), c.BlockCount(fs.size)
	}
	return int64(len(fs.present)), c.BlockCount(fs.size)
}

// Complete reports whether every block of the file is cached.
func (c *Cache) Complete(k FileKey) bool {
	have, total := c.Present(k)
	return total > 0 && have == total
}

// Hydrate merges all blocks of a complete file into one file and drops the
// individual blocks. The hydrated file is what FUSE passthrough hands to the
// kernel.
func (c *Cache) Hydrate(k FileKey) error {
	fh := k.hash()
	c.admitMu.Lock()
	c.mu.Lock()
	fs := c.files[fh]
	// A file in the sparse layout publishes itself by renaming its own
	// file; there are no block files for hydration to merge.
	if c.wb.closed || fs == nil || fs.hydrated || fs.busy > 0 || fs.part != nil {
		c.mu.Unlock()
		c.admitMu.Unlock()
		return nil
	}
	size, total, generation := fs.size, c.BlockCount(fs.size), fs.generation
	if c.anyInMemoryLocked(fh) || total == 0 || int64(len(fs.present)) != total {
		c.mu.Unlock()
		c.admitMu.Unlock()
		return errors.New("cache: hydrate on incomplete or unflushed content")
	}
	fs.busy++
	c.hydrateWG.Add(1)
	startedAt := c.opt.Now()
	c.mu.Unlock()
	// During conversion both representations exist. Reserve the temporary
	// copy, and protect its source blocks before choosing eviction victims.
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
		c.hydrateWG.Done()
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
		c.hydrateWG.Done()
	}()
	out, err := os.CreateTemp(filepath.Join(c.opt.Dir, "hydrated"), ".hydrate-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer func() { out.Close(); c.discardTemp(tmp) }()
	buf := make([]byte, 0, c.opt.BlockSize)
	for idx := int64(0); idx < total; idx++ {
		c.mu.Lock()
		closed := c.wb.closed
		c.mu.Unlock()
		if closed {
			return errors.New("cache: closed during hydration")
		}
		if c.readSince(fh, startedAt) {
			// A reader arrived for this file after the copy began; finishing
			// would put a whole-file write in front of it. This asks about
			// the file being merged rather than about the mount, for the
			// reason hydrateDue gives: during a burst the mount is never idle,
			// and yielding to that meant the copy never finished at all.
			c.scheduleHydrate(k)
			return nil
		}
		b, err := readInto(buf, c.blockPath(fh, idx))
		if err != nil {
			return fmt.Errorf("cache: hydrate block %d: %w", idx, err)
		}
		_, want := c.BlockRange(idx, size)
		if int64(len(b)) != want {
			return errors.New("cache: hydrate source block has wrong size")
		}
		if _, err := out.Write(b); err != nil {
			return err
		}
		buf = b[:0]
	}
	if err := out.Sync(); err != nil {
		return err
	}
	info, err := out.Stat()
	if err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wb.closed || c.files[fh] != fs || fs.generation != generation {
		return errors.New("cache: content invalidated during hydration")
	}
	if err := os.Rename(tmp, c.hydratedPath(fh)); err != nil {
		return err
	}
	c.attachWholeLocked(fh, fs, info)
	fs.whole.lastAccess = c.opt.Now()
	c.removeFileBlocksLocked(fh, fs)
	// Consume the reservation atomically with publication, before another
	// admission can observe both the new object and its temporary charge.
	c.reservedBytes -= size
	c.reservedEntries--
	reservationHeld = false
	return nil
}

// HydratedPath returns the path of the merged file and whether it exists.
// FUSE uses it for passthrough; MCP uses it to serve whole-file reads.
func (c *Cache) HydratedPath(k FileKey) (string, bool) {
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[fh]
	if fs == nil || !fs.hydrated || fs.whole == nil {
		return "", false
	}
	p := c.hydratedPath(fh)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.detachWholeLocked(fh, fs)
		}
		return "", false
	}
	fs.whole.lastAccess = c.opt.Now()
	return p, true
}

// AdoptFile installs an existing local file as the hydrated cache entry for k,
// moving it into the cache. The upload path uses it so a file that was just
// written is readable without downloading it back.
func (c *Cache) AdoptFile(k FileKey, localPath string, size int64) error {
	err := c.installWhole(k, localPath, size, true, false)
	if err == nil {
		c.rememberKey(k)
	}
	return err
}

// LinkFile installs localPath as the hydrated cache entry for k while leaving
// the original in place, using a hard link when the filesystem allows it. The
// write path uses it so a just-written file is readable from the cache while
// the upload queue still needs the blob.
func (c *Cache) LinkFile(k FileKey, localPath string, size int64) error {
	err := c.installWhole(k, localPath, size, false, false)
	if err == nil {
		c.rememberKey(k)
	}
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Pin marks a file's blocks exempt from eviction.
func (c *Cache) Pin(k FileKey, pinned bool) {
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	fs, ok := c.files[fh]
	if !ok {
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.pinned = pinned
	for id, m := range c.blocks {
		if id.file == fh {
			m.pinned = pinned || fs.userPinned
		}
	}
}

// Forget drops every cached block and the hydrated file for k. Used when a

// IsPinned reports whether a file is exempt from eviction.
func (c *Cache) IsPinned(k FileKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs, ok := c.files[k.hash()]
	return ok && (fs.pinned || fs.userPinned)
}

// SetUserPin applies the VFS's persistent path policy without changing the
// temporary pins that protect data waiting for upload. Call it before filling
// a file, so admitting later blocks cannot evict its earlier blocks.
func (c *Cache) SetUserPin(k FileKey, pinned bool) {
	fh := k.hash()
	c.mu.Lock()
	remember := false
	defer func() {
		c.mu.Unlock()
		if remember {
			c.rememberKey(k)
		}
	}()
	fs, ok := c.files[fh]
	if !ok {
		if !pinned {
			return
		}
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key = k
	remember = !fs.keyWritten
	fs.keyWritten = true
	if fs.userPinned == pinned {
		return
	}
	fs.userPinned = pinned
	for id, m := range c.blocks {
		if id.file == fh {
			m.pinned = fs.pinned || pinned
		}
	}
}

// file's version changes or is deleted.
func (c *Cache) Forget(k FileKey) {
	_ = c.forgetFile(k, false)
}

// ForgetChecked is for explicit durable cleanup. Failed unlinks/syncs are
// reported so the caller can retain its cleanup intent and retry. Other hard
// links and open WholeFile leases remain valid and continue to be accounted.
func (c *Cache) ForgetChecked(k FileKey) error {
	return c.forgetFile(k, true)
}

func (c *Cache) forgetFile(k FileKey, durable bool) error {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	fh := k.hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelHydrateLocked(fh)
	fs := c.files[fh]
	remove := func(p string) error {
		if info, err := os.Lstat(p); err == nil && info.IsDir() {
			return fmt.Errorf("cache: cleanup target is a directory")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if fs != nil {
		for id, m := range c.blocks {
			if id.file != fh {
				continue
			}
			if err := remove(c.blockPath(fh, id.index)); err != nil {
				return err
			}
			if err := remove(c.sidecarPath(fh, id.index)); err != nil {
				return err
			}
			c.bytes -= m.size
			c.dropBlockLocked(id)
			delete(fs.present, id.index)
		}
	}
	if err := remove(c.hydratedPath(fh)); err != nil {
		return err
	}
	if fs != nil && fs.part != nil {
		if err := remove(c.partPath(fh)); err != nil {
			return err
		}
		if err := remove(c.partBitmapPath(fh)); err != nil {
			return err
		}
		c.forgetPartLocked(fs, fs.part, true)
	}
	if fs != nil {
		c.detachWholeLocked(fh, fs)
	}
	if err := remove(c.keyPath(fh)); err != nil {
		return err
	}
	if durable {
		for _, dir := range []string{filepath.Dir(c.hydratedPath(fh)), filepath.Dir(c.keyPath(fh))} {
			d, err := os.Open(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			err = d.Sync()
			closeErr := d.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	delete(c.files, fh)
	return nil
}

// makeRoom evicts until admitting need bytes respects every cap.
func (c *Cache) makeRoom(need int64) error {
	entries := 0
	if need > 0 {
		entries = 1
	}
	return c.makeRoomFor(need, entries, need, nil)
}

// makeRoomFor is called with admitMu held. Free-space need may be zero for
// a hard link even though its logical bytes must enter the cache budget.
func (c *Cache) makeRoomFor(need int64, entries int, diskNeed int64, exclude *blockID) error {
	now := c.opt.Now()
	c.mu.Lock()
	expire := now.Sub(c.lastExpire) >= housekeepEvery
	if expire {
		c.lastExpire = now
	}
	minFree, maxBytes := c.minFree.Load(), c.maxBytes.Load()
	checkFree := minFree > 0 && (diskNeed > 0 || now.Sub(c.lastFreeCheck) >= housekeepEvery || c.bytes+c.wholeBytes+c.partBytes-c.bytesAtFreeCheck >= freeCheckBytes)
	c.mu.Unlock()
	if expire {
		c.evictExpiredExcept(exclude)
	}
	for {
		c.mu.Lock()
		overBytes := maxBytes > 0 && c.bytes+c.wholeBytes+c.partBytes+c.orphanBytes+c.reservedBytes+c.detachedFlushBytes+need > maxBytes
		overObjects := c.opt.MaxBlocks > 0 && len(c.blocks)+len(c.objects)+len(c.parts)+len(c.orphanTemps)+c.reservedEntries+c.detachedFlushEntries+entries > c.opt.MaxBlocks
		c.mu.Unlock()
		if !overBytes && !overObjects {
			break
		}
		if !c.evictOneExcept(exclude) {
			return ErrNoSpace
		}
	}
	if checkFree {
		for {
			free, err := c.opt.FreeSpace(c.opt.Dir)
			if err != nil {
				return fmt.Errorf("cache: check free space: %w", err)
			}
			c.mu.Lock()
			reserved := c.reservedBytes + c.writeReserved + c.wb.pending + c.detachedFlushBytes
			c.mu.Unlock()
			if free >= minFree && diskNeed <= free-minFree && reserved <= free-minFree-diskNeed {
				break
			}
			if !c.evictOneExcept(exclude) {
				return ErrNoSpace
			}
		}
		c.mu.Lock()
		c.lastFreeCheck = now
		c.bytesAtFreeCheck = c.bytes + c.wholeBytes + c.partBytes
		c.mu.Unlock()
	}
	return nil
}

// housekeepEvery bounds how often makeRoom scans for expired blocks and
// asks for free space; freeCheckBytes forces a free-space check sooner when
// that many bytes were added since the last one.
const (
	housekeepEvery = time.Second
	freeCheckBytes = 32 << 20
)

// piece is one sub-block's worth of a read of a partial block: either the
// bytes still held in memory, or an instruction to read them from disk.
type piece struct {
	at   int64  // offset within the block
	n    int    // length
	data []byte // nil means on disk
}

// partialPiecesLocked splits a read of a partial block into pieces by
// sub-block, capturing the in-memory ones now. c.mu must be held.
func (c *Cache) partialPiecesLocked(m *blockMeta, off int64, dst []byte) []piece {
	sub := m.partial.sub
	var out []piece
	for n := 0; n < len(dst); {
		pos := off + int64(n)
		i := int(pos / sub)
		within := pos - int64(i)*sub
		take := int(sub - within)
		if take > len(dst)-n {
			take = len(dst) - n
		}
		if b, inMem := m.subs[i]; inMem {
			if within >= int64(len(b)) {
				break
			}
			if int64(take) > int64(len(b))-within {
				take = int(int64(len(b)) - within)
			}
			out = append(out, piece{at: pos, n: take, data: b[within : within+int64(take)]})
		} else {
			out = append(out, piece{at: pos, n: take})
		}
		n += take
	}
	return out
}

// readPieces assembles a read of a partial block from memory and disk.
func (c *Cache) readPieces(open *os.File, path string, off int64, dst []byte, pieces []piece) (int, error) {
	needDisk := false
	for _, p := range pieces {
		if p.data == nil {
			needDisk = true
		}
	}
	f := open
	if f == nil && needDisk {
		// The block file may not exist yet when everything is in memory,
		// so it is opened only for pieces that are on disk.
		var err error
		f, err = os.Open(path)
		if err != nil {
			return 0, err
		}
		defer f.Close()
	}
	n := 0
	for _, p := range pieces {
		if p.data != nil {
			n += copy(dst[n:n+p.n], p.data)
			continue
		}
		r, err := f.ReadAt(dst[n:n+p.n], p.at)
		n += r
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
		if r < p.n {
			break
		}
	}
	return n, nil
}

// dropBlockLocked removes a block from the index, closing the file it may
// have kept open. c.mu must be held.
func (c *Cache) dropBlockLocked(id blockID) {
	if m, ok := c.blocks[id]; ok {
		if m.claimLagsLocked() {
			c.lagging--
		}
		if m.f != nil {
			m.f.Close()
			m.f = nil
			c.openPartials--
		}
		// The block file is being unlinked or renamed over; nothing may keep
		// reading it afterwards through a descriptor that names it.
		c.retireBlockReadLocked(id, m)
		if m.mem != nil {
			if m.flushing {
				m.flushDetached = true
				c.detachedFlushBytes += int64(len(m.mem))
				c.detachedFlushEntries++
			}
			c.wb.pending -= int64(len(m.mem))
			m.mem = nil
		}
		for _, b := range m.subs {
			c.wb.pending -= int64(len(b))
		}
		m.subs = nil
	}
	delete(c.blocks, id)
}

// evictExpired drops blocks older than MaxAge.
func (c *Cache) evictExpired() { c.evictExpiredExcept(nil) }

func (c *Cache) evictExpiredExcept(exclude *blockID) {
	if c.opt.MaxAge <= 0 {
		return
	}
	cutoff := c.opt.Now().Add(-c.opt.MaxAge)
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, m := range c.blocks {
		fs := c.files[id.file]
		if m.pinned || fs != nil && fs.busy > 0 || exclude != nil && id == *exclude || !m.lastAccess.Before(cutoff) {
			continue
		}
		if err := os.Remove(c.blockPath(id.file, id.index)); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		os.Remove(c.sidecarPath(id.file, id.index))
		c.bytes -= m.size
		c.dropBlockLocked(id)
		c.evictions++
		if fs != nil {
			delete(fs.present, id.index)
		}
	}
	for _, o := range c.objects {
		if o.lastAccess.Before(cutoff) {
			c.evictObjectLocked(o)
		}
	}
	for _, p := range c.parts {
		if p.lastAccess.Before(cutoff) {
			c.evictPartLocked(p)
		}
	}
}

// evictOne removes the best victim: coldest probationary block first, then
// coldest hot block. Pinned blocks are never evicted. It reports whether it
// freed anything.
func (c *Cache) evictOne() bool { return c.evictOneExcept(nil) }

func (c *Cache) evictOneExcept(exclude *blockID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cleanTempLocked() {
		return true
	}
	var victim blockID
	var object *wholeObject
	found, bestHot := false, false
	var bestTime time.Time
	better := func(hot bool, at time.Time) bool {
		return !found || bestHot && !hot || bestHot == hot && at.Before(bestTime)
	}
	for id, m := range c.blocks {
		fs := c.files[id.file]
		if m.pinned || fs != nil && fs.busy > 0 || exclude != nil && id == *exclude {
			continue
		}
		if better(m.hot, m.lastAccess) {
			victim, bestHot, bestTime, found = id, m.hot, m.lastAccess, true
		}
	}
	for _, o := range c.objects {
		if len(o.refs) == 0 || c.objectProtectedLocked(o) {
			continue
		}
		if better(o.hot, o.lastAccess) {
			object, bestHot, bestTime, found = o, o.hot, o.lastAccess, true
		}
	}
	var part *wholePart
	for _, p := range c.parts {
		if c.partProtectedLocked(c.files[p.fh]) {
			continue
		}
		if better(p.hot, p.lastAccess) {
			part, object, bestHot, bestTime, found = p, nil, p.hot, p.lastAccess, true
		}
	}
	if !found {
		return false
	}
	if part != nil {
		return c.evictPartLocked(part)
	}
	if object != nil {
		return c.evictObjectLocked(object)
	}
	if err := os.Remove(c.blockPath(victim.file, victim.index)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	os.Remove(c.sidecarPath(victim.file, victim.index))
	c.bytes -= c.blocks[victim].size
	c.dropBlockLocked(victim)
	c.evictions++
	if fs := c.files[victim.file]; fs != nil {
		delete(fs.present, victim.index)
	}
	return true
}

// GC applies every cap without admitting new data. `cloudfs cache gc` calls it.
func (c *Cache) GC() error {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	c.mu.Lock()
	for c.cleanTempLocked() {
	}
	c.mu.Unlock()
	c.evictExpired()
	return c.makeRoom(0)
}

// Stats snapshots the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Stats{Blocks: len(c.blocks), Bytes: c.bytes + c.wholeBytes + c.partBytes + c.orphanBytes, WholeBytes: c.wholeBytes, SparseBytes: c.partBytes, ReservedBytes: c.reservedBytes + c.detachedFlushBytes, WriteReservedBytes: c.writeReserved, OrphanBytes: c.orphanBytes, Hits: c.hits, Misses: c.misses, Evictions: c.evictions}
	for _, o := range c.objects {
		if o.readers > 0 {
			s.LeasedBytes += o.size
		}
	}
	for _, fs := range c.files {
		if fs.hydrated {
			s.HydratedFiles++
		}
	}
	for _, m := range c.blocks {
		if m.pinned {
			s.PinnedBlocks++
		}
	}
	return s
}

// Keys lists the cached file keys, sorted, for diagnostics.
// UserPinnedKeys returns the keys that currently carry the persistent pin,
// in a stable order. Reconciliation works from this rather than from the whole
// cache: after a tree change, only something that was pinned can have stopped
// being pinned, and walking every cached object to find that out costs a
// metadata query per object on a path that every rename takes.
func (c *Cache) UserPinnedKeys() []FileKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []FileKey{}
	for _, fs := range c.files {
		if fs.userPinned && fs.key.RemoteID != "" {
			out = append(out, fs.key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Known reports whether the cache already has a record for k. Restoring a pin
// consults it so that a rule covering a thousand files that have never been
// read does not create a thousand cache records for content nobody fetched;
// those are pinned when their first block is admitted instead.
func (c *Cache) Known(k FileKey) bool {
	if k.RemoteID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.files[k.hash()]
	return ok
}

func (c *Cache) Keys() []FileKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]FileKey, 0, len(c.files))
	for _, fs := range c.files {
		if fs.key.RemoteID != "" {
			out = append(out, fs.key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// claimLagsLocked reports whether the block holds more sub-blocks than its
// on-disk claim names. c.mu must be held.
func (m *blockMeta) claimLagsLocked() bool {
	return m.partial != nil && m.partial.have > m.sidecarHave
}

// readInto reads a whole file into buf, growing it only when the file does
// not fit. The returned slice aliases buf.
func readInto(buf []byte, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if int64(cap(buf)) < st.Size() {
		buf = make([]byte, st.Size())
	}
	buf = buf[:st.Size()]
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
