package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
)

// writeState is the staging file behind an open write handle.
type writeState struct {
	// mu serializes staging IO with commit, which closes and replaces its file.
	mu      sync.Mutex
	staging *journal.Staging
	// parentRemoteID is where the file will be created.
	parentRemoteID string
	parentIno      uint64
	name           string
	// expectedVersion is the remote version at open time, for conflict detection.
	expectedVersion string
	mode            config.Mode
	dirty           bool
	// lastUploadID is the upload this handle queued on its previous flush,
	// kept for diagnostics. Superseding is done per inode at commit time,
	// because the kernel truncates through a handle of its own and this one
	// never sees what that queued.
	lastUploadID string
	// committed is the node as the last successful commit left it, so the
	// handle can refresh itself without reading it back.
	committed *meta.Node
	// pending is a commit whose blob is already in the objects directory
	// but which failed later (a busy metadata store, say). The next flush
	// finishes it rather than committing the same staging file twice.
	pending *pendingCommit
}

// pendingCommit is the state of a commit between the irreversible steps.
type pendingCommit struct {
	blob      string
	size      int64
	hashes    map[provider.HashType]string
	upload    journal.Upload
	journaled bool
}

// SetWriteBackend wires the journal and uploader into the VFS. It is separate
// from New so a read-only mount can skip both.
func (f *FS) SetWriteBackend(j *journal.Journal, u *upload.Uploader) {
	f.journal = j
	f.uploader = u
	if j != nil {
		// The deletes still queued from before: their targets must stay
		// out of listings until they run.
		_ = f.loadQueuedDeletes(context.Background(), j)
	}
}

// Journal exposes the write journal (status, doctor).
func (f *FS) Journal() *journal.Journal { return f.journal }

// requireOwner is the write fence of a VFS that shares its cache with the
// storage owner: a `cloudfs mcp` stdio server started beside `cloudfs
// mount` gets a journal it does not own, no uploader, and the same meta.
// A mutation that went ahead would leave a node in shared meta and a
// journal row the owner never publishes (TODO.md T-43), so every write
// entry point refuses with ErrNotOwner before touching either. A VFS with
// no journal at all is left to the "no write backend" errors of each path,
// which is how read-only assemblies and tests without a journal run.
func (f *FS) requireOwner() error {
	if f.journal != nil && !f.journal.Owner() {
		return ErrNotOwner
	}
	return nil
}

// newWriteState prepares staging for a write handle. An existing file is
// materialised into staging first so random writes and appends work.
func (f *FS) newWriteState(ctx context.Context, h *Handle) (*writeState, error) {
	if f.journal == nil {
		return nil, errors.New("vfs: no write backend configured")
	}
	parentRemoteID := h.parentRemoteID
	if parentRemoteID == "" {
		id, err := f.dirRemoteID(ctx, h.Node.ParentIno)
		if err != nil {
			return nil, err
		}
		parentRemoteID = id
		if parentRemoteID == "" {
			parentRemoteID = h.Mount.RootID
		}
		h.parentRemoteID = parentRemoteID
	}
	want := h.Mount.Provider.Capabilities().RapidUpload
	if len(want) == 0 {
		want = h.Mount.Provider.Capabilities().HashTypes
	}
	ws := &writeState{
		parentRemoteID: parentRemoteID, parentIno: h.Node.ParentIno,
		name: h.Node.Name,
		// Conflict detection compares against what the provider last had, not
		// against a local-only version from an earlier unsent write of ours.
		// Using the local token here would make every second local write to
		// the same file look like someone else's change.
		expectedVersion: h.Node.RemoteVersion,
		mode:            h.Mount.Mode,
	}
	return ws, nil
}

// openStaging creates the staging file for a write handle and seeds it with
// the file's current content, so a partial write does not truncate it. Empty
// and brand-new files skip the download.
//
// It happens at the first write, not at open: the kernel turns O_TRUNC into a
// separate truncate through a handle of its own, and a handle that had
// already downloaded the old content would write its 4 KiB into a full-sized
// staging file and commit the old tail back. It also means opening a large
// file for writing costs nothing until something is actually written.
func (f *FS) openStaging(ctx context.Context, h *Handle, seed bool) (*journal.Staging, error) {
	want := h.Mount.Provider.Capabilities().RapidUpload
	if len(want) == 0 {
		want = h.Mount.Provider.Capabilities().HashTypes
	}
	st, err := f.journal.NewStaging(want)
	if err != nil {
		return nil, err
	}
	if seed && h.Node.RemoteID != "" && h.Node.Size > 0 {
		if err := f.materialise(ctx, h, st); err != nil {
			st.Discard()
			return nil, err
		}
	}
	return st, nil
}

// materialise copies the current remote content into the staging file.
func (f *FS) materialise(ctx context.Context, h *Handle, st *journal.Staging) error {
	key := h.fileKey()
	// A hydrated cache entry is a straight file copy.
	if src, err := f.cache.OpenWhole(key); err == nil {
		defer src.Close()
		buf := make([]byte, 1<<20)
		var off int64
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := st.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
				off += int64(n)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					break
				}
				return rerr
			}
		}
		return nil
	}
	// Otherwise pull it block by block through the normal read path.
	total := f.cache.BlockCount(h.Node.Size)
	for idx := int64(0); idx < total; idx++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, err := f.blockAt(ctx, h, key, idx)
		if err != nil {
			return err
		}
		off, _ := f.cache.BlockRange(idx, h.Node.Size)
		if _, err := st.WriteAt(b, off); err != nil {
			return err
		}
	}
	return nil
}

// Write writes into an open write handle.
func (f *FS) Write(ctx context.Context, h *Handle, p []byte, off int64) (int, error) {
	// Writes count as foreground IO too: staging files live on the same disk
	// as the block cache, and a close() the kernel is blocked on is exactly
	// what background work must not queue behind.
	f.fgIO.Add(1)
	defer f.fgIO.Add(-1)
	h.mu.Lock()
	w := h.writer
	h.mu.Unlock()
	if w == nil {
		return 0, ErrReadOnly
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mode == config.ModeReadonly {
		return 0, ErrReadOnly
	}
	st, err := f.ensureStaging(ctx, h, w)
	if err != nil {
		return 0, err
	}
	n, err := st.WriteAt(p, off)
	if n > 0 {
		h.mu.Lock()
		w.dirty = true
		h.mu.Unlock()
	}
	return n, err
}

// Truncate resizes an open write handle.
// TruncatePath applies a truncate that arrived without a file handle of its
// own — truncate(2) by path, and the O_TRUNC the kernel performs when it has
// no descriptor to attach it to.
//
// It applies to the write handles already open on the inode when there are
// any. Each write handle owns a private staging file, so a truncate that made
// a handle of its own would commit a second, competing snapshot of the same
// inode: two uploads, and whichever landed last would win. For a rewrite that
// is a coin toss between the new content and the empty file the truncate
// produced, and the file's size after close(2) was decided by which commit
// happened to run second.
func (f *FS) TruncatePath(ctx context.Context, ino uint64, size int64) error {
	applied := 0
	for _, h := range f.writeHandles(ino) {
		ok, err := f.truncateHandle(ctx, h, size)
		if err != nil {
			return err
		}
		if ok {
			applied++
		}
	}
	if applied > 0 {
		return nil
	}
	h, err := f.Open(ctx, ino, true)
	if err != nil {
		return err
	}
	if _, err := f.truncateHandle(ctx, h, size); err != nil {
		f.Release(ctx, h)
		return err
	}
	return f.Release(ctx, h)
}

// writeHandles snapshots the open write handles for one inode.
func (f *FS) writeHandles(ino uint64) []*Handle { return f.writeHandlesFor(ino) }

func (f *FS) Truncate(ctx context.Context, h *Handle, size int64) error {
	applied, err := f.truncateHandle(ctx, h, size)
	if err != nil {
		return err
	}
	if !applied {
		return ErrReadOnly
	}
	return nil
}

// truncateHandle reports whether the handle could take the truncate. A handle
// closed between being listed and being used is not an error: the caller falls
// back to a handle of its own.
func (f *FS) truncateHandle(ctx context.Context, h *Handle, size int64) (bool, error) {
	h.mu.Lock()
	w := h.writer
	closed := h.closed
	h.mu.Unlock()
	if w == nil || closed {
		return false, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Truncating to nothing does not need the old content, and this is the
	// path every rewrite takes: the kernel turns O_TRUNC into a truncate,
	// and seeding the staging file first would download a file that is
	// about to be discarded.
	st, err := f.ensureStagingSeeded(ctx, h, w, size > 0)
	if err != nil {
		return false, err
	}
	if err := st.Truncate(size); err != nil {
		return false, err
	}
	h.mu.Lock()
	w.dirty = true
	h.mu.Unlock()
	return true, nil
}

func (w *writeState) readAt(buf []byte, off int64) (int, error) {
	n, err := w.staging.ReadAt(buf, off)
	if errors.Is(err, io.EOF) && n > 0 {
		return n, nil
	}
	return n, err
}

// commitWrite is the durability point: fsync the staging file, move it under
// its content hash, and commit the journal row. Only then does close() return.
// In strict mode it additionally waits for the upload to land remotely.
//
// Each step past the staging rename is recorded in w.pending, so a failure
// after it — the kernel sends another FLUSH, and the application retries the
// close — resumes instead of touching a staging file that no longer exists.
func (f *FS) commitWrite(ctx context.Context, h *Handle, w *writeState) error {
	if w.pending == nil {
		if w.staging == nil {
			return nil // committed already, nothing written since
		}
		if !w.dirty {
			return w.staging.Discard()
		}
		hashes, err := w.staging.Hashes()
		if err != nil {
			w.staging.Discard()
			return err
		}
		size := w.staging.Size()
		blob, err := f.journal.CommitStaging(w.staging, hashes)
		if err != nil {
			return err
		}
		u := journal.Upload{
			ID:              journal.NewID(),
			StagingID:       w.staging.ID,
			Remote:          h.Mount.Remote,
			RemoteParentID:  w.parentRemoteID,
			Name:            remoteName(w.name, h.Node.Kind),
			BlobPath:        blob,
			Size:            size,
			Hashes:          hashes,
			ExpectedVersion: w.expectedVersion,
			Ino:             h.Ino,
			NeedsPublish:    true,
		}
		binding, err := f.uploadBinding(ctx, h.Mount)
		if err != nil {
			return err
		}
		applyUploadBinding(&u, binding)
		// Hold the upload briefly before it becomes eligible. The kernel
		// sends a FLUSH per closed descriptor, so a single shell redirection
		// commits twice: once for the empty file it just created and once
		// for the content. Without this window both would be sent, which on
		// a rate-limited drive is a wasted request and an extra chance to
		// trip risk control. Strict mode skips the wait because its caller
		// is blocked on the upload landing.
		if w.mode != config.ModeStrict && f.opt.WriteSettle > 0 {
			u.NextRetryAt = f.now().Add(f.opt.WriteSettle)
		}
		w.pending = &pendingCommit{blob: blob, size: size, hashes: hashes, upload: u}
	}
	p := w.pending
	if !p.journaled {
		if err := f.journal.Commit(ctx, p.upload); err != nil {
			return err
		}
		p.journaled = true
		f.retargetIfParentLanded(ctx, p.upload.ID, w.parentIno, p.upload.RemoteParentID)
	}
	// Earlier snapshots of the same file that have not started uploading are
	// replaced by this one. Per inode rather than per handle: the kernel
	// truncates through a handle of its own, so the handle that writes the
	// content never sees the upload the truncate queued.
	_, _ = f.journal.DropSuperseded(ctx, h.Mount.Remote, h.Ino, p.upload.ID)
	w.lastUploadID = p.upload.ID
	// Reflect the new content locally straight away: the application must be
	// able to stat and read back what it just wrote, whether or not the
	// upload has finished. Until the upload lands, the node points at a
	// local-only key whose cache entry is a hard link to the committed blob,
	// and it is pinned so eviction cannot lose the only readable copy.
	node := h.Node
	node.Size = p.size
	node.MTime = f.now()
	node.Dirty = true
	if v, ok := p.hashes[provider.HashSHA1]; ok {
		node.HashType, node.Hash = string(provider.HashSHA1), v
	}
	oldKey := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	node.RemoteID = localRemoteID(p.upload.ID)
	node.Version = localVersion(p.upload.ID)
	// RemoteVersion deliberately stays as it was: it records what the provider
	// last had, which is what the next conflict check must compare against.
	if f.commitFault != nil {
		if err := f.commitFault(); err != nil {
			return err
		}
	}
	// Cache must be readable before metadata exposes its new local identity.
	// The publication gate keeps upload workers from completing this row
	// before the tree knows which committed version it represents.
	localKey := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	if err := f.cache.LinkPinnedFile(localKey, p.blob, p.size); err != nil {
		return err
	}
	// By inode, not by name: the file may have been renamed since the
	// handle was opened, and its name then is not where the data goes.
	//
	// barrier publishes as durably as power, not as cheaply as crash. The
	// journal row is the durable truth and RecoverPublications rebuilds this
	// from it — but MarkPublished, which closes that door, runs in its own
	// fsynced transaction afterwards. A publication weaker than the flag that
	// retires it could leave a power loss with "already published" recorded
	// and no node to show for it, and recovery would not replay it.
	update := f.meta.UpdateByIno
	if f.journal.Durability() != journal.DurabilityCrash {
		update = f.meta.PublishByIno
	}
	err := update(ctx, node)
	if errors.Is(err, meta.ErrNotFound) {
		// Unlinked while open. POSIX discards the data with the last
		// descriptor; here that means the queued upload must not land, or
		// the next listing would bring the file back.
		if derr := f.journal.DropPending(ctx, p.upload.ID); errors.Is(derr, journal.ErrInFlight) {
			_ = f.journal.Tombstone(ctx, p.upload.ID)
		}
		w.pending = nil
		f.cache.Pin(localKey, false)
		f.cache.Forget(localKey)
		return nil
	}
	if err != nil {
		return err
	}
	w.committed = &node
	f.changedNode(ctx, h.Ino, false, KindWrite)
	if err := f.journal.MarkPublished(ctx, p.upload.ID); err != nil {
		return err
	}
	if oldKey.RemoteID != "" && oldKey != localKey {
		// The previous version's blocks are stale now.
		f.cache.Forget(oldKey)
	}
	f.invalidateFrom(ctx, h.Ino)
	w.pending = nil

	if w.mode == config.ModeStrict {
		return f.flushUpload(ctx, p.upload.ID, h.Mount.Remote)
	}
	return nil
}

// flushUpload drives the uploader until the given upload leaves the queue.
// Strict mode uses it so close() means "durable remotely".
func (f *FS) flushUpload(ctx context.Context, uploadID, remote string) error {
	if f.uploader == nil {
		return errors.New("vfs: strict mode needs an uploader")
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u, err := f.journal.Get(ctx, uploadID)
		if err != nil {
			return err
		}
		switch u.State {
		case journal.StateDone:
			return nil
		case journal.StateDead:
			return fmt.Errorf("vfs: upload failed: %s", u.LastError)
		case journal.StateCancelling, journal.StateCancelled:
			return journal.ErrCancelled
		case journal.StatePurging:
			return journal.ErrUploadPurging
		}
		if _, err := f.uploader.DrainOnce(ctx, remote); err != nil {
			return err
		}
		// Nothing was due: wait for the retry timer rather than spinning.
		u2, err := f.journal.Get(ctx, uploadID)
		if err != nil {
			return err
		}
		if u2.State == u.State && u2.Attempt == u.Attempt {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// Sync commits an open write handle's data without closing it, so fsync(2)
// has the same durability meaning as close(2): the bytes are on local disk and
// journaled. In strict mode it also waits for the upload.
func (f *FS) Sync(ctx context.Context, h *Handle) error {
	h.mu.Lock()
	w := h.writer
	closed := h.closed
	h.mu.Unlock()
	if w == nil || closed {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dirty {
		return nil
	}
	// Commit the current contents, then continue writing into a fresh staging
	// file seeded from what was just committed.
	if err := f.commitWrite(ctx, h, w); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if w.committed != nil {
		h.Node = *w.committed
	}
	// The handle stays writable, but the fresh staging file it would write
	// into is made only when a write actually arrives: the kernel sends a
	// FLUSH per closed descriptor, and almost every file is closed right
	// after its first flush, so re-staging the committed blob here was
	// work thrown away for every small file.
	w.staging = nil
	w.dirty = false
	w.pending = nil
	return nil
}

// ensureStaging returns the handle's staging file, creating it on the first
// write. The check and the assignment are both under h.mu: go-fuse dispatches
// every request on its own goroutine, so two writes on one descriptor can
// otherwise both find it missing, both build one, and the loser's bytes land
// in a file nothing commits.
func (f *FS) ensureStaging(ctx context.Context, h *Handle, w *writeState) (*journal.Staging, error) {
	return f.ensureStagingSeeded(ctx, h, w, true)
}

func (f *FS) ensureStagingSeeded(ctx context.Context, h *Handle, w *writeState, seed bool) (*journal.Staging, error) {
	h.mu.Lock()
	if w.staging != nil {
		st := w.staging
		h.mu.Unlock()
		return st, nil
	}
	h.mu.Unlock()
	// The staging file is seeded from the node, so the node has to be the
	// current one. A handle caches what the file looked like when it was
	// opened, and the kernel implements O_TRUNC as a truncate through a
	// handle of its own: seeding from the stale snapshot copies the content
	// that truncate just removed back over the top of it, and the write that
	// follows commits a file with the old tail still attached.
	if fresh, err := f.meta.Get(ctx, h.Ino); err == nil {
		h.mu.Lock()
		h.Node = fresh
		h.mu.Unlock()
	}
	ws, err := f.newWriteState(ctx, h)
	if err != nil {
		return nil, err
	}
	st, err := f.openStaging(ctx, h, seed)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if w.staging != nil {
		// Another write got there first; keep its file and drop ours.
		st.Discard()
		return w.staging, nil
	}
	w.staging, w.name, w.parentIno, w.parentRemoteID = st, ws.name, ws.parentIno, ws.parentRemoteID
	w.expectedVersion = ws.expectedVersion
	return st, nil
}

// Create makes a new empty file and returns an open write handle.
func (f *FS) Create(ctx context.Context, parent uint64, name string) (*Handle, error) {
	return f.create(ctx, parent, name, provider.KindFile)
}

func (f *FS) create(ctx context.Context, parent uint64, name string, kind provider.Kind) (*Handle, error) {
	if err := checkLinkName(name); err != nil {
		return nil, err
	}
	m, parentPath, err := f.MountForIno(ctx, parent)
	if err != nil {
		return nil, err
	}
	if m.Mode == config.ModeReadonly {
		return nil, ErrReadOnly
	}
	if f.journal == nil {
		return nil, errors.New("vfs: no write backend configured")
	}
	if err := f.requireOwner(); err != nil {
		return nil, err
	}
	if _, err := f.lookupNode(ctx, parent, name); err == nil {
		return nil, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// lookupNode may refresh a directory, so acquire the publication gate
	// only afterwards. Recheck the parent before creating a protected node.
	f.remotePublishMu.RLock()
	defer f.remotePublishMu.RUnlock()
	p, err := f.meta.Get(ctx, parent)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !p.IsDir() {
		return nil, ErrNotDir
	}
	node := meta.Node{
		ParentIno: parent, Name: name, Kind: kind, Mode: 0o644,
		MTime: f.now(), Remote: m.Remote, TTL: f.opt.AttrTTL, Dirty: true,
	}
	if kind == provider.KindSymlink {
		node.Mode = 0o777
	}
	// Upsert clears the name's own negative-cache entry; the rest of the
	// parent's entries stay valid.
	node, err = f.meta.Insert(ctx, node)
	if errors.Is(err, meta.ErrExists) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	f.invalidateFrom(ctx, parent)
	_ = parentPath

	h := &Handle{Ino: node.Ino, Node: node, Mount: m, lastBlock: -1, window: 1}
	ws, err := f.newWriteState(ctx, h)
	if err != nil {
		return nil, err
	}
	// A create always produces a file, even if nothing is written to it.
	ws.dirty = true
	h.writer = ws
	f.mu.Lock()
	h.FH = f.nextFH
	f.nextFH++
	f.registerHandleLocked(h)
	readers := f.addWriterLocked(node.Ino, false)
	f.mu.Unlock()
	markReadersFollowing(readers)
	f.changedEntry(ctx, parent, name, false, KindCreate)
	return h, nil
}

// WriteFile is the path-based write used by MCP. It creates or replaces a file
// and returns once the write is journaled (or uploaded, in strict mode).
func (f *FS) WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (Attr, error) {
	dir, name := path.Split(path.Clean("/" + strings.TrimPrefix(p, "/")))
	dir = path.Clean(dir)
	if name == "" {
		return Attr{}, ErrIsDir
	}
	if err := f.requireOwner(); err != nil {
		return Attr{}, err
	}
	parent, err := f.resolve(ctx, dir)
	if err != nil {
		return Attr{}, err
	}
	if !parent.IsDir() {
		return Attr{}, ErrNotDir
	}
	var h *Handle
	existing, lerr := f.lookupNode(ctx, parent.Ino, name)
	switch {
	case lerr == nil:
		h, err = f.Open(ctx, existing.Ino, true)
		if err != nil {
			return Attr{}, err
		}
		if !appendMode {
			if err := f.Truncate(ctx, h, 0); err != nil {
				return Attr{}, err
			}
		}
	case errors.Is(lerr, ErrNotFound):
		h, err = f.Create(ctx, parent.Ino, name)
		if err != nil {
			return Attr{}, err
		}
	default:
		return Attr{}, lerr
	}
	off := int64(0)
	if appendMode {
		off = h.Node.Size
		if h.writer.staging != nil {
			off = h.writer.staging.Size()
		}
	}
	if _, err := f.Write(ctx, h, data, off); err != nil {
		f.Release(ctx, h)
		return Attr{}, err
	}
	if err := f.Release(ctx, h); err != nil {
		return Attr{}, err
	}
	return f.StatPath(ctx, p)
}

// Mkdir creates a directory. On a writeback mount it is a local commit the
// way close() is: the directory is in the tree and usable at once, and a
// queued row creates it on the backend — copying a source tree in no longer
// pays a round trip per directory. Until the row lands the node carries a
// local-only id, and everything written into the directory addresses that
// id; the queue holds those rows back until the directory exists and then
// points them at the real one. A strict mount creates the directory on the
// backend before returning, as it does for file content.
func (f *FS) Mkdir(ctx context.Context, parent uint64, name string) (Attr, error) {
	if err := checkLinkName(name); err != nil {
		return Attr{}, err
	}
	m, _, err := f.MountForIno(ctx, parent)
	if err != nil {
		return Attr{}, err
	}
	if m.Mode == config.ModeReadonly {
		return Attr{}, ErrReadOnly
	}
	if err := f.requireOwner(); err != nil {
		return Attr{}, err
	}
	if _, err := f.lookupNode(ctx, parent, name); err == nil {
		return Attr{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return Attr{}, err
	}
	if m.Mode == config.ModeWriteback && f.journal != nil {
		return f.mkdirQueued(ctx, m, parent, name)
	}
	if err := f.ensureRemoteDir(ctx, parent); err != nil {
		return Attr{}, err
	}
	parentNode, err := f.meta.Get(ctx, parent)
	if err != nil {
		return Attr{}, err
	}
	parentID := parentNode.RemoteID
	if parentID == "" {
		parentID = m.RootID
	}
	e, err := m.Provider.Mkdir(ctx, parentID, name)
	if err != nil {
		return Attr{}, mapProviderErr(err)
	}
	node := nodeFromEntry(m.Remote, e, f.opt.AttrTTL)
	node.ParentIno = parent
	// Born complete: the directory is empty, and the first lookup inside it
	// must not list an empty directory on the backend.
	node, err = f.meta.InsertCompleteDir(ctx, node)
	if errors.Is(err, meta.ErrExists) {
		return Attr{}, ErrExists
	}
	if err != nil {
		return Attr{}, err
	}
	_ = f.meta.ClearAbsent(ctx, parent)
	f.invalidateFrom(ctx, parent)
	f.changedEntry(ctx, parent, name, false, KindMkdir)
	return f.attrOf(ctx, node), nil
}

// mkdirQueued commits a directory locally and queues its creation, in the
// order a file commit uses: the node first (a row needs an inode to bind
// to), then the durable row, then the node's local identity, then the
// publication gate that lets workers take the row. A crash between any two
// steps leaves something recovery knows how to finish or discard.
func (f *FS) mkdirQueued(ctx context.Context, m Mount, parent uint64, name string) (Attr, error) {
	parentID, err := f.dirRemoteID(ctx, parent)
	if err != nil {
		return Attr{}, err
	}
	if parentID == "" {
		parentID = m.RootID
	}
	now := f.now()
	node := meta.Node{
		ParentIno: parent, Name: name, Kind: provider.KindDir, Mode: 0o755,
		Remote: m.Remote, MTime: now, FetchedAt: now, TTL: f.opt.AttrTTL, Dirty: true,
	}
	node, err = f.meta.InsertCompleteDir(ctx, node)
	if errors.Is(err, meta.ErrExists) {
		return Attr{}, ErrExists
	}
	if err != nil {
		return Attr{}, err
	}
	u := journal.Upload{
		ID: journal.NewID(), Kind: journal.KindMkdir, Remote: m.Remote,
		RemoteParentID: parentID, Name: name, Ino: node.Ino, NeedsPublish: true,
	}
	binding, err := f.uploadBinding(ctx, m)
	if err != nil {
		return Attr{}, err
	}
	applyUploadBinding(&u, binding)
	if err := f.journal.Commit(ctx, u); err != nil {
		_ = f.meta.Remove(ctx, node.Ino)
		return Attr{}, err
	}
	node.RemoteID = localRemoteID(u.ID)
	node.Version = localVersion(u.ID)
	update := f.meta.UpdateByIno
	if f.journal.Durability() != journal.DurabilityCrash {
		update = f.meta.PublishByIno
	}
	if err := update(ctx, node); err != nil {
		return Attr{}, err
	}
	if err := f.journal.MarkPublished(ctx, u.ID); err != nil {
		return Attr{}, err
	}
	f.retargetIfParentLanded(ctx, u.ID, parent, parentID)
	_ = f.meta.ClearAbsent(ctx, parent)
	f.invalidateFrom(ctx, parent)
	f.changedEntry(ctx, parent, name, false, KindMkdir)
	return f.attrOf(ctx, node), nil
}

// ensureRemoteDir creates on the backend, now, every queued directory on
// the path to ino, top-down, and returns once ino has a real id. It is the
// price an operation that needs a real id synchronously pays — a move of a
// backend entry into a queued directory, a server-side copy into one, a
// strict mount's mkdir under one. The queue's worker would get there on its
// own; this runs the rows in the caller's thread instead of waiting for it,
// and waits only for a row a worker has already taken.
func (f *FS) ensureRemoteDir(ctx context.Context, ino uint64) error {
	var chain []meta.Node // queued ancestors, bottom-up, ino first
	for cur := ino; ; {
		n, err := f.meta.Get(ctx, cur)
		if err != nil {
			return err
		}
		if !IsLocalOnly(n.RemoteID) {
			break
		}
		chain = append(chain, n)
		cur = n.ParentIno
	}
	if len(chain) == 0 {
		return nil
	}
	if f.journal == nil || f.uploader == nil {
		return errors.New("vfs: a queued directory needs the upload queue to be created")
	}
	for i := len(chain) - 1; i >= 0; i-- {
		rowID, _ := LocalUploadID(chain[i].RemoteID)
		row, err := f.journal.ClaimID(ctx, rowID)
		if err == nil {
			f.uploader.RunOne(ctx, row)
		} else if !errors.Is(err, journal.ErrInFlight) {
			return err
		}
		if err := f.awaitUpload(ctx, rowID); err != nil {
			return err
		}
	}
	return nil
}

// awaitUpload waits for a row to leave the queue without driving the queue
// itself: the row is running on this thread or on a worker, and taking other
// rows along would make a rename wait for unrelated uploads.
func (f *FS) awaitUpload(ctx context.Context, uploadID string) error {
	for {
		u, err := f.journal.Get(ctx, uploadID)
		if err != nil {
			return err
		}
		switch u.State {
		case journal.StateDone:
			return nil
		case journal.StateDead:
			return fmt.Errorf("vfs: creating the directory failed: %s", u.LastError)
		case journal.StateCancelling, journal.StateCancelled:
			return journal.ErrCancelled
		case journal.StatePurging:
			return journal.ErrUploadPurging
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// retargetIfParentLanded closes the gap between reading a parent's id and
// committing a row that addresses it. When the id was a queued directory's
// local id, that directory may have landed in between — after its landing
// rewrote the rows it knew about, and before this one existed. The row would
// never be claimable. The directory's adoption commits its real id before
// it rewrites its children, so a re-read now sees either the real id (and
// this row follows it) or the local id (and the rewrite, which has not run
// yet, will cover this row).
func (f *FS) retargetIfParentLanded(ctx context.Context, rowID string, parentIno uint64, addressed string) {
	if !IsLocalOnly(addressed) {
		return
	}
	p, err := f.meta.Get(ctx, parentIno)
	if err != nil || p.RemoteID == "" || IsLocalOnly(p.RemoteID) {
		return
	}
	_ = f.journal.RetargetParent(ctx, rowID, p.RemoteID)
}

// Remove deletes a file or an empty directory.
//
// On a writeback mount it is a local commit like close() and mkdir: the
// name is gone when the call returns, and a queued row removes the entry
// from the backend. On a strict mount the backend goes first, and that
// round trip runs outside the admission gate: every open on the mount
// passes through the gate, and a delete held it for the whole of a slow
// backend's reply, which stalled every other file for those seconds.
func (f *FS) Remove(ctx context.Context, parent uint64, name string, recursive bool) error {
	m, _, err := f.MountForIno(ctx, parent)
	if err != nil {
		return err
	}
	if m.Mode == config.ModeReadonly {
		return ErrReadOnly
	}
	if err := f.requireOwner(); err != nil {
		return err
	}
	// The lookup and rmdir's emptiness check come first, outside the gate:
	// either can need the backend — a name the tree has not cached, a
	// directory whose listing a delta poll marked stale — and remove itself
	// then finds everything local.
	n, err := f.lookupNode(ctx, parent, name)
	if err != nil {
		return err
	}
	if n.IsDir() && !recursive {
		if err := f.requireEmptyDir(ctx, n, !f.queuesDeletes(m)); err != nil {
			return err
		}
	}
	if f.queuesDeletes(m) {
		f.copyPublishMu.Lock()
		defer f.copyPublishMu.Unlock()
		return f.remove(ctx, m, parent, name, recursive, removeQueued, n.Ino, "")
	}
	if !IsLocalOnly(n.RemoteID) && n.RemoteID != "" {
		if err := m.Provider.Delete(ctx, n.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return mapProviderErr(err)
		}
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	return f.remove(ctx, m, parent, name, recursive, removeDone, n.Ino, "")
}

// removeMode says what remove does about the backend's copy.
type removeMode int

const (
	// removeQueued commits a delete row; the queue removes the entry.
	removeQueued removeMode = iota
	// removeNow asks the backend in this thread, under the gate. The
	// rename of a queued write uses it on a strict mount for the name it
	// is about to reuse.
	removeNow
	// removeDone: the backend was already asked, before the gate was
	// taken. Only the local side is left. Rename uses it for the name it
	// is about to reuse: the backend refuses to clobber, so the target
	// must be gone before the rename is sent, and that round trip is made
	// before the gate like Remove's.
	removeDone
)

// remove takes the name out of the tree. Caller holds copyPublishMu. ino,
// when set, is the node the caller looked up before taking the gate: a
// different node under the name now means the name moved on in between,
// and it is left alone.
func (f *FS) remove(ctx context.Context, m Mount, parent uint64, name string, recursive bool, mode removeMode, ino uint64, deleteOrderName string) error {
	n, err := f.lookupNode(ctx, parent, name)
	if err != nil {
		return err
	}
	if ino != 0 && n.Ino != ino {
		return nil
	}
	if n.IsDir() && !recursive {
		if err := f.requireEmptyDir(ctx, n, mode != removeQueued); err != nil {
			return err
		}
	}
	switch {
	case IsLocalOnly(n.RemoteID):
		if pending, err := f.copyAwaitingSubmit(ctx, n); err != nil {
			return err
		} else if pending {
			if err := f.journal.FailCopy(ctx, strings.TrimPrefix(n.RemoteID, localIDPrefix), "copy target was removed locally"); err != nil {
				return err
			}
		}
		if err := f.cancelQueued(ctx, n); err != nil {
			return err
		}
	case n.RemoteID == "" || mode == removeDone:
	case mode == removeNow:
		if err := m.Provider.Delete(ctx, n.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return mapProviderErr(err)
		}
	default:
		if n.IsDir() {
			// What the backend has under the directory goes first: its
			// row is held back until theirs have run, and the backend is
			// asked to confirm the directory is empty before it goes.
			if err := f.queueDeletesBelow(ctx, m, n); err != nil {
				return err
			}
		}
		parentID, err := f.dirRemoteID(ctx, parent)
		if err != nil {
			return err
		}
		if parentID == "" {
			parentID = m.RootID
		}
		if err := f.queueDelete(ctx, m, n, parentID, deleteOrderName); err != nil {
			return err
		}
	}
	if n.IsDir() {
		// Whatever is queued below a directory that is leaving the tree
		// has nowhere to land. A file under a queued directory would wait
		// for a parent that is never created; a file under a backend
		// directory would be sent to a parent that was just deleted.
		if err := f.cancelQueuedBelow(ctx, n.Ino); err != nil {
			return err
		}
	}
	f.cache.Forget(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
	if err := f.meta.Remove(ctx, n.Ino); err != nil {
		return err
	}
	f.dropPaths()
	f.invalidateFrom(ctx, parent)
	f.invalidateEntryFrom(ctx, parent, name)
	f.changedEntry(ctx, parent, name, n.IsDir(), KindRemove)
	return nil
}

// requireEmptyDir is rmdir's check. The tree's own listing answers when it
// is complete — whatever its age: rm -rf has just unlinked everything
// through this tree, and asking the backend again for each directory was a
// round trip per directory on the foreground path. A queued rmdir has the
// backend confirm before the directory goes; a synchronous one (askBackend)
// has no second chance and lists when the listing is not fresh.
func (f *FS) requireEmptyDir(ctx context.Context, n meta.Node, askBackend bool) error {
	var kids []meta.Node
	st, err := f.meta.DirState(ctx, n.Ino)
	if err != nil {
		return err
	}
	if st.Complete && !askBackend {
		kids, err = f.meta.Children(ctx, n.Ino)
	} else {
		kids, err = f.readDirRefresh(ctx, n.Ino, false)
	}
	if err != nil {
		return err
	}
	if len(kids) > 0 {
		return ErrNotEmpty
	}
	return nil
}

// cancelQueued takes a node's queued rows out of the queue: a write or a
// directory creation that was never sent is dropped rather than asking the
// backend to delete an id it does not have; one already on the wire is
// tombstoned so it finishes and is then deleted from the backend — dropping
// the row would leave the finished entry behind, a delete that resurrects
// its target.
func (f *FS) cancelQueued(ctx context.Context, n meta.Node) error {
	if f.journal == nil {
		return nil
	}
	pending, err := f.journal.ByIno(ctx, n.Ino)
	if err != nil {
		return err
	}
	for _, u := range pending {
		if u.State == journal.StateUploading {
			if err := f.journal.Tombstone(ctx, u.ID); err != nil && !errors.Is(err, journal.ErrNotFound) {
				return err
			}
			continue
		}
		err := f.journal.DropPending(ctx, u.ID)
		if errors.Is(err, journal.ErrInFlight) {
			// Claimed between our look and our drop: same as above.
			err = f.journal.Tombstone(ctx, u.ID)
		}
		if err != nil && !errors.Is(err, journal.ErrNotFound) {
			return err
		}
	}
	return nil
}

// cancelQueuedBelow cancels the queued rows of every local-only node under
// a directory that is about to leave the tree.
func (f *FS) cancelQueuedBelow(ctx context.Context, dir uint64) error {
	if f.journal == nil {
		return nil
	}
	kids, err := f.meta.Children(ctx, dir)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if IsLocalOnly(k.RemoteID) {
			if err := f.cancelQueued(ctx, k); err != nil {
				return err
			}
		}
		if k.IsDir() {
			if err := f.cancelQueuedBelow(ctx, k.Ino); err != nil {
				return err
			}
		}
	}
	return nil
}

// Rename moves a file or directory. Cross-remote moves are refused; the caller
// should copy instead.
//
// The backend's part — moving or renaming the entry — runs outside the
// admission gate every open passes through; only the tree's part is under
// it. See Remove.
func (f *FS) Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
	if err := f.rename(ctx, oldParent, oldName, newParent, newName); err != nil {
		return err
	}
	if oldParent != newParent || oldName != newName {
		f.changedRename(ctx, oldParent, oldName, newParent, newName)
	}
	return nil
}

func (f *FS) rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
	if err := checkLinkName(newName); err != nil {
		return err
	}
	srcMount, _, err := f.MountForIno(ctx, oldParent)
	if err != nil {
		return err
	}
	dstMount, _, err := f.MountForIno(ctx, newParent)
	if err != nil {
		return err
	}
	if srcMount.Remote != dstMount.Remote {
		return ErrCrossMount
	}
	if srcMount.Mode == config.ModeReadonly {
		return ErrReadOnly
	}
	if err := f.requireOwner(); err != nil {
		return err
	}
	n, err := f.lookupNode(ctx, oldParent, oldName)
	if err != nil {
		return err
	}
	if n.Kind == provider.KindSymlink && len(remoteName(newName, n.Kind)) > 255 {
		return syscall.ENAMETOOLONG
	}
	// A file that has not been uploaded yet exists only as a queued write.
	// Retarget that write instead of asking the provider about an id it has
	// never seen; this is the "write a temp file then rename it" pattern every
	// editor uses to save.
	if IsLocalOnly(n.RemoteID) {
		if pending, err := f.copyAwaitingSubmit(ctx, n); err != nil {
			return err
		} else if pending {
			return fmt.Errorf("vfs: copy publication is pending: %w", syscall.EBUSY)
		}
		f.copyPublishMu.Lock()
		inFlight, err := f.renameLocalOnly(ctx, n, dstMount, newParent, newName)
		f.copyPublishMu.Unlock()
		if err != nil {
			return err
		}
		if inFlight == "" {
			return nil
		}
		// The upload started between the lookup and the retarget, so it
		// lands under the old name. Wait for it — outside the gate, this
		// is a remote round trip — then rename the finished file on the
		// backend. Waiting is what the caller expects: rename(2) either
		// happens or reports why, it does not half-happen.
		if err := f.flushUpload(ctx, inFlight, n.Remote); err != nil {
			return err
		}
		fresh, err := f.meta.Get(ctx, n.Ino)
		if err != nil {
			return err
		}
		if IsLocalOnly(fresh.RemoteID) {
			return fmt.Errorf("vfs: %s finished uploading but has no remote id yet; retry in a moment", n.Name)
		}
		return f.rename(ctx, oldParent, n.Name, newParent, newName)
	}

	// rename(2) replaces the destination. The remotes refuse to clobber (the
	// drivers pass "do not overwrite" so nothing is lost silently), so the
	// decision is made here: remove the target first — on the backend too,
	// in this thread, since the rename about to be sent needs it gone.
	//
	// This is the one place cloudfs cannot be atomic the way a local rename
	// is. A failure between the delete and the rename leaves the destination
	// gone and the source still in place under its old name, so the data the
	// caller wanted to keep is never the thing that is lost.
	if victim, err := f.meta.Lookup(ctx, newParent, newName); err == nil && victim.Ino != n.Ino {
		if !IsLocalOnly(victim.RemoteID) && victim.RemoteID != "" {
			if err := dstMount.Provider.Delete(ctx, victim.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
				return mapProviderErr(err)
			}
		}
		f.copyPublishMu.Lock()
		err := f.remove(ctx, dstMount, newParent, newName, true, removeDone, victim.Ino, "")
		f.copyPublishMu.Unlock()
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	} else if err != nil && !errors.Is(err, meta.ErrNotFound) {
		return err
	}
	// On a backend whose ids are paths, the deletes queued under a
	// directory name paths the move would take away from under them.
	if err := f.settleDeletesUnder(ctx, srcMount, n.RemoteID); err != nil {
		return err
	}

	caps := srcMount.Provider.Capabilities()
	// The backend answers with the entry as it now stands. On a path-id
	// backend that answer carries a different id than the one we asked
	// about, and so does every descendant's — dropping it is how a renamed
	// directory's children end up addressed by a path that no longer exists.
	movedID := n.RemoteID
	if n.RemoteID != "" {
		if oldParent != newParent {
			if !caps.ServerMove {
				return provider.ErrUnsupported
			}
			// The backend cannot move an entry into a directory it does
			// not have yet; create the queued directory first.
			if err := f.ensureRemoteDir(ctx, newParent); err != nil {
				return err
			}
			parentNode, err := f.meta.Get(ctx, newParent)
			if err != nil {
				return err
			}
			targetID := parentNode.RemoteID
			if targetID == "" {
				targetID = dstMount.RootID
			}
			moved, err := srcMount.Provider.Move(ctx, movedID, targetID)
			if err != nil {
				return mapProviderErr(err)
			}
			if moved.ID != "" {
				movedID = moved.ID
			}
		}
		if oldName != newName {
			if !caps.ServerRename {
				return provider.ErrUnsupported
			}
			renamed, err := srcMount.Provider.Rename(ctx, movedID, remoteName(newName, n.Kind))
			if err != nil {
				return mapProviderErr(err)
			}
			if renamed.ID != "" {
				movedID = renamed.ID
			}
		}
	}
	// The backend has already moved the entry. Whatever the tree does now,
	// what is cached about both directories describes the old shape, so the
	// invalidation runs even on the error paths below — leaving stale
	// dentries behind is how a failed rename keeps answering with names the
	// backend no longer has.
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	defer func() {
		f.dropPaths()
		f.invalidateFrom(ctx, oldParent)
		f.invalidateFrom(ctx, newParent)
		// Both names: the old one is now a stale positive dentry, and the new
		// one may be a cached negative lookup from before the move.
		f.invalidateEntryFrom(ctx, oldParent, oldName)
		f.invalidateEntryFrom(ctx, newParent, newName)
	}()
	if movedID != n.RemoteID {
		// Descendants follow only where the id is a path. An opaque id that
		// changed says nothing about the ids beneath it. The new name and the
		// new ids are one fact about the tree: committed separately, a reader
		// in between finds the directory under its new name with its children
		// still addressed by a path the backend no longer has, and a crash
		// there makes that permanent.
		return f.meta.RenameAndRetarget(ctx, n.Ino, newParent, newName, movedID, caps.PathIDs)
	}
	return f.meta.Rename(ctx, n.Ino, newParent, newName)
}

// localIDPrefix marks a node whose only copy is the locally committed blob.
// Reads resolve it through the cache; the provider is never asked for it.
const localIDPrefix = journal.LocalIDPrefix

func localRemoteID(uploadID string) string { return localIDPrefix + uploadID }
func localVersion(uploadID string) string  { return "local-" + uploadID }

// IsLocalOnly reports whether a node has not been uploaded yet.
func IsLocalOnly(remoteID string) bool { return strings.HasPrefix(remoteID, localIDPrefix) }

// LocalUploadID returns the journal upload whose blob is the only copy of
// this node, for readers outside this package that have to reach the bytes
// without asking a backend that does not have them yet.
func LocalUploadID(remoteID string) (string, bool) {
	if !IsLocalOnly(remoteID) {
		return "", false
	}
	return strings.TrimPrefix(remoteID, localIDPrefix), true
}

// renameLocalOnly moves a file whose only copy is the queued write. It
// retargets the pending upload, replaces any file already at the destination,
// and updates the tree. Caller holds copyPublishMu. When the upload is
// already on the wire it cannot be retargeted; the row's id comes back as
// inFlight and nothing has been done — the caller waits for it, without the
// gate, and renames the finished file instead.
func (f *FS) renameLocalOnly(ctx context.Context, n meta.Node, dst Mount, newParent uint64, newName string) (inFlight string, err error) {
	if f.journal == nil {
		return "", errors.New("vfs: no write backend configured")
	}
	parentNode, err := f.meta.Get(ctx, newParent)
	if err != nil {
		return "", err
	}
	// A queued directory's local id is a valid destination: the row waits
	// for the directory the way every row under it does, and follows it to
	// its real id when it lands.
	targetParentID := parentNode.RemoteID
	if targetParentID == "" {
		targetParentID = dst.RootID
	}

	pending, err := f.journal.ByIno(ctx, n.Ino)
	if err != nil {
		return "", err
	}
	for _, u := range pending {
		err := f.journal.Retarget(ctx, u.ID, targetParentID, remoteName(newName, n.Kind))
		if err == nil {
			f.retargetIfParentLanded(ctx, u.ID, newParent, targetParentID)
			continue
		}
		if !errors.Is(err, journal.ErrNotFound) {
			return "", err
		}
		// The upload started between our snapshot and now. Retargeting a
		// row on the wire is not possible; the caller waits for it.
		return u.ID, nil
	}

	// A file already at the destination is replaced, matching rename(2).
	// The queued write goes out by name and the queue holds it behind the
	// victim's delete, so on a writeback mount the victim's removal is a
	// local commit too: an editor's save-by-rename never waits for the
	// backend.
	if victim, err := f.meta.Lookup(ctx, newParent, newName); err == nil && victim.Ino != n.Ino {
		mode := removeNow
		if f.queuesDeletes(dst) {
			mode = removeQueued
		}
		// Delete rows order later writes by Name. A file and a symlink with the
		// same virtual name have different wire names, so use the incoming row's
		// wire name as the ordering key while deletion itself still addresses the
		// victim by RemoteID.
		orderName := remoteName(newName, n.Kind)
		if err := f.remove(ctx, dst, newParent, newName, true, mode, victim.Ino, orderName); err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
	} else if err != nil && !errors.Is(err, meta.ErrNotFound) {
		return "", err
	}

	if err := f.meta.Rename(ctx, n.Ino, newParent, newName); err != nil {
		return "", err
	}
	f.dropPaths()
	f.invalidateFrom(ctx, n.ParentIno)
	f.invalidateFrom(ctx, newParent)
	return "", nil
}

// UploadHooks builds the callbacks the uploader needs so a completed upload
// updates the tree and seeds the read cache from the local blob.
// publishFaultAt is a test seam at the boundaries of publishing an upload's
// result, matching uploadCleanupFault. Production leaves the hook nil.
func (f *FS) publishFaultAt(phase string) error {
	if f.publishFault != nil {
		return f.publishFault(phase)
	}
	return nil
}

func (f *FS) UploadHooks() upload.Hooks {
	return upload.Hooks{
		Authorize: f.validateUploadBinding,
		OnSuccess: func(ctx context.Context, u journal.Upload, r upload.Result) error {
			if u.IsDelete() {
				// The backend has let go of what the tree let go of
				// earlier; listings may show whatever is there now.
				f.deleting.drop(u.Remote, u.RemoteID)
				return nil
			}
			if r.Entry.ID == "" {
				// The provider acknowledged the upload without describing the
				// resulting file (some rapid-upload paths do this). Mark the
				// parent stale so the next listing picks up the real id; the
				// local-only cache entry stays until then so reads keep working.
				if u.Ino != 0 {
					if n, err := f.meta.Get(ctx, u.Ino); err == nil {
						_ = f.meta.Invalidate(ctx, n.ParentIno)
						f.invalidate(n.ParentIno)
						f.changedNode(ctx, n.ParentIno, true, KindRemote)
					}
				}
				return nil
			}
			if u.Ino == 0 {
				return nil
			}
			n, err := f.meta.Get(ctx, u.Ino)
			if errors.Is(err, meta.ErrNotFound) {
				if u.IsMkdir() {
					// The directory is gone locally but the rows queued under
					// it may not be; they still have somewhere to go.
					return f.journal.RetargetChildren(ctx, localRemoteID(u.ID), r.Entry.ID)
				}
				return nil // deleted while uploading
			}
			if err != nil {
				return err
			}
			if u.IsMkdir() {
				return f.adoptDirectory(ctx, u, r, n)
			}
			// Everything below decides what to write from the row just read. A
			// newer write can commit in between, which is what the seam lets a
			// test place deterministically.
			if err := f.publishFaultAt("upload-result-read"); err != nil {
				return err
			}
			if r.ConflictName != "" {
				// The data landed beside the remote file under a conflict
				// name. Release the local-only cache entry and mark the parent
				// stale: the next listing restores this node to the remote
				// version and surfaces the conflict copy next to it.
				localKey := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
				if IsLocalOnly(localKey.RemoteID) {
					f.cache.Pin(localKey, false)
					f.cache.Forget(localKey)
				}
				_ = f.meta.Invalidate(ctx, n.ParentIno)
				f.invalidate(n.ParentIno)
				f.changedNode(ctx, n.ParentIno, true, KindRemote)
				return nil
			}
			if n.RemoteID != localRemoteID(u.ID) {
				// The node has moved on: a later flush — the truncate a
				// rewrite starts with, say — committed while this upload was
				// in flight, and the node points at that content now.
				// Adopting the entry would put the superseded size back on a
				// file that no longer has it. Only the remote version is
				// taken, and it must be: it is what the next upload of this
				// file claims to have started from, and leaving it stale
				// makes our own write look like someone else's change and
				// land as a conflict copy.
				// Only the remote version, and only that column: the rest of
				// the row was read before the newer commit landed, and writing
				// it back would put the superseded content's size and identity
				// on a file that has moved past it.
				if err := f.meta.SetRemoteVersion(ctx, n.Ino, r.Entry.Version); err != nil && !errors.Is(err, meta.ErrNotFound) {
					return err
				}
				f.cache.Pin(cache.FileKey{Remote: n.Remote, RemoteID: localRemoteID(u.ID),
					Version: localVersion(u.ID)}, false)
				return nil
			}
			localKey := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
			n.RemoteID = r.Entry.ID
			n.Version = r.Entry.Version
			n.RemoteVersion = r.Entry.Version
			n.Size = r.Entry.Size
			if !r.Entry.ModTime.IsZero() {
				n.MTime = r.Entry.ModTime
			}
			n.Dirty = false
			for _, ht := range []provider.HashType{provider.HashSHA1, provider.HashMD5, provider.HashSHA256} {
				if v, ok := r.Entry.Hashes[ht]; ok && v != "" {
					n.HashType, n.Hash = string(ht), v
					break
				}
			}
			// The blob we just uploaded is exactly the file's content: install
			// it under the real key before the node points there, so no read
			// can land in the gap between the two, then drop the local-only
			// entry that covered it.
			key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
			if _, err := f.protectPinned(ctx, n); err != nil {
				return err
			}
			if _, err := os.Stat(u.BlobPath); err == nil {
				if err := f.cache.LinkFile(key, u.BlobPath, n.Size); err != nil {
					return err
				}
			}
			// By inode: a rename since the upload was queued must not turn
			// this into an insert under the old name. Gone means deleted
			// while uploading; the tombstone path handles the remote copy.
			//
			// Conditional on the identity this upload was published under: the
			// check above read the node, and a newer write can commit between
			// that read and this write. Without the condition the completing
			// upload puts its own — now superseded — size, id and version back
			// on the file, which is how two consecutive rewrites could end up
			// reporting the first one's content.
			adopted, err := f.meta.AdoptByIno(ctx, n, localRemoteID(u.ID), f.journal.Durability() != journal.DurabilityCrash)
			if err != nil {
				return err
			}
			if !adopted {
				// Either the node is gone, or it moved on while we were
				// looking. Both are the "already superseded" case: record what
				// the remote now holds and leave the tree alone.
				if err := f.meta.SetRemoteVersion(ctx, n.Ino, r.Entry.Version); err != nil && !errors.Is(err, meta.ErrNotFound) {
					return err
				}
				f.cache.Pin(cache.FileKey{Remote: n.Remote, RemoteID: localRemoteID(u.ID),
					Version: localVersion(u.ID)}, false)
				return nil
			}
			// The node now points at the remote file. Everything a reader
			// needs under the new key had to be in place before this line;
			// the seam is where a test observes that from the outside.
			if err := f.publishFaultAt("remote-identity"); err != nil {
				return err
			}
			if IsLocalOnly(localKey.RemoteID) {
				f.cache.Pin(localKey, false)
				f.cache.Forget(localKey)
			}
			// The bytes were written locally, but what changed here is the
			// node's identity: it now names the remote's file and version.
			f.invalidate(n.Ino)
			f.changedNode(ctx, n.Ino, false, KindRemote)
			return nil
		},
		Exists: func(ctx context.Context, u journal.Upload) bool {
			if u.Ino == 0 {
				return true
			}
			n, err := f.meta.Get(ctx, u.Ino)
			if errors.Is(err, meta.ErrNotFound) {
				return false
			}
			if err != nil {
				return true // unknown: treat it as still here
			}
			// The node may still be in the tree while the directory it
			// lives in is gone; the parent is what the upload addresses.
			if n.ParentIno != 0 {
				if _, perr := f.meta.Get(ctx, n.ParentIno); errors.Is(perr, meta.ErrNotFound) {
					return false
				}
			}
			return true
		},
		RemoteVersion: func(ctx context.Context, u journal.Upload) (string, bool) {
			if u.Ino == 0 {
				return "", false
			}
			n, err := f.meta.Get(ctx, u.Ino)
			if err != nil {
				return "", false
			}
			return n.RemoteVersion, true
		},
		OnDead: func(ctx context.Context, u journal.Upload, cause error) {
			if u.IsDelete() {
				// The entry is staying on the backend — a directory that
				// gained someone else's files, say. The tree should show it
				// again rather than keep hiding a name the backend has:
				// stop filtering it and have the parent listed afresh.
				f.deleting.drop(u.Remote, u.RemoteID)
				if p, ok := f.nodeForRemoteDir(ctx, u.Remote, u.RemoteParentID); ok {
					_ = f.meta.Invalidate(ctx, p.Ino)
					f.invalidate(p.Ino)
					f.changedNode(ctx, p.Ino, true, KindRemote)
				}
				return
			}
			// Leave the local node dirty: the data is still on disk and the
			// operator can requeue it. Status surfaces the failure.
			if u.Ino != 0 {
				f.invalidate(u.Ino)
			}
		},
	}
}

// adoptDirectory gives a queued directory the id the backend made for it,
// then points every row queued under its local id at the real one. The
// order matters twice over: a row committed against the local id while this
// runs re-reads the parent after committing, and it must find the real id
// there whenever the rewrite has already passed it by; and a crash between
// the two is healed by the retry, which finds the directory existing,
// adopts it again (a no-op) and rewrites again.
func (f *FS) adoptDirectory(ctx context.Context, u journal.Upload, r upload.Result, n meta.Node) error {
	if n.IsDir() {
		n.RemoteID = r.Entry.ID
		n.Version = r.Entry.Version
		n.RemoteVersion = r.Entry.Version
		n.Size = 0
		if !r.Entry.ModTime.IsZero() {
			n.MTime = r.Entry.ModTime
		}
		n.Dirty = false
		adopted, err := f.meta.AdoptByIno(ctx, n, localRemoteID(u.ID), f.journal.Durability() != journal.DurabilityCrash)
		if err != nil {
			return err
		}
		if err := f.publishFaultAt("directory-adopted"); err != nil {
			return err
		}
		if err := f.journal.RetargetChildren(ctx, localRemoteID(u.ID), r.Entry.ID); err != nil {
			return err
		}
		if err := f.publishFaultAt("directory-children-retargeted"); err != nil {
			return err
		}
		if !adopted {
			return nil
		}
		if r.Merged {
			// The backend had this directory already, with whatever is in
			// it; the empty listing the tree holds is not its contents.
			_ = f.meta.Invalidate(ctx, n.Ino)
		}
		f.invalidate(n.Ino)
		f.changedNode(ctx, n.Ino, false, KindRemote)
		return nil
	}
	return f.journal.RetargetChildren(ctx, localRemoteID(u.ID), r.Entry.ID)
}

// RepairLost detaches the tree from uploads whose blobs recovery found
// missing or torn. Each such node pointed at a local-only key whose data is
// gone, so every read of it would fail; removing the node and staling the
// parent listing brings back the remote version on the next readdir, or
// nothing at all for a file the remote never had. The dead letter keeps
// whatever bytes remain, with the reason, for `cloudfs uploads`.
func (f *FS) RepairLost(ctx context.Context, j *journal.Journal, ids []string) {
	for _, id := range ids {
		u, err := j.Get(ctx, id)
		if err != nil {
			continue
		}
		node, err := f.meta.Get(ctx, u.Ino)
		if err != nil || node.RemoteID != localRemoteID(id) {
			continue // the node has moved on to a later write
		}
		f.cache.Forget(cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version})
		_ = f.meta.Remove(ctx, node.Ino)
		f.dropPaths()
		_ = f.meta.Invalidate(ctx, node.ParentIno)
		f.invalidate(node.ParentIno)
		// The name is gone from the tree; that it went because the bytes
		// were lost rather than by request does not change what a
		// subscriber sees. Recovery has no requester, so the origin is the
		// remote fallback.
		f.changedEntry(ctx, node.ParentIno, node.Name, node.IsDir(), KindRemove)
	}
}
