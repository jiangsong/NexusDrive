package vfs

import (
	"context"
	"errors"
	"path"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// trackCopy is installed only after acquiring the journal preparation handle,
// so a competing resume cannot replace the foreground operation's cancel func.
func (f *FS) trackCopy(ctx context.Context, id string) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	f.copyRunsMu.Lock()
	if f.copyRuns == nil {
		f.copyRuns = map[string]context.CancelFunc{}
	}
	if _, exists := f.copyRuns[id]; exists {
		f.copyRunsMu.Unlock()
		cancel()
		return nil, nil, journal.ErrCopyBusy
	}
	f.copyRuns[id] = cancel
	f.copyRunsMu.Unlock()
	finish := func() {
		cancel()
		f.copyRunsMu.Lock()
		delete(f.copyRuns, id)
		f.copyRunsMu.Unlock()
	}
	job, err := f.journal.GetCopy(ctx, id)
	if err == nil && job.State == journal.CopyCancelled {
		err = journal.ErrCopyCancelled
	}
	if err != nil {
		finish()
		return nil, nil, err
	}
	return ctx, finish, nil
}

// ForgetCopy discards terminal history and unreferenced prepared bytes only.
// It never deletes a local/remote file or cancels an active transfer. Retained
// copy_bindings tombstones remain to prevent revival by a stale journal backup.
func (f *FS) ForgetCopy(ctx context.Context, id string) error {
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: copy cleanup requires journal ownership")
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	job, err := f.journal.GetCopy(ctx, id)
	if err != nil {
		return err
	}
	if job.State != journal.CopyFailed && job.State != journal.CopyCancelled && job.State != journal.CopySubmitted && job.State != journal.CopyPurging {
		return journal.ErrCopyState
	}
	key := cache.FileKey{Remote: job.Spec.TargetRemote, RemoteID: localRemoteID(id), Version: localVersion(id)}
	if err := f.meta.FenceCopyCleanup(ctx, job.Spec.MetaIdentity, key.Remote, key.RemoteID); err != nil {
		return err
	}
	if err := f.journal.BeginCopyCleanup(ctx, id); err != nil {
		return err
	}
	if f.copyCleanupFault != nil {
		if err := f.copyCleanupFault(); err != nil {
			return err
		}
	}
	f.cache.Pin(key, false)
	if err := f.cache.ForgetChecked(key); err != nil {
		return err
	}
	return f.journal.FinishCopyCleanup(ctx, id)
}

// CancelCopy is a durable stop, not data deletion. A completed local version
// stays readable; partial content remains in the journal. Submitted work must
// be administered as an upload, never cancelled by replaying preparation.
func (f *FS) CancelCopy(ctx context.Context, id string) error {
	if f.journal == nil {
		return errors.New("vfs: no copy journal")
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	if err := f.journal.CancelCopy(ctx, id); err != nil {
		return err
	}
	f.copyRunsMu.Lock()
	if cancel := f.copyRuns[id]; cancel != nil {
		cancel()
	}
	f.copyRunsMu.Unlock()
	return nil
}

// RetryCopy validates the retained prefix and local source/target bindings,
// then queues an explicit retry. Source version validation still runs at
// resume, so returning nil means queued, not copied/uploaded.
func (f *FS) RetryCopy(ctx context.Context, id string) error {
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: copy retry requires journal ownership")
	}
	job, err := f.journal.GetCopy(ctx, id)
	if err != nil {
		return err
	}
	if job.State != journal.CopyFailed && job.State != journal.CopyCancelled {
		return journal.ErrCopyState
	}
	_, parent, err := f.copyDestination(ctx, job.Spec)
	if err != nil {
		return err
	}
	if job.Checkpoint < job.Spec.Size {
		if _, err := f.copySourceMount(job.Spec); err != nil {
			return err
		}
	}
	if err := f.meta.CheckCopyTarget(ctx, id, meta.Node{ParentIno: parent.Ino, Name: path.Base(job.Spec.TargetPath), Remote: job.Spec.TargetRemote, RemoteID: localRemoteID(id), Version: localVersion(id), Size: job.Spec.Size}); err != nil {
		return err
	}
	if err := f.journal.RetryCopy(ctx, id); err != nil {
		return err
	}
	f.copyWorkerMu.Lock()
	if f.copyWake == nil {
		f.copyWake = make(chan struct{}, 1)
	}
	select {
	case f.copyWake <- struct{}{}:
	default:
	}
	f.copyWorkerMu.Unlock()
	return nil
}
