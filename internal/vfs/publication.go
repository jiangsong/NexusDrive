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
			if u.Tombstone || n.Remote != u.Remote || n.RemoteID != localRemoteID(u.ID) {
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
		if u.Ino == 0 || n.IsDir() || n.Remote != u.Remote {
			return fmt.Errorf("vfs: cannot publish upload %s into an unrelated node", u.ID)
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
		f.changedNode(ctx, n.Ino, false)
	}
	return f.recoverCopyVersions(ctx, j)
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
