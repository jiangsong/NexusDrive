package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"syscall"

	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// Copy copies the last committed file contents to an exact, absent destination.
// Same-account copies use ServerCopier when advertised. Other copies reuse an
// immutable cached inode or stream through bounded buffers into journal staging.
// Success means locally journaled (writeback) or uploaded (strict). An in-flight
// uncommitted write handle is not flushed implicitly. Directories are explicit
// errors; recursive tree operations are separate from this file primitive.
func (f *FS) Copy(ctx context.Context, src, dst string) (Attr, error) {
	for _, p := range []string{src, dst} {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > 4096 || strings.ContainsAny(p, "\x00\\") {
			return Attr{}, errors.New("vfs: copy requires canonical absolute virtual paths")
		}
	}
	if src == dst {
		return Attr{}, ErrExists
	}
	n, err := f.resolve(ctx, src)
	if err != nil {
		return Attr{}, err
	}
	if n.IsDir() {
		return Attr{}, ErrIsDir
	}
	if n.RemoteID == "" {
		return Attr{}, fmt.Errorf("vfs: source has no committed version: %w", syscall.EBUSY)
	}
	parent, err := f.resolve(ctx, path.Dir(dst))
	if err != nil {
		return Attr{}, err
	}
	if !parent.IsDir() {
		return Attr{}, ErrNotDir
	}
	dm, _, err := f.MountForIno(ctx, parent.Ino)
	if err != nil {
		return Attr{}, err
	}
	if dm.Mode == config.ModeReadonly {
		return Attr{}, ErrReadOnly
	}
	if f.journal == nil || !f.journal.Owner() {
		return Attr{}, fmt.Errorf("vfs: copy requires the write-journal owner: %w", syscall.EBUSY)
	}
	name := path.Base(dst)
	if _, err := f.lookupNode(ctx, parent.Ino, name); err == nil {
		return Attr{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return Attr{}, err
	}
	sm, _, err := f.MountForIno(ctx, n.Ino)
	if err != nil {
		return Attr{}, err
	}
	if sm.Remote == dm.Remote && !IsLocalOnly(n.RemoteID) && !n.Dirty && dm.Provider.Capabilities().ServerCopy {
		if copier, ok := dm.Provider.(provider.ServerCopier); ok {
			parentID, err := f.dirRemoteID(ctx, parent.Ino)
			if err != nil {
				return Attr{}, err
			}
			if parentID == "" {
				parentID = dm.RootID
			}
			entry, err := copier.Copy(ctx, n.RemoteID, parentID, name)
			if err == nil {
				if entry.ID == "" || entry.Kind != provider.KindFile || entry.Size < 0 {
					return Attr{}, errors.New("vfs: remote copy returned an invalid file; reconcile the destination before retrying")
				}
				out := nodeFromEntry(dm.Remote, entry, f.opt.AttrTTL)
				out.ParentIno, out.Name = parent.Ino, name
				out, err = f.meta.Insert(ctx, out)
				if errors.Is(err, meta.ErrExists) {
					return Attr{}, ErrExists
				}
				if err != nil {
					return Attr{}, fmt.Errorf("vfs: remote copy completed; metadata update failed: %w", err)
				}
				f.invalidateFrom(ctx, parent.Ino)
				f.changedEntry(ctx, parent.Ino, name, false)
				return f.attrOf(ctx, out), nil
			}
			// Unsupported guarantees no copy was performed. Do not fall back
			// on timeouts/transient failures: the remote may already have acted.
			if !errors.Is(err, provider.ErrUnsupported) {
				return Attr{}, mapProviderErr(err)
			}
		}
	}
	h, err := f.Open(ctx, n.Ino, false)
	if err != nil {
		return Attr{}, err
	}
	defer f.Release(context.Background(), h)
	return f.beginCopyJob(ctx, src, dst, h, dm, parent)
}
