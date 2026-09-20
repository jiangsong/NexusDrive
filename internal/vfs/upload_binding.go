package vfs

import (
	"context"
	"errors"
	"fmt"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// ErrUploadBindingChanged prevents durable bytes accepted under one local
// database/account generation from being sent through another configuration.
//
// Its own text names no cause. Every rejection below wraps it with the fence
// that actually failed: one bare sentinel for a dozen different checks told
// operators their account binding had changed when the real reason was that
// the file had left the metadata store, which sends them to the wrong place.
var ErrUploadBindingChanged = errors.New("vfs: queued upload rejected")

// bindingRejected names the fence that turned an upload away. The reason is
// for a person reading a dead letter, so it says what changed, not which
// branch was taken.
func bindingRejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUploadBindingChanged, fmt.Sprintf(format, args...))
}

func (f *FS) uploadBinding(ctx context.Context, m Mount) (journal.UploadBinding, error) {
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return journal.UploadBinding{}, err
	}
	if identity == "" || m.Prefix == "" || m.RootID == "" || m.AccountBinding == "" {
		return journal.UploadBinding{}, bindingRejected(
			"mount %q is not fully identified yet (metadata identity %q, prefix %q, root id %q, account binding %q)",
			m.Remote, identity, m.Prefix, m.RootID, m.AccountBinding)
	}
	return journal.UploadBinding{MetaIdentity: identity, MountPrefix: m.Prefix,
		MountRootID: m.RootID, AccountBinding: m.AccountBinding}, nil
}

// uploadNodeMatches reports whether n is the node u was queued for, still
// carrying the identity the queue published: a file with the row's name and
// size, or for a directory creation the directory itself.
func uploadNodeMatches(n meta.Node, u journal.Upload) bool {
	if n.Remote != u.Remote || n.RemoteID != localRemoteID(u.ID) || n.Version != localVersion(u.ID) || n.Name != u.Name {
		return false
	}
	if u.IsMkdir() {
		return n.IsDir()
	}
	return !n.IsDir() && n.Size == u.Size
}

func applyUploadBinding(u *journal.Upload, binding journal.UploadBinding) {
	u.MetaIdentity = binding.MetaIdentity
	u.MountPrefix = binding.MountPrefix
	u.MountRootID = binding.MountRootID
	u.AccountBinding = binding.AccountBinding
}

// validateUploadBinding performs no provider IO. It is called after a worker
// claims a row but before provider resolution or any remote request.
func (f *FS) validateUploadBinding(ctx context.Context, u journal.Upload) error {
	if u.MetaIdentity == "" || u.MountPrefix == "" || u.MountRootID == "" || u.AccountBinding == "" {
		return bindingRejected("this upload carries no binding fields; it was queued before they existed")
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return err
	}
	if identity != u.MetaIdentity {
		return bindingRejected("queued against metadata store %s, but this daemon opened %s", u.MetaIdentity, identity)
	}
	var recorded Mount
	found := false
	for _, m := range f.mounts {
		if m.Prefix == u.MountPrefix && m.Remote == u.Remote {
			if m.RootID != u.MountRootID {
				return bindingRejected("mount %q now has root id %q, not the %q this upload was queued for",
					u.MountPrefix, m.RootID, u.MountRootID)
			}
			if m.AccountBinding != u.AccountBinding {
				return bindingRejected("the account binding of mount %q changed since this upload was queued", u.MountPrefix)
			}
			recorded, found = m, true
			break
		}
	}
	if !found {
		return bindingRejected("no mount at %q serves remote %q any more", u.MountPrefix, u.Remote)
	}
	if recorded.Mode == config.ModeReadonly {
		return bindingRejected("mount %q is now read-only", u.MountPrefix)
	}
	if u.IsDelete() {
		// The node is gone by design; the mount and account fence above
		// is all a delete has to prove.
		return nil
	}
	if u.Ino == 0 {
		return bindingRejected("this upload names no inode")
	}
	n, err := f.meta.Get(ctx, u.Ino)
	if errors.Is(err, meta.ErrNotFound) && u.Tombstone {
		// Deletion removes the local node before an already-started upload
		// finishes and compensates remotely. The durable mount/account fence
		// still applies, but there is intentionally no current path to prove.
		return nil
	}
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return bindingRejected("%q (inode %d) is no longer in the metadata store; its bytes are still in the journal at %s",
				u.Name, u.Ino, u.BlobPath)
		}
		return err
	}
	if !uploadNodeMatches(n, u) {
		return bindingRejected("inode %d is no longer the file this upload was queued for (now %q on %q, %d bytes)",
			u.Ino, n.Name, n.Remote, n.Size)
	}
	currentPath, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return bindingRejected("inode %d has no path any more", u.Ino)
		}
		return err
	}
	current, ok := f.mountFor(currentPath)
	if !ok {
		return bindingRejected("%q is not under any mount any more", currentPath)
	}
	if current.Prefix != recorded.Prefix || current.Remote != recorded.Remote {
		return bindingRejected("%q moved to mount %q on remote %q, away from %q on %q",
			currentPath, current.Prefix, current.Remote, recorded.Prefix, recorded.Remote)
	}
	if current.RootID != recorded.RootID || current.AccountBinding != recorded.AccountBinding {
		return bindingRejected("the mount serving %q no longer has the root id and account binding this upload was queued for", currentPath)
	}
	if current.Mode == config.ModeReadonly {
		return bindingRejected("the mount serving %q is now read-only", currentPath)
	}
	parent, err := f.meta.Get(ctx, n.ParentIno)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return bindingRejected("the parent directory of %q (inode %d) is gone", currentPath, n.ParentIno)
		}
		return err
	}
	parentPath, err := f.meta.Path(ctx, parent.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return bindingRejected("the parent directory of %q has no path any more", currentPath)
		}
		return err
	}
	parentID := parent.RemoteID
	if parentPath == current.Prefix {
		parentID = current.RootID
	}
	if parentID != u.RemoteParentID {
		return bindingRejected("the directory %q now has remote id %q, not the %q this upload was queued for",
			parentPath, parentID, u.RemoteParentID)
	}
	return nil
}

func (f *FS) bindingForUpload(ctx context.Context, u journal.Upload) (journal.UploadBinding, error) {
	n, err := f.meta.Get(ctx, u.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return journal.UploadBinding{}, bindingRejected("%q (inode %d) is no longer in the metadata store", u.Name, u.Ino)
		}
		return journal.UploadBinding{}, err
	}
	p, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		return journal.UploadBinding{}, err
	}
	m, ok := f.mountFor(p)
	if !ok || m.Remote != u.Remote {
		return journal.UploadBinding{}, bindingRejected("%q is not served by remote %q any more", p, u.Remote)
	}
	return f.uploadBinding(ctx, m)
}
