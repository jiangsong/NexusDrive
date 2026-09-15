package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
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
func (f *FS) writeHandles(ino uint64) []*Handle {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Handle
	for _, h := range f.handles {
		if h.Ino == ino && h.writer != nil {
			out = append(out, h)
		}
	}
	return out
}

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
			Name:            w.name,
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
	update := f.meta.UpdateByIno
	if f.journal.Durability() == journal.DurabilityPower {
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
	if w == nil || closed || !w.dirty {
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
		ParentIno: parent, Name: name, Kind: provider.KindFile, Mode: 0o644,
		MTime: f.now(), Remote: m.Remote, TTL: f.opt.AttrTTL, Dirty: true,
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
	f.handles[h.FH] = h
	f.addWriterLocked(node.Ino)
	f.mu.Unlock()
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

// Mkdir creates a directory on the remote and in the tree.
func (f *FS) Mkdir(ctx context.Context, parent uint64, name string) (Attr, error) {
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
	node, err = f.meta.Upsert(ctx, node)
	if err != nil {
		return Attr{}, err
	}
	_ = f.meta.ClearAbsent(ctx, parent)
	f.invalidateFrom(ctx, parent)
	f.changedEntry(ctx, parent, name, false, KindMkdir)
	return f.attrOf(ctx, node), nil
}

// Remove deletes a file or an empty directory.
func (f *FS) Remove(ctx context.Context, parent uint64, name string, recursive bool) error {
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	return f.remove(ctx, parent, name, recursive)
}

func (f *FS) remove(ctx context.Context, parent uint64, name string, recursive bool) error {
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
	n, err := f.lookupNode(ctx, parent, name)
	if err != nil {
		return err
	}
	if n.IsDir() && !recursive {
		kids, err := f.readDirRefresh(ctx, n.Ino, false)
		if err != nil {
			return err
		}
		if len(kids) > 0 {
			return ErrNotEmpty
		}
	}
	if IsLocalOnly(n.RemoteID) {
		if pending, err := f.copyAwaitingSubmit(ctx, n); err != nil {
			return err
		} else if pending {
			if err := f.journal.FailCopy(ctx, strings.TrimPrefix(n.RemoteID, localIDPrefix), "copy target was removed locally"); err != nil {
				return err
			}
		}
		// Never uploaded: cancel the queued write rather than asking the
		// provider to delete an id it does not have.
		if f.journal != nil {
			pending, err := f.journal.ByIno(ctx, n.Ino)
			if err != nil {
				return err
			}
			for _, u := range pending {
				if u.State == journal.StateUploading {
					// Already on the wire: let it finish, then have the
					// uploader delete it from the backend. Dropping the row
					// here would leave the finished file behind — a delete
					// that resurrects its target.
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
		}
	} else if n.RemoteID != "" {
		if err := m.Provider.Delete(ctx, n.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return mapProviderErr(err)
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

// Rename moves a file or directory. Cross-remote moves are refused; the caller
// should copy instead.
func (f *FS) Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	if err := f.rename(ctx, oldParent, oldName, newParent, newName); err != nil {
		return err
	}
	if oldParent != newParent || oldName != newName {
		f.changedRename(ctx, oldParent, oldName, newParent, newName)
	}
	return nil
}

func (f *FS) rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
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
		return f.renameLocalOnly(ctx, n, dstMount, newParent, newName, oldParent)
	}

	// rename(2) replaces the destination. The remotes refuse to clobber (the
	// drivers pass "do not overwrite" so nothing is lost silently), so the
	// decision is made here: remove the target first.
	//
	// This is the one place cloudfs cannot be atomic the way a local rename
	// is. A failure between the delete and the rename leaves the destination
	// gone and the source still in place under its old name, so the data the
	// caller wanted to keep is never the thing that is lost.
	if victim, err := f.meta.Lookup(ctx, newParent, newName); err == nil && victim.Ino != n.Ino {
		if err := f.remove(ctx, newParent, newName, true); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	} else if err != nil && !errors.Is(err, meta.ErrNotFound) {
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
			renamed, err := srcMount.Provider.Rename(ctx, movedID, newName)
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
const localIDPrefix = "cloudfs-local:"

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
// and updates the tree.
func (f *FS) renameLocalOnly(ctx context.Context, n meta.Node, dst Mount, newParent uint64, newName string, oldParent uint64) error {
	if f.journal == nil {
		return errors.New("vfs: no write backend configured")
	}
	parentNode, err := f.meta.Get(ctx, newParent)
	if err != nil {
		return err
	}
	targetParentID := parentNode.RemoteID
	if targetParentID == "" || IsLocalOnly(targetParentID) {
		targetParentID = dst.RootID
	}

	pending, err := f.journal.ByIno(ctx, n.Ino)
	if err != nil {
		return err
	}
	for _, u := range pending {
		err := f.journal.Retarget(ctx, u.ID, targetParentID, newName)
		if err == nil {
			continue
		}
		if !errors.Is(err, journal.ErrNotFound) {
			return err
		}
		// The upload started between our snapshot and now, so it will land
		// under the old name. Wait for it, then fall back to a server-side
		// rename of the finished file. Waiting is what the caller expects:
		// rename(2) either happens or reports why, it does not half-happen.
		if err := f.flushUpload(ctx, u.ID, u.Remote); err != nil {
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

	// A file already at the destination is replaced, matching rename(2).
	if victim, err := f.meta.Lookup(ctx, newParent, newName); err == nil && victim.Ino != n.Ino {
		if err := f.remove(ctx, newParent, newName, true); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	} else if err != nil && !errors.Is(err, meta.ErrNotFound) {
		return err
	}

	if err := f.meta.Rename(ctx, n.Ino, newParent, newName); err != nil {
		return err
	}
	f.dropPaths()
	f.invalidateFrom(ctx, oldParent)
	f.invalidateFrom(ctx, newParent)
	return nil
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
				return nil // deleted while uploading
			}
			if err != nil {
				return err
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
			adopted, err := f.meta.AdoptByIno(ctx, n, localRemoteID(u.ID), f.journal.Durability() == journal.DurabilityPower)
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
			// Leave the local node dirty: the data is still on disk and the
			// operator can requeue it. Status surfaces the failure.
			if u.Ino != 0 {
				f.invalidate(u.Ino)
			}
		},
	}
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
