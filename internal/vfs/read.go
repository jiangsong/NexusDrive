package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
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
	// followWrites keeps a reader on the committed identity after staging disappears.
	followWrites bool
	liveReadMu   sync.Mutex
	// parentRemoteID is the provider id of the parent directory, resolved
	// once per handle: every flush re-stages the file and needs it.
	parentRemoteID string
	closed         bool
	activeReads    int
	openedKey      cache.FileKey // immutable admission identity, even after Node changes
	// localKey is the identity OpenLocal leased a cache entry under; see
	// LocalCurrent.
	localKey cache.FileKey
	// snapshot pins the handle to the version it was opened against: a
	// commit on the inode does not move it to the new node (see
	// adoptCommittedNode). Copy reads through such a handle.
	snapshot bool
	// subMiss counts sub-block fetches per block on this handle. Enough of
	// them in one block means the reads have locality, and the rest of the
	// block is worth fetching in one go.
	subMiss map[int64]int
	// dirAheadSeen marks that this handle has already told the directory
	// read-ahead it started reading; the signal is one per open file, not
	// one per read.
	dirAheadSeen bool
	// rate is an exponentially weighted average of how fast this handle is
	// being read, in bytes per second, and lastReadAt when the previous
	// read returned. The read-ahead window is sized from it: a player
	// pulling 5 MiB/s needs a few seconds of runway, not a fixed 64 MiB.
	rate       float64
	lastReadAt time.Time
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
	if write && n.Kind == provider.KindSymlink {
		return nil, syscall.ELOOP
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
		if err := f.requireOwner(); err != nil {
			return nil, err
		}
		ws, err := f.newWriteState(ctx, h)
		if err != nil {
			return nil, err
		}
		h.writer = ws
	}
	f.mu.Lock()
	h.followWrites = !write && f.writers[ino] > 0
	h.FH = f.nextFH
	f.nextFH++
	f.registerHandleLocked(h)
	var readers []*Handle
	if write {
		readers = f.addWriterLocked(ino, true)
	}
	f.mu.Unlock()
	markReadersFollowing(readers)
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
	// Keep the writer discoverable until its final commit is visible to readers.
	if h.writer != nil {
		h.writer.mu.Lock()
		defer h.writer.mu.Unlock()
	}
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
		defer func() {
			f.mu.Lock()
			f.retireHandleLocked(h)
			f.mu.Unlock()
		}()
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
	return f.read(ctx, h, buf, off, true)
}

// read can retain the committed snapshot for durable background copies.
func (f *FS) read(ctx context.Context, h *Handle, buf []byte, off int64, live bool) (int, error) {
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
			f.retireHandleLocked(h)
			f.mu.Unlock()
		}
	}()
	if f.readStartedFault != nil {
		f.readStartedFault()
	}
	f.noteRead(ctx, h.Ino)
	if w != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.staging != nil {
			return w.readAt(buf, off)
		}
	} else if live {
		// Git index-pack opens extra readers before closing the pack writer.
		// They must see staging, including a final partial page absent from the
		// kernel cache. Serialize reads on this handle as its identity can move.
		h.liveReadMu.Lock()
		defer h.liveReadMu.Unlock()
		for _, writer := range f.writeHandlesFor(h.Ino) {
			ww := writer.writer
			ww.mu.Lock()
			writer.mu.Lock()
			closed := writer.closed
			writer.mu.Unlock()
			if !closed && ww.staging != nil && ww.pending == nil {
				h.mu.Lock()
				h.followWrites = true
				h.mu.Unlock()
				n, err := ww.readAt(buf, off)
				ww.mu.Unlock()
				return n, err
			}
			ww.mu.Unlock()
		}
		h.mu.Lock()
		follow := h.followWrites
		h.mu.Unlock()
		if follow {
			fresh, err := f.meta.Get(ctx, h.Ino)
			if err != nil {
				return 0, err
			}
			h.mu.Lock()
			h.Node = fresh
			h.mu.Unlock()
		}
	}
	if !live {
		// A versioned read wants what the tree last committed, never the
		// bytes a sibling descriptor is still staging.
		return f.readCached(ctx, h, buf, off)
	}
	if st, ok := f.stagedWriter(h.Ino); ok {
		// Another handle on this inode holds bytes it has not committed
		// yet. They are the file's content as far as every descriptor is
		// concerned — the size Stat reports comes from the same staging
		// file — so the read is served from there rather than from the
		// cache, which still holds the version before the write began.
		n, err := readStaged(st, buf, off)
		if !errors.Is(err, fs.ErrClosed) {
			return n, err
		}
		// The writer committed between the lookup and the read. Its bytes
		// are in the cache now, under the identity the commit gave the
		// node; the snapshot this handle took at open predates it.
		f.refreshNode(ctx, h)
	}
	return f.readCached(ctx, h, buf, off)
}

// readCommitted reads the last committed content of the file behind h,
// ignoring bytes any open write handle has staged: a read handle used to
// snapshot a version, not to observe a file being written.
func (f *FS) readCommitted(ctx context.Context, h *Handle, buf []byte, off int64) (int, error) {
	h.mu.Lock()
	h.snapshot = true
	h.mu.Unlock()
	f.fgIO.Add(1)
	defer f.fgIO.Add(-1)
	f.noteRead(ctx, h.Ino)
	return f.readCached(ctx, h, buf, off)
}

// readStaged reads from a staging file with read(2) semantics: a short read
// at the end is a count, and only a read starting at or past the end is EOF.
func readStaged(st *journal.Staging, buf []byte, off int64) (int, error) {
	n, err := st.ReadAt(buf, off)
	if errors.Is(err, io.EOF) && n > 0 {
		return n, nil
	}
	return n, err
}

// refreshNode replaces the handle's snapshot of its node with the tree's
// current row. A handle opened for reading keeps the node as it was at
// open, which is right until a writer on the same inode commits: from then
// on the cache holds the content under the new identity and the old key is
// forgotten, so reading through the snapshot would fetch the old version
// again.
func (f *FS) refreshNode(ctx context.Context, h *Handle) {
	fresh, err := f.meta.Get(ctx, h.Ino)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.Node = fresh
	h.mu.Unlock()
}

// errLocalOnlyGone marks a read of a file that exists only as a committed
// local blob whose cache entry is no longer under the handle's key. It is
// almost always the upload landing underneath the reader, which is
// recoverable; anything else is a lost blob and stays an error.
var errLocalOnlyGone = errors.New("vfs: local-only cache entry is gone")

// adoptRemoteIdentity re-reads the node and, if the upload has landed, moves
// the handle onto the remote identity the bytes now live under.
func (f *FS) adoptRemoteIdentity(ctx context.Context, h *Handle) (meta.Node, bool) {
	fresh, err := f.meta.Get(ctx, h.Ino)
	if err != nil || IsLocalOnly(fresh.RemoteID) || fresh.RemoteID == "" {
		return meta.Node{}, false
	}
	h.mu.Lock()
	h.Node = fresh
	h.mu.Unlock()
	return fresh, true
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
			if adopted, ok := f.adoptRemoteIdentity(ctx, h); ok {
				key, size = h.fileKey(), adopted.Size
				if off >= size {
					return 0, io.EOF
				}
				if off+int64(len(buf)) > size {
					buf = buf[:size-off]
				}
			}
		}
	}
	h.mu.Lock()
	first := !h.dirAheadSeen
	h.dirAheadSeen = true
	h.mu.Unlock()
	if first {
		f.noteDirRead(ctx, h)
	}
	bs := f.cache.BlockSize()
	total := 0
	stalled := false
	// awaited: a read waits for an in-flight prefetch of its own file at
	// most once, and only when it is about to go to the drive itself. A read
	// the cache can serve never waits for speculation at all.
	awaited := false
	for total < len(buf) {
		pos := off + int64(total)
		idx := pos / bs
		inBlock := pos - idx*bs
		// The common case is a hit, and it must cost only the bytes asked
		// for: materialising the whole block here would make every read pay
		// the block size.
		n, ok := f.cache.ReadAt(key, idx, inBlock, buf[total:])
		if !ok && !awaited {
			// A sibling prefetch may already be fetching this very file.
			// Waiting for it saves the drive a second request for the same
			// bytes, and what it installs is the whole file, so the miss
			// below is usually a hit by the time it returns.
			awaited = true
			if f.awaitDirReadAhead(ctx, h.Ino) {
				n, ok = f.cache.ReadAt(key, idx, inBlock, buf[total:])
			}
		}
		if !ok {
			// The reader had to wait for bytes: the window is behind the
			// reader and may grow. A read served from the cache is proof
			// it is far enough ahead already.
			stalled = true
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
				// The entry can also be released between the check above and
				// this read: the upload completes, the node moves to the
				// remote identity and the local-only entry is forgotten while
				// this loop is in it. Retrying under the node's current
				// identity is the same recovery, applied where it actually
				// fails rather than only where it is convenient to look.
				if total == 0 && IsLocalOnly(key.RemoteID) && errors.Is(err, errLocalOnlyGone) {
					if adopted, ok := f.adoptRemoteIdentity(ctx, h); ok {
						key, size = h.fileKey(), adopted.Size
						if off >= size {
							return 0, io.EOF
						}
						if off+int64(len(buf)) > size {
							buf = buf[:size-off]
						}
						continue
					}
				}
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
	f.maybeReadAhead(ctx, h, key, off, int64(total), stalled)
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
		return nil, fmt.Errorf("%w: %q", errLocalOnlyGone, h.Node.Name)
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

// errShortRange marks a shortRead failure distinctly from other transient
// errors, so a coalesced multi-block readahead run can tell "the server
// capped this request's size" apart from an ordinary transient failure and
// fall back to single-block requests instead of retrying the same range.
var errShortRange = errors.New("vfs: short range read")

// shortRead rejects a range that came back shorter than asked while the file
// is known to continue past it. Accepting it would cache a truncated block as
// complete — which is what a server that caps requests below the packet size
// would otherwise cause, silently. A short read at the very end of the file
// is the normal case and passes.
func shortRead(got, want, off, size int64) error {
	if got >= want || off+got >= size {
		return nil
	}
	return fmt.Errorf("%w: %w: %d of %d bytes at %d (file is %d bytes)",
		provider.ErrTransient, errShortRange, got, want, off, size)
}

// ReadFileRange is the path-based read used by MCP: it opens, reads a range
// and closes, without a persistent handle.
func (f *FS) ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error) {
	data, _, err := f.ReadFileRangeAtVersion(ctx, p, "", off, length)
	return data, err
}

// ReadFileRangeAtVersion is ReadFileRange with an optional version fence.
// The version is checked on the opened handle, so replacing the path between
// a preceding stat and this call cannot return bytes from the replacement.
// On a mismatch data is nil and current is the version that was opened.
func (f *FS) ReadFileRangeAtVersion(ctx context.Context, p, expected string, off, length int64) (data []byte, current string, err error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return nil, "", err
	}
	if n.IsDir() {
		return nil, "", ErrIsDir
	}
	h, err := f.Open(ctx, n.Ino, false)
	if err != nil {
		return nil, "", err
	}
	defer f.Release(ctx, h)
	n = h.Node
	current = n.Version
	if expected != "" && current != expected {
		return nil, current, nil
	}
	if off >= n.Size {
		return nil, current, nil
	}
	if length <= 0 || off+length > n.Size {
		length = n.Size - off
	}
	buf := make([]byte, length)
	read := 0
	for read < len(buf) {
		got, err := f.read(ctx, h, buf[read:], off+int64(read), expected == "")
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, current, err
		}
		if got == 0 {
			break
		}
		read += got
	}
	return buf[:read], current, nil
}

// ErrNoDownloadURL says the mount's provider serves file bytes only to
// requests carrying its own credential, so there is no link another process
// could use (Drive, Box; a pool inherits the answer from its members).
// ErrNotUploaded says the file exists only in the local journal so far.
// Both are sentinels rather than sentences because the control server and
// the MCP tools put them in front of people, in their language.
var (
	ErrNoDownloadURL = errors.New("vfs: this remote does not hand out links usable by other processes")
	ErrNotUploaded   = errors.New("vfs: the file has not been uploaded yet")
)

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
		return provider.Link{}, fmt.Errorf("%w: %s", ErrNotUploaded, p)
	}
	m, _, err := f.MountForIno(ctx, n.Ino)
	if err != nil {
		return provider.Link{}, err
	}
	if !m.Provider.Capabilities().LinkShareable {
		return provider.Link{}, fmt.Errorf("%w: %s", ErrNoDownloadURL, m.Remote)
	}
	link, err := m.Provider.DownloadURL(ctx, n.RemoteID)
	if err != nil {
		return provider.Link{}, mapProviderErr(err)
	}
	return link, nil
}

// HandsOutLinks reports whether DownloadURL can ever succeed under p: the
// mount's provider serves bytes to third parties. The console asks it per
// directory so it can leave the "download link" button out instead of
// offering one that every click refuses.
func (f *FS) HandsOutLinks(ctx context.Context, p string) (bool, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return false, err
	}
	m, _, err := f.MountForIno(ctx, n.Ino)
	if err != nil {
		return false, err
	}
	return m.Provider.Capabilities().LinkShareable, nil
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
		// readDirRefresh, not ReadDir: filling a pin is background work, and
		// only the child names are wanted. Going through ReadDir would count
		// the pin filler as a foreground metadata request against itself, and
		// build an Attr per entry that nothing here reads.
		kids, err := f.readDirRefresh(ctx, n.Ino, false)
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
	workers  sync.WaitGroup
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
			p.workers.Add(1)
			go p.run()
		}
	}
	return p
}

func (p *prefetcher) run() {
	defer p.workers.Done()
	for {
		select {
		case <-p.stopC:
			return
		case job := <-p.queue:
			p.waitIdle()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			go func() {
				select {
				case <-p.stopC:
					cancel()
				case <-ctx.Done():
				}
			}()
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
	p.fs.yieldToForeground(p.stopC, prefetchYield)
}

func (p *prefetcher) schedule(path string, ino uint64, depth int) {
	if p.depth <= 0 || depth > p.depth {
		return
	}
	p.enqueue(prefetchJob{path: path, ino: ino, depth: depth})
}

func (p *prefetcher) enqueue(j prefetchJob) {
	select {
	case <-p.stopC:
		return
	default:
	}
	p.inflight.Add(1)
	select {
	case p.queue <- j:
	case <-p.stopC:
		p.inflight.Add(-1)
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
	p.workers.Wait()
}

// pathJoin joins a directory path and a child name inside the mount.
func pathJoin(dir, name string) string { return path.Join(dir, name) }

// LocalReadAllowed excludes mutable files from immutable cache leases.
func (f *FS) LocalReadAllowed(h *Handle) bool {
	h.mu.Lock()
	allowed := h.writer == nil && !h.closed && !h.followWrites && !IsLocalOnly(h.Node.RemoteID) && !h.Node.IsDir()
	h.mu.Unlock()
	return allowed && !f.hasWriter(h.Ino)
}

// LocalPath returns the path of a fully cached copy of the file behind h, when
// one exists and nothing makes it unsafe to read directly: the handle is not
// writing, and the file's content is not a pending local write that the
// cache holds as its only copy. The FUSE layer hands this file to the kernel
// for passthrough reads.
func (f *FS) LocalPath(h *Handle) (string, bool) {
	if !f.LocalReadAllowed(h) {
		return "", false
	}
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
	if !f.LocalReadAllowed(h) {
		return nil, ErrNotFound
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.writer != nil || h.closed || IsLocalOnly(h.Node.RemoteID) || h.Node.IsDir() {
		return nil, ErrNotFound
	}
	key := h.fileKey()
	wf, err := f.cache.OpenWhole(key)
	if err != nil {
		return nil, err
	}
	h.localKey = key
	return wf, nil
}

// LocalCurrent reports whether the copy OpenLocal leased for h is still the
// file's content. The lease is a descriptor on the immutable entry for one
// version; it stops being the file the moment another handle stages bytes
// into it, and for good once that handle's close commits them under a new
// identity. Both are cases where the file is being rewritten out from under
// a reader, and both are what a local disk shows the reader immediately, so
// the caller falls back to Read, which serves the staged bytes (write.go)
// or the committed version.
func (f *FS) LocalCurrent(h *Handle) bool {
	h.mu.Lock()
	current := !h.closed && h.fileKey() == h.localKey
	h.mu.Unlock()
	if !current {
		return false
	}
	_, staged := f.stagedWriter(h.Ino)
	return !staged
}

// ShareTarget is what the share tool needs to know about a file before
// it asks the provider for a public link (docs/agent-first-design.md
// §8.2): whether it is on the drive at all, how much of it the cache
// holds (the credential scan reads only what is cached), and the
// provider's Sharer with the remote id. Zero provider calls.
type ShareTarget struct {
	Path     string
	Remote   string
	RemoteID string
	Size     int64
	// Synced is false while the file exists only locally.
	Synced bool
	// Cached is the fraction of the file the block cache holds.
	Cached float64
	Sharer provider.Sharer
}

// ErrNoShare says the mount's provider cannot create public links.
var ErrNoShare = errors.New("vfs: this remote cannot create public links")

// ShareTargetOf resolves p for sharing.
func (f *FS) ShareTargetOf(ctx context.Context, p string) (ShareTarget, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return ShareTarget{}, err
	}
	if n.IsDir() {
		return ShareTarget{}, ErrIsDir
	}
	m, _, err := f.MountForIno(ctx, n.Ino)
	if err != nil {
		return ShareTarget{}, err
	}
	t := ShareTarget{Path: path.Clean("/" + p), Remote: m.Remote, RemoteID: n.RemoteID, Size: n.Size, Synced: !IsLocalOnly(n.RemoteID)}
	if t.Synced && n.Size > 0 {
		have, total := f.cache.Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
		if total > 0 {
			t.Cached = float64(have) / float64(total)
		}
	} else if n.Size == 0 {
		t.Cached = 1
	}
	sh, ok := provider.SharerOf(m.Provider)
	if !ok {
		return t, ErrNoShare
	}
	t.Sharer = sh
	return t, nil
}
