package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// Handle is an open file. Reads track sequentiality so the VFS can grow a
// read-ahead window; writes accumulate in a staging file (see write.go).
type Handle struct {
	FH    uint64
	Ino   uint64
	Node  meta.Node
	Mount Mount

	mu sync.Mutex
	// lastBlock is the block of the previous read and lastEnd the offset just
	// past it; seqRun counts consecutive reads that continued from lastEnd.
	// Read-ahead arms only after a run, because on a big file a random read
	// lands in the block after the previous one often enough that block
	// adjacency alone kept pulling whole blocks a random reader never used.
	lastBlock int64
	lastEnd   int64
	seqRun    int
	// window is the current read-ahead width in blocks.
	window int
	// readAheadCtx owns prefetches for the current sequential run. A seek or
	// close cancels the old run so a media player jumping ahead does not keep
	// downloading blocks around the abandoned playback position.
	readAheadCtx    context.Context
	readAheadCancel context.CancelFunc
	// writer is non-nil for handles opened for writing.
	writer *writeState
	// parentRemoteID is the provider id of the parent directory, resolved
	// once per handle: every flush re-stages the file and needs it.
	parentRemoteID string
	closed         bool
	activeReads    int
	openedKey      cache.FileKey // immutable admission identity, even after Node changes
	// subMiss counts sub-block fetches per block on this handle. Enough of
	// them in one block means the reads have locality, and the rest of the
	// block is worth fetching in one go.
	subMiss map[int64]int
}

// Open opens a file by inode. write selects the write path.
func (f *FS) Open(ctx context.Context, ino uint64, write bool) (*Handle, error) {
	if write {
		f.copyPublishMu.Lock()
		defer f.copyPublishMu.Unlock()
		f.remotePublishMu.RLock()
		defer f.remotePublishMu.RUnlock()
	} else {
		f.copyPublishMu.RLock()
		defer f.copyPublishMu.RUnlock()
	}
	n, err := f.meta.Get(ctx, ino)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if n.IsDir() {
		return nil, ErrIsDir
	}
	if write {
		if pending, err := f.copyAwaitingSubmit(ctx, n); err != nil {
			return nil, err
		} else if pending {
			return nil, fmt.Errorf("vfs: copy publication is pending: %w", syscall.EBUSY)
		}
	}
	m, _, err := f.MountForIno(ctx, ino)
	if err != nil {
		return nil, err
	}
	h := &Handle{Ino: ino, Node: n, Mount: m, lastBlock: -1, window: 1}
	h.openedKey = h.fileKey()
	if _, err := f.protectPinned(ctx, n); err != nil {
		return nil, err
	}
	if write {
		if m.Mode == "readonly" {
			return nil, ErrReadOnly
		}
		ws, err := f.newWriteState(ctx, h)
		if err != nil {
			return nil, err
		}
		h.writer = ws
	}
	f.mu.Lock()
	h.FH = f.nextFH
	f.nextFH++
	f.handles[h.FH] = h
	if write {
		f.addWriterLocked(ino)
	}
	f.mu.Unlock()
	return h, nil
}

// HandleByFH looks up an open handle.
func (f *FS) HandleByFH(fh uint64) (*Handle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.handles[fh]
	return h, ok
}

// Release closes a handle. For write handles it commits the staged data to
// the journal (or, in strict mode, waits for the upload).
func (f *FS) Release(ctx context.Context, h *Handle) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	w := h.writer
	remove := h.activeReads == 0
	readAheadCancel := h.readAheadCancel
	h.readAheadCtx, h.readAheadCancel = nil, nil
	h.mu.Unlock()
	if readAheadCancel != nil {
		readAheadCancel()
	}

	if remove {
		f.mu.Lock()
		delete(f.handles, h.FH)
		f.mu.Unlock()
	}

	if w != nil {
		defer f.removeWriter(h.Ino)
		return f.commitWrite(ctx, h, w)
	}
	return nil
}

func (h *Handle) fileKey() cache.FileKey {
	return cache.FileKey{Remote: h.Node.Remote, RemoteID: h.Node.RemoteID, Version: h.Node.Version}
}

// Read fills buf from offset off, faulting blocks in as needed and triggering
// read-ahead on sequential access. It returns the number of bytes read; a
// short read means end of file.
func (f *FS) Read(ctx context.Context, h *Handle, buf []byte, off int64) (int, error) {
	// Background listing work stands aside while a read is outstanding:
	// warming a tree is never worth adding latency to the data path.
	f.fgIO.Add(1)
	defer f.fgIO.Add(-1)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, syscall.EBADF
	}
	h.activeReads++
	w := h.writer
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.activeReads--
		remove := h.closed && h.activeReads == 0
		h.mu.Unlock()
		if remove {
			f.mu.Lock()
			delete(f.handles, h.FH)
			f.mu.Unlock()
		}
	}()
	if f.readStartedFault != nil {
		f.readStartedFault()
	}
	if w != nil && w.staging != nil {
		// A write handle reads through its staging file so the reader sees
		// its own writes. Once committed and not written since, the data is
		// in the cache under the handle's node like any other file.
		return w.readAt(buf, off)
	}
	return f.readCached(ctx, h, buf, off)
}

func (f *FS) readCached(ctx context.Context, h *Handle, buf []byte, off int64) (int, error) {
	// Stop detached work for the previous sequential run before waiting on
	// the new range. Doing this after the read would let the stale prefetches
	// finish during a slow seek.
	f.cancelReadAheadOnSeek(h, off)
	size := h.Node.Size
	if off >= size {
		return 0, io.EOF
	}
	if off+int64(len(buf)) > size {
		buf = buf[:size-off]
	}
	key := h.fileKey()
	if IsLocalOnly(key.RemoteID) {
		if _, known := f.cache.Present(key); known == 0 {
			// The upload landed and released the local-only entry, but
			// this handle still carries the identity from before. Take the
			// node's current identity: the same bytes now sit under the
			// remote key, or come from the backend.
			if fresh, err := f.meta.Get(ctx, h.Ino); err == nil && !IsLocalOnly(fresh.RemoteID) && fresh.RemoteID != "" {
				h.mu.Lock()
				h.Node = fresh
				h.mu.Unlock()
				key = h.fileKey()
				size = fresh.Size
				if off >= size {
					return 0, io.EOF
				}
				if off+int64(len(buf)) > size {
					buf = buf[:size-off]
				}
			}
		}
	}
	bs := f.cache.BlockSize()
	total := 0
	for total < len(buf) {
		pos := off + int64(total)
		idx := pos / bs
		inBlock := pos - idx*bs
		// The common case is a hit, and it must cost only the bytes asked
		// for: materialising the whole block here would make every read pay
		// the block size.
		n, ok := f.cache.ReadAt(key, idx, inBlock, buf[total:])
		if !ok {
			var err error
			if f.wantsSubBlock(h, idx) {
				n, err = f.readSubBlock(ctx, h, key, idx, inBlock, buf[total:])
			} else {
				var block []byte
				block, err = f.blockAt(ctx, h, key, idx)
				if err == nil {
					if inBlock >= int64(len(block)) {
						break
					}
					n = copy(buf[total:], block[inBlock:])
				}
			}
			if err != nil {
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
		}
		total += n
		if n == 0 {
			break
		}
	}
	f.maybeReadAhead(ctx, h, key, off, int64(total))
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// subBlockPromote is the floor on how many sub-block misses in one block, on
// one handle, make the rest of that block worth fetching in the background.
// The real threshold is a quarter of the block's sub-blocks when that is
// larger: a random workload over a big file touches every block a few times,
// and promoting on a small fixed count made it fetch the whole file in the
// background — exactly the traffic sub-blocks exist to avoid.
const subBlockPromote = 4

// seqSlackMax caps how far a read may start from the end of the previous one
// and still count as continuing it (the kernel reorders requests inside its
// read-ahead window, which is this large); seqArm is how many such reads in
// a row open the read-ahead window.
const (
	seqSlackMax = 1 << 20
	seqArm      = 3
)

// seqSlack scales the slack to the block size, so that a read every block
// (a strided scan) is not mistaken for a sequential one: a quarter of a
// block, at most seqSlackMax.
func (f *FS) seqSlack() int64 {
	if s := f.cache.BlockSize() / 4; s < seqSlackMax {
		return s
	}
	return seqSlackMax
}

// promoteThreshold returns the miss count at which a block is completed.
func (f *FS) promoteThreshold() int {
	subs := int(f.cache.BlockSize() / f.cache.SubBlockSize())
	if subs/4 > subBlockPromote {
		return subs / 4
	}
	return subBlockPromote
}

// wantsSubBlock decides whether a miss should fetch only the sub-blocks the
// read needs. A sequential reader wants whole blocks — read-ahead depends on
// them and the bytes will be used — while a random reader would pay the
// whole block for a few kilobytes. Backends without ranged reads, and caches
// configured without sub-blocks, always take whole blocks.
func (f *FS) wantsSubBlock(h *Handle, idx int64) bool {
	if f.cache.SubBlockSize() >= f.cache.BlockSize() {
		return false
	}
	if !h.Mount.Provider.Capabilities().RangeRead {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seqRun >= seqArm && (idx == h.lastBlock || idx == h.lastBlock+1) {
		return false
	}
	return h.subMiss[idx] < f.promoteThreshold()
}

// readSubBlock fetches just the sub-blocks covering the read, stores them,
// and copies out the bytes asked for. After several such fetches land in the
// same block the remainder is fetched in the background, so a reader that
// keeps coming back to a region ends up with the whole block without ever
// waiting for it.
func (f *FS) readSubBlock(ctx context.Context, h *Handle, key cache.FileKey, idx, inBlock int64, dst []byte) (int, error) {
	_, blockLen := f.cache.BlockRange(idx, h.Node.Size)
	if inBlock >= blockLen {
		return 0, io.EOF
	}
	want := int64(len(dst))
	if want > blockLen-inBlock {
		want = blockLen - inBlock
	}
	subOff, subLen := f.cache.SubRange(inBlock, want, blockLen)
	data, err := f.subFlight.Do(subKey{ino: h.Ino, idx: idx, off: subOff, n: subLen}, func() ([]byte, error) {
		if f.cache.HasRange(key, idx, subOff, subLen) {
			return nil, nil
		}
		return f.fetchSub(ctx, h, key, idx, subOff, subLen)
	})
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	if h.subMiss == nil {
		h.subMiss = map[int64]int{}
	}
	h.subMiss[idx]++
	promote := h.subMiss[idx] == f.promoteThreshold()
	h.mu.Unlock()
	if promote {
		f.completeBlockLater(ctx, h, key, idx)
	}
	// Serve straight from the bytes just fetched: reading them back from
	// the cache file is a second lookup and a pread for the same data.
	if data != nil && inBlock >= subOff && inBlock-subOff < int64(len(data)) {
		// The buffer is not recycled here. The flight hands the same slice
		// to every waiter, and only the leader would return it: the
		// followers are still copying out of it, and a buffer handed back
		// while they read serves them another block's bytes.
		return copy(dst[:want], data[inBlock-subOff:]), nil
	}
	if n, ok := f.cache.ReadAt(key, idx, inBlock, dst[:want]); ok {
		return n, nil
	}
	return 0, fmt.Errorf("vfs: sub-block %d/%d vanished from the cache", idx, subOff)
}

// fetchSub pulls one aligned range of a block from the backend.
func (f *FS) fetchSub(ctx context.Context, h *Handle, key cache.FileKey, idx, subOff, subLen int64) ([]byte, error) {
	if IsLocalOnly(h.Node.RemoteID) {
		return nil, fmt.Errorf("vfs: local-only file %q is missing from the cache", h.Node.Name)
	}
	// Not pooled: this buffer is handed to every waiter on the flight and
	// then to the cache, so there is no point at which it is known to have
	// no readers left.
	buf := make([]byte, subLen)
	read, err := f.readRange(ctx, h, idx*f.cache.BlockSize()+subOff, buf)
	if err != nil {
		return nil, err
	}
	if err := shortRead(int64(read), subLen, idx*f.cache.BlockSize()+subOff, h.Node.Size); err != nil {
		return nil, err
	}
	buf = buf[:read]
	if err := f.cache.PutRange(key, idx, subOff, buf, h.Node.Size); err != nil && !errors.Is(err, cache.ErrNoSpace) {
		return nil, err
	}
	return buf, nil
}

// readRange fills buf from the provider at off, through the buffer-filling
// fast path when the backend offers one and through ReadRange otherwise.
func (f *FS) readRange(ctx context.Context, h *Handle, off int64, buf []byte) (int, error) {
	if ra, ok := h.Mount.Provider.(provider.RangeReaderAt); ok {
		n, err := ra.ReadRangeAt(ctx, h.Node.RemoteID, h.Node.Version, off, buf)
		if !errors.Is(err, provider.ErrUnsupported) {
			if err != nil {
				return 0, mapProviderErr(err)
			}
			return n, nil
		}
	}
	rc, err := h.Mount.Provider.ReadRange(ctx, h.Node.RemoteID, h.Node.Version, off, int64(len(buf)))
	if err != nil {
		return 0, mapProviderErr(err)
	}
	defer rc.Close()
	read, err := io.ReadFull(rc, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return read, nil
}

// shortRead rejects a range that came back shorter than asked while the file
// is known to continue past it. Accepting it would cache a truncated block as
// complete — which is what a server that caps requests below the packet size
// would otherwise cause, silently. A short read at the very end of the file
// is the normal case and passes.
func shortRead(got, want, off, size int64) error {
	if got >= want || off+got >= size {
		return nil
	}
	return fmt.Errorf("%w: short read: %d of %d bytes at %d (file is %d bytes)",
		provider.ErrTransient, got, want, off, size)
}

// completeBlockLater fetches the rest of a block in the background.
func (f *FS) completeBlockLater(ctx context.Context, h *Handle, key cache.FileKey, idx int64) {
	node, mount := h.Node, h.Mount
	go func() {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		ph := &Handle{Ino: h.Ino, Node: node, Mount: mount, lastBlock: -1, window: 1}
		_, _ = f.blockFlight.Do(blockKey{ino: h.Ino, idx: idx}, func() ([]byte, error) {
			if f.cache.Has(key, idx) {
				return nil, nil
			}
			return f.fetchBlock(pctx, ph, key, idx)
		})
	}()
}

// blockAt returns one block, from cache or provider.
func (f *FS) blockAt(ctx context.Context, h *Handle, key cache.FileKey, idx int64) ([]byte, error) {
	if b, ok := f.cache.Get(key, idx); ok {
		return b, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		block, err := f.blockFlight.Do(blockKey{ino: h.Ino, idx: idx}, func() ([]byte, error) {
			// Another goroutine may have filled it while we waited.
			if b, ok := f.cache.Get(key, idx); ok {
				return b, nil
			}
			return f.fetchBlock(ctx, h, key, idx)
		})
		// A foreground read can arrive on a block owned by a stale read-ahead
		// run just as a seek cancels that run. Retry once with the foreground
		// context instead of leaking the background cancellation to the app.
		if !errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return block, err
		}
	}
	return nil, context.Canceled
}

func (f *FS) fetchBlock(ctx context.Context, h *Handle, key cache.FileKey, idx int64) ([]byte, error) {
	off, n := f.cache.BlockRange(idx, h.Node.Size)
	if n <= 0 {
		return nil, io.EOF
	}
	if IsLocalOnly(h.Node.RemoteID) {
		// The file exists only as the committed blob behind the cache. A miss
		// here means the cache entry was lost, which is a bug worth surfacing
		// rather than a provider request for an id the remote never had.
		return nil, fmt.Errorf("vfs: local-only file %q is missing from the cache", h.Node.Name)
	}
	// Not pooled: the cache keeps this buffer as the block's in-memory
	// copy while readers copy out of it, so it has no safe moment to be
	// recycled.
	buf := make([]byte, n)
	read, err := f.readRange(ctx, h, off, buf)
	if err != nil {
		return nil, err
	}
	if err := shortRead(int64(read), n, off, h.Node.Size); err != nil {
		return nil, err
	}
	buf = buf[:read]
	// A cache that is full degrades to uncached reads rather than failing.
	// The block is handed to the reader before it reaches the disk.
	if err := f.cache.PutAsync(key, idx, buf, h.Node.Size); err != nil && !errors.Is(err, cache.ErrNoSpace) {
		return nil, err
	}
	return buf, nil
}

// maybeReadAhead grows a sequential window and prefetches following blocks in
// the background. A read counts as sequential when it lands in the same or
// the next block and starts within seqSlack of where the previous read ended:
// the kernel's own read-ahead reorders requests inside its window, so exact
// contiguity is too strict, while block adjacency alone is too loose. The
// window only opens after seqArm such reads in a row.
func (f *FS) maybeReadAhead(ctx context.Context, h *Handle, key cache.FileKey, off, n int64) {
	// The sequential run is tracked even when prefetching is disabled:
	// wantsSubBlock relies on it to hand a sequential reader whole blocks.
	maxWindow := f.opt.ReadAheadBlocks
	bs := f.cache.BlockSize()
	idx := off / bs
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	adjacent := idx == h.lastBlock+1 || idx == h.lastBlock
	slack := f.seqSlack()
	near := off-h.lastEnd <= slack && h.lastEnd-off <= slack
	var staleCancel context.CancelFunc
	if adjacent && near {
		h.seqRun++
	} else {
		staleCancel = h.readAheadCancel
		h.readAheadCtx, h.readAheadCancel = nil, nil
		h.seqRun = 0
		h.window = 1
	}
	sequential := h.seqRun >= seqArm
	if sequential && maxWindow > 0 && h.window < maxWindow {
		h.window *= 2
		if h.window > maxWindow {
			h.window = maxWindow
		}
	}
	h.lastBlock = idx
	h.lastEnd = off + n
	window := h.window
	var readAheadCtx context.Context
	if sequential && window > 1 {
		if h.readAheadCtx == nil {
			h.readAheadCtx, h.readAheadCancel = context.WithCancel(context.WithoutCancel(ctx))
		}
		readAheadCtx = h.readAheadCtx
	}
	h.mu.Unlock()
	if staleCancel != nil {
		staleCancel()
	}
	if !sequential || window <= 1 {
		return
	}
	last := f.cache.BlockCount(h.Node.Size) - 1
	node, mount := h.Node, h.Mount
	for i := idx + 1; i <= idx+int64(window) && i <= last; i++ {
		i := i
		if f.cache.Has(key, i) {
			continue
		}
		bk := blockKey{ino: h.Ino, idx: i}
		// One goroutine per block in flight. The kernel sends a READ every
		// 128 KiB–1 MiB, and spawning a waiter per window block on each of
		// them piled up thousands of goroutines on the same flights; the
		// wake-up storm when a block landed cost more CPU than the copy.
		if !f.prefetching.claim(bk) {
			continue
		}
		go func() {
			defer f.prefetching.release(bk)
			// The run context outlives the triggering read, but a seek/close
			// cancels it and the timeout still bounds a stationary reader.
			pctx, cancel := context.WithTimeout(readAheadCtx, 2*time.Minute)
			defer cancel()
			ph := &Handle{Ino: h.Ino, Node: node, Mount: mount, lastBlock: -1, window: 1}
			_, _ = f.blockFlight.Do(bk, func() ([]byte, error) {
				if f.cache.Has(key, i) {
					return nil, nil
				}
				return f.fetchBlock(pctx, ph, key, i)
			})
		}()
	}
}

func (f *FS) cancelReadAheadOnSeek(h *Handle, off int64) {
	idx := off / f.cache.BlockSize()
	h.mu.Lock()
	if h.lastBlock < 0 {
		h.mu.Unlock()
		return
	}
	adjacent := idx == h.lastBlock+1 || idx == h.lastBlock
	slack := f.seqSlack()
	near := off-h.lastEnd <= slack && h.lastEnd-off <= slack
	var cancel context.CancelFunc
	if !adjacent || !near {
		cancel = h.readAheadCancel
		h.readAheadCtx, h.readAheadCancel = nil, nil
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// inflight is a set of blocks a prefetch goroutine is already working on.
type inflight struct {
	mu sync.Mutex
	m  map[blockKey]struct{}
}

func (s *inflight) claim(k blockKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[blockKey]struct{}{}
	}
	if _, ok := s.m[k]; ok {
		return false
	}
	s.m[k] = struct{}{}
	return true
}

func (s *inflight) release(k blockKey) {
	s.mu.Lock()
	delete(s.m, k)
	s.mu.Unlock()
}

// ReadFileRange is the path-based read used by MCP: it opens, reads a range
// and closes, without a persistent handle.
func (f *FS) ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if n.IsDir() {
		return nil, ErrIsDir
	}
	h, err := f.Open(ctx, n.Ino, false)
	if err != nil {
		return nil, err
	}
	defer f.Release(ctx, h)
	n = h.Node
	if off >= n.Size {
		return nil, nil
	}
	if length <= 0 || off+length > n.Size {
		length = n.Size - off
	}
	buf := make([]byte, length)
	read := 0
	for read < len(buf) {
		got, err := f.Read(ctx, h, buf[read:], off+int64(read))
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if got == 0 {
			break
		}
		read += got
	}
	return buf[:read], nil
}

// DownloadURL returns a direct link for a path, so a caller can fetch a large
// file without routing the bytes through cloudfs. It fails for files the
// provider will not serve to third parties, and for files not yet uploaded.
func (f *FS) DownloadURL(ctx context.Context, p string) (provider.Link, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return provider.Link{}, err
	}
	if n.IsDir() {
		return provider.Link{}, ErrIsDir
	}
	if IsLocalOnly(n.RemoteID) {
		return provider.Link{}, fmt.Errorf("vfs: %s has not been uploaded yet", p)
	}
	m, _, err := f.MountForIno(ctx, n.Ino)
	if err != nil {
		return provider.Link{}, err
	}
	if !m.Provider.Capabilities().LinkShareable {
		return provider.Link{}, fmt.Errorf("vfs: %s does not hand out links usable by other processes", m.Remote)
	}
	link, err := m.Provider.DownloadURL(ctx, n.RemoteID)
	if err != nil {
		return provider.Link{}, mapProviderErr(err)
	}
	return link, nil
}

// Prefetch pulls a whole file into the cache. `cloudfs pin` and the MCP pin
// tool use it.
func (f *FS) Prefetch(ctx context.Context, p string) error { return f.Pin(ctx, p) }

func (f *FS) fillPinned(ctx context.Context, p string) error {
	if !f.pathPinned(p) {
		return nil
	}
	n, err := f.resolve(ctx, p)
	if err != nil {
		return err
	}
	if n.IsDir() {
		kids, err := f.ReadDir(ctx, n.Ino)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := f.fillPinned(ctx, pathJoin(p, k.Name)); err != nil {
				return err
			}
		}
		return nil
	}
	h, err := f.Open(ctx, n.Ino, false)
	if err != nil {
		return err
	}
	defer f.Release(ctx, h)
	n = h.Node
	key := h.fileKey()
	keep, err := f.protectPinned(ctx, n)
	if err != nil || !keep {
		return err
	}
	if _, ok := f.cache.HydratedPath(key); ok {
		return nil
	}
	if IsLocalOnly(key.RemoteID) {
		return errors.New("vfs: pending content is unavailable in the local cache")
	}
	for idx := int64(0); idx < f.cache.BlockCount(n.Size); idx++ {
		if !f.pathPinned(p) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if f.cache.Has(key, idx) {
			continue
		}
		if _, err := f.blockAt(ctx, h, key, idx); err != nil {
			return err
		}
		if !f.cache.Has(key, idx) {
			return ErrNoSpace
		}
	}
	return f.cache.SettleFile(ctx, key, n.Size)
}

// prefetcher warms subdirectory listings in the background after a readdir, so
// find and ls -R mostly hit the local tree.
type prefetcher struct {
	fs       *FS
	depth    int
	queue    chan prefetchJob
	stopC    chan struct{}
	once     sync.Once
	inflight atomic.Int64
}

// prefetchFanout is how many sibling directories one prefetch job lists at
// once.
const prefetchFanout = 8

type prefetchJob struct {
	path  string
	ino   uint64
	depth int
}

func newPrefetcher(fs *FS, depth int) *prefetcher {
	p := &prefetcher{fs: fs, depth: depth, queue: make(chan prefetchJob, 256), stopC: make(chan struct{})}
	if depth > 0 {
		for i := 0; i < 2; i++ { // two workers keeps provider QPS low
			go p.run()
		}
	}
	return p
}

func (p *prefetcher) run() {
	for {
		select {
		case <-p.stopC:
			return
		case job := <-p.queue:
			p.waitIdle()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			kids, err := p.fs.meta.Children(ctx, job.ino)
			if err == nil {
				// Sibling directories are listed side by side: a tree of 40
				// folders costs one round trip, not forty in a row, and the
				// provider limiter still bounds the rate.
				sem := make(chan struct{}, prefetchFanout)
				var wg sync.WaitGroup
				for _, k := range kids {
					if k.Kind != provider.KindDir {
						continue
					}
					st, err := p.fs.meta.DirState(ctx, k.Ino)
					if err == nil && st.Complete {
						continue
					}
					wg.Add(1)
					sem <- struct{}{}
					go func(k meta.Node) {
						defer func() { <-sem; wg.Done() }()
						p.waitIdle()
						_, _ = p.fs.readDirRefresh(ctx, k.Ino, false)
						if job.depth < p.depth {
							p.enqueue(prefetchJob{path: pathJoin(job.path, k.Name), ino: k.Ino, depth: job.depth + 1})
						}
					}(k)
				}
				wg.Wait()
			}
			cancel()
			p.inflight.Add(-1)
		}
	}
}

// prefetchYield bounds how long a prefetch job stands aside for foreground
// reads. Reads on a mounted tree never stop entirely, so the wait has to end
// somewhere or a busy mount would never warm its listings at all.
const prefetchYield = 5 * time.Second

// waitIdle blocks while foreground reads are in flight. Listing a directory
// costs a provider round trip and a metadata write transaction, both of which
// the read path is waiting behind.
func (p *prefetcher) waitIdle() {
	deadline := time.Now().Add(prefetchYield)
	// Backing off keeps a long wait from costing thousands of timer
	// wake-ups: starting background listing a few tens of milliseconds late
	// is free, and up to nine of these can be waiting at once.
	for wait := time.Millisecond; ; {
		if p.fs.fgIO.Load() == 0 {
			return
		}
		select {
		case <-p.stopC:
			return
		default:
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(wait)
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}

func (p *prefetcher) schedule(path string, ino uint64, depth int) {
	if p.depth <= 0 || depth > p.depth {
		return
	}
	p.enqueue(prefetchJob{path: path, ino: ino, depth: depth})
}

func (p *prefetcher) enqueue(j prefetchJob) {
	p.inflight.Add(1)
	select {
	case p.queue <- j:
	default:
		p.inflight.Add(-1) // queue full: drop, the TTL path will catch up
	}
}

// drain waits until queued prefetch work finishes. Tests use it; production
// code never needs to block on prefetching.
func (p *prefetcher) drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.inflight.Load() == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (p *prefetcher) stop() {
	p.once.Do(func() { close(p.stopC) })
}

// pathJoin joins a directory path and a child name inside the mount.
func pathJoin(dir, name string) string { return path.Join(dir, name) }

// LocalPath returns the path of a fully cached copy of the file behind h, when
// one exists and nothing makes it unsafe to read directly: the handle is not
// writing, and the file's content is not a pending local write that the
// cache holds as its only copy. The FUSE layer hands this file to the kernel
// for passthrough reads.
func (f *FS) LocalPath(h *Handle) (string, bool) {
	h.mu.Lock()
	writing := h.writer != nil
	h.mu.Unlock()
	if writing || IsLocalOnly(h.Node.RemoteID) || h.Node.IsDir() {
		return "", false
	}
	return f.cache.HydratedPath(h.fileKey())
}

// OpenLocal leases the immutable complete cache entry for passthrough. The
// lease keeps GC from claiming its space while the kernel still owns the fd.
func (f *FS) OpenLocal(h *Handle) (*cache.WholeFile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.writer != nil || h.closed || IsLocalOnly(h.Node.RemoteID) || h.Node.IsDir() {
		return nil, ErrNotFound
	}
	return f.cache.OpenWhole(h.fileKey())
}
