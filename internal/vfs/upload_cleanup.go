package vfs

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

var ErrUploadCleanupTarget = errors.New("vfs: upload cleanup target, original metadata or writable mount changed")
var ErrUploadCleanupBusy = fmt.Errorf("vfs: retained upload content has an open handle: %w", syscall.EBUSY)

// DiscardUploadOffline uses the same coordinator against exclusively owned
// local stores, without constructing a provider or modifying mount metadata.
// The caller must not have another VFS using these stores in this process.
// No recovery of unrelated uploads or publications is performed.
func DiscardUploadOffline(ctx context.Context, opt Options, j *journal.Journal, id string, confirm bool) error {
	if !confirm || j == nil || !j.Owner() {
		return errors.New("vfs: offline discard requires confirmation and exclusive journal ownership")
	}
	opt.PrefetchDepth, opt.ReadAheadBlocks = 0, 0
	f, err := newFS(opt, true)
	if err != nil {
		return err
	}
	defer f.Close()
	f.SetWriteBackend(j, nil)
	return f.DiscardUpload(ctx, id, true)
}

// DiscardUpload irreversibly removes a stopped upload's local version and
// private bookkeeping. It does not cancel transfers or undo remote operations.
// A first request requires the retained current version; after an authorized
// purging intent, retries may finish an already-deleted version. No provider IO
// is performed. Management callers must require explicit confirmation.
func (f *FS) DiscardUpload(ctx context.Context, id string, confirm bool) error {
	if !confirm {
		return errors.New("vfs: upload discard requires explicit confirmation of local data loss")
	}
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: upload cleanup requires journal ownership")
	}
	// Open/read admission and namespace/copy/resume publication share this
	// gate. Create/remote refresh admission is excluded by the second gate.
	// Existing writers remain registered until their final commit returns.
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	f.remotePublishMu.Lock()
	defer f.remotePublishMu.Unlock()
	u, err := f.journal.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.State != journal.StateCancelled && u.State != journal.StatePurging || u.Tombstone || u.NeedsPublish || u.Ino == 0 {
		return journal.ErrUploadCleanupState
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return err
	}
	if u.State == journal.StatePurging {
		original, err := f.journal.UploadCleanupIdentity(ctx, id)
		if err != nil {
			return err
		}
		if original != identity {
			return journal.ErrUploadCleanupIdentity
		}
	}
	nodes, err := f.uploadCleanupNodes(ctx, u, identity)
	if err != nil {
		return err
	}
	key := cache.FileKey{Remote: u.Remote, RemoteID: localRemoteID(id), Version: localVersion(id)}
	inos := map[uint64]bool{u.Ino: true}
	for _, n := range nodes {
		inos[n.Ino] = true
	}
	f.mu.Lock()
	busy := false
	for ino := range inos {
		busy = busy || f.writers[ino] > 0
	}
	for _, h := range f.handles {
		if inos[h.Ino] || h.openedKey == key {
			busy = true
			break
		}
	}
	f.mu.Unlock()
	if busy {
		return ErrUploadCleanupBusy
	}
	if _, err := f.journal.BeginUploadCleanup(ctx, id, identity); err != nil {
		return err
	}
	if err := f.uploadCleanupBoundary("intent"); err != nil {
		return err
	}
	if err := f.meta.RemoveLocalVersion(ctx, meta.LocalVersionCleanup{
		StoreIdentity: identity, Remote: key.Remote, RemoteID: key.RemoteID,
		Version: key.Version, Size: u.Size, Nodes: nodes,
	}); err != nil {
		return err
	}
	// Metadata absence is visible even if subsequent cache or journal cleanup
	// fails. Notify now and retain purging for a later local-only retry.
	f.dropPaths()
	for _, n := range nodes {
		f.invalidateFrom(ctx, n.Ino)
		f.invalidateFrom(ctx, n.ParentIno)
		// Discarding the local version removes every name that carried it;
		// the requester (control, MCP, or recovery) is in ctx.
		f.changedEntry(ctx, n.ParentIno, n.Name, false, KindRemove)
	}
	f.wakePins()
	if err := f.uploadCleanupBoundary("metadata"); err != nil {
		return err
	}
	f.cache.Pin(key, false)
	f.cache.SetUserPin(key, false) // persistent path rules themselves remain
	if err := f.cache.ForgetChecked(key); err != nil {
		return err
	}
	if err := f.uploadCleanupBoundary("cache"); err != nil {
		return err
	}
	return f.journal.FinishUploadCleanup(ctx, id, identity)
}

func (f *FS) uploadCleanupBoundary(phase string) error {
	if f.uploadCleanupFault != nil {
		return f.uploadCleanupFault(phase)
	}
	return nil
}

// All checks are local, under publication gates: resolving a cold provider
// directory here could deadlock remote publication and is not authorization.
func (f *FS) uploadCleanupNodes(ctx context.Context, u journal.Upload, identity string) ([]meta.Node, error) {
	nodes, err := f.meta.Aliases(ctx, u.Remote, localRemoteID(u.ID))
	if err != nil {
		return nil, err
	}
	foundOriginal := false
	for _, n := range nodes {
		if n.IsDir() || !n.Dirty || n.Version != localVersion(u.ID) || n.Size != u.Size || remoteName(n.Name, n.Kind) != u.Name {
			return nil, ErrUploadCleanupTarget
		}
		p, err := f.meta.Path(ctx, n.Ino)
		if err != nil {
			return nil, err
		}
		m, ok := f.mountFor(p)
		if !ok || m.Remote != u.Remote {
			return nil, ErrUploadCleanupTarget
		}
		if m.Mode == config.ModeReadonly {
			return nil, ErrReadOnly
		}
		parent, err := f.meta.Get(ctx, n.ParentIno)
		if err != nil {
			return nil, err
		}
		parentPath, err := f.meta.Path(ctx, parent.Ino)
		if err != nil {
			return nil, err
		}
		parentID := parent.RemoteID
		if parentPath == m.Prefix {
			parentID = m.RootID
		}
		if !parent.IsDir() || parentID != u.RemoteParentID {
			return nil, ErrUploadCleanupTarget
		}
		foundOriginal = foundOriginal || n.Ino == u.Ino
	}
	if u.State == journal.StateCancelled && !foundOriginal {
		return nil, ErrUploadCleanupTarget
	}
	if job, err := f.journal.GetCopy(ctx, u.ID); err == nil {
		m, ok := f.mountFor(job.Spec.TargetPath)
		if job.Spec.MetaIdentity != identity || !ok || m.Prefix != job.Spec.TargetMount ||
			m.RootID != job.Spec.TargetRootID || m.Remote != job.Spec.TargetRemote {
			return nil, ErrUploadCleanupTarget
		}
		if m.Mode == config.ModeReadonly {
			return nil, ErrReadOnly
		}
	} else if !errors.Is(err, journal.ErrNotFound) {
		return nil, err
	}
	return nodes, nil
}

// RecoverUploadCleanups resumes only previously confirmed intents, before
// publication recovery or accepting requests. Failure preserves the intent and
// stops startup; it must not enable upload replay or discard through a new DB.
func (f *FS) RecoverUploadCleanups(ctx context.Context) error {
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: cleanup recovery requires journal ownership")
	}
	cursor := ""
	for {
		rows, next, err := f.journal.ListActive(ctx, cursor, 200)
		if err != nil {
			return err
		}
		for _, u := range rows {
			if u.State == journal.StatePurging {
				if err := f.DiscardUpload(ctx, u.ID, true); err != nil {
					return fmt.Errorf("vfs: pending local upload cleanup: %w", err)
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}
