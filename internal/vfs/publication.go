package vfs

import (
	"context"
	"errors"
	"fmt"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// RecoverPublications completes journal -> cache -> metadata publication.
// Startup only: call after Journal.Recover and before workers, refreshers or
// callers can mutate the tree. No provider calls or upload replay are needed.
func (f *FS) RecoverPublications(ctx context.Context, j *journal.Journal) error {
	if !j.Owner() {
		return errors.New("vfs: publication recovery requires storage ownership")
	}
	rows, err := j.All(ctx)
	if err != nil {
		return err
	}
	for _, u := range rows {
		if u.IsDelete() {
			continue // nothing local to publish; the row runs as it is
		}
		stopped := u.State == journal.StateCancelled || u.State == journal.StateCancelling
		if !stopped && u.State != journal.StatePending && u.State != journal.StateUploading {
			continue
		}
		if !u.NeedsPublish {
			// Cache links are reconstructible, not the durable source of
			// truth. A power loss may lose one even after the gate opened.
			// Restore only the exact version still selected by metadata.
			n, err := f.meta.Get(ctx, u.Ino)
			if errors.Is(err, meta.ErrNotFound) {
				if stopped {
					continue
				}
				if u.IsMkdir() {
					// A directory creation carries no bytes to keep; with
					// its node gone there is nothing to reconcile.
					if err := j.DropPending(ctx, u.ID); err != nil && !errors.Is(err, journal.ErrNotFound) {
						return err
					}
					continue
				}
				if u.Ino != 0 && !u.Tombstone {
					if err := j.Fail(ctx, u.ID, errors.New("local publication target is missing; retained payload requires reconciliation")); err != nil {
						return err
					}
				}
				continue
			}
			if err != nil {
				return err
			}
			if u.Tombstone || u.IsMkdir() || n.Remote != u.Remote || n.RemoteID != localRemoteID(u.ID) {
				continue
			}
			key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
			if whole, err := f.cache.OpenWhole(key); err == nil {
				whole.Close()
				f.cache.Pin(key, true)
			} else if err := f.cache.LinkPinnedFile(key, u.BlobPath, u.Size); err != nil {
				return err
			}
			continue
		}
		if u.Tombstone {
			// Never recreate a deleted name. Preserve the existing remote
			// compensation path for an explicitly tombstoned row.
			if err := j.MarkPublished(ctx, u.ID); err != nil {
				return err
			}
			continue
		}
		newer, err := j.Superseded(ctx, u)
		if err != nil {
			return err
		}
		if newer {
			if stopped {
				continue
			} // retain cancelled history, never republish it
			if err := j.DropPending(ctx, u.ID); err != nil {
				return err
			}
			continue
		}
		n, err := f.meta.Get(ctx, u.Ino)
		if errors.Is(err, meta.ErrNotFound) {
			if stopped {
				continue
			}
			if u.IsMkdir() {
				if err := j.DropPending(ctx, u.ID); err != nil && !errors.Is(err, journal.ErrNotFound) {
					return err
				}
				continue
			}
			// Absence alone cannot distinguish an intentional unlink from
			// lost/corrupt metadata. Do not resurrect the name, but never
			// destroy the only durable content based on a cache lookup.
			if err := j.Fail(ctx, u.ID, errors.New("local publication target is missing; retained payload requires reconciliation")); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if u.Ino == 0 || n.IsDir() != u.IsMkdir() || n.Remote != u.Remote {
			return fmt.Errorf("vfs: cannot publish upload %s into an unrelated node", u.ID)
		}
		if u.IsMkdir() {
			// The directory's local identity, the step mkdir did not reach.
			// No cache entry: a directory has no bytes.
			if err := f.publishQueuedDir(ctx, j, u, n); err != nil {
				return err
			}
			continue
		}
		oldKey := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
		n.RemoteID, n.Version = localRemoteID(u.ID), localVersion(u.ID)
		n.Size, n.Dirty = u.Size, true
		n.MTime = u.CreatedAt
		if sum := u.Hashes[provider.HashSHA1]; sum != "" {
			n.HashType, n.Hash = string(provider.HashSHA1), sum
		} else {
			n.HashType, n.Hash = "", ""
		}
		key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
		if err := f.cache.LinkPinnedFile(key, u.BlobPath, u.Size); err != nil {
			return fmt.Errorf("vfs: restore upload %s cache: %w", u.ID, err)
		}
		// Preserve the current name, parent and RemoteVersion: a rename or
		// an earlier upload may already have advanced them independently.
		update := f.meta.UpdateByIno
		if j.Durability() == journal.DurabilityPower {
			update = f.meta.PublishByIno
		}
		if err := update(ctx, n); err != nil {
			return err
		}
		if err := j.MarkPublished(ctx, u.ID); err != nil {
			return err
		}
		if oldKey.RemoteID != "" && oldKey != key {
			f.cache.Forget(oldKey)
		}
		f.invalidateFrom(ctx, n.Ino)
		// This is a write's commit finishing after a restart. Whoever made
		// the write died with the previous process, so the origin is the
		// background fallback; the kind is still what the content is.
		f.changedNode(ctx, n.Ino, false, KindWrite)
	}
	if err := f.removeUncommittedDirs(ctx, j); err != nil {
		return err
	}
	return f.recoverCopyVersions(ctx, j)
}

// publishQueuedDir finishes a queued directory's local commit: the node
// takes the row's local identity and the row's publication gate opens.
func (f *FS) publishQueuedDir(ctx context.Context, j *journal.Journal, u journal.Upload, n meta.Node) error {
	n.RemoteID, n.Version = localRemoteID(u.ID), localVersion(u.ID)
	n.Dirty = true
	update := f.meta.UpdateByIno
	if j.Durability() == journal.DurabilityPower {
		update = f.meta.PublishByIno
	}
	if err := update(ctx, n); err != nil {
		return err
	}
	if err := j.MarkPublished(ctx, u.ID); err != nil {
		return err
	}
	f.invalidateFrom(ctx, n.Ino)
	return nil
}

// removeUncommittedDirs takes out directories an interrupted mkdir left in
// the tree with no row to create them: nothing was acknowledged for them,
// and nothing will ever put them on the backend. Whatever was created
// beneath one (nothing, in practice: the window is between two writes)
// goes with it.
func (f *FS) removeUncommittedDirs(ctx context.Context, j *journal.Journal) error {
	dirs, err := f.meta.UncommittedDirs(ctx)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if f.isMountDir(d.Ino) {
			continue
		}
		rows, err := j.ByIno(ctx, d.Ino)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			continue // a row exists; publication above took care of it
		}
		if err := f.meta.Remove(ctx, d.Ino); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		f.dropPaths()
		_ = f.meta.Invalidate(ctx, d.ParentIno)
	}
	return nil
}

// Full versions can be visible before upload handoff. They remain readable
// after failure/cancellation, without resuming IO or recreating missing names.
func (f *FS) recoverCopyVersions(ctx context.Context, j *journal.Journal) error {
	jobs, err := j.CopyJobs(ctx)
	if err != nil {
		return err
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State == journal.CopySubmitted || job.State == journal.CopyPurging || job.Checkpoint != job.Spec.Size || job.Spec.MetaIdentity != identity {
			continue
		}
		aliases, err := f.meta.Aliases(ctx, job.Spec.TargetRemote, localRemoteID(job.ID))
		if err != nil {
			return err
		}
		referenced := false
		for _, n := range aliases {
			if !n.IsDir() && n.Version == localVersion(job.ID) && n.Size == job.Spec.Size {
				referenced = true
				break
			}
		}
		if !referenced {
			continue
		}
		key := cache.FileKey{Remote: job.Spec.TargetRemote, RemoteID: localRemoteID(job.ID), Version: localVersion(job.ID)}
		if err := j.WithReadyCopy(ctx, job.ID, func(payload string, size int64) error {
			return f.cache.LinkPinnedFile(key, payload, size)
		}); err != nil {
			return fmt.Errorf("vfs: restore retained copy %s: %w", job.ID, err)
		}
	}
	return nil
}
