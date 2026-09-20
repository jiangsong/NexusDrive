package vfs

import (
	"context"
	"errors"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// ErrUploadBindingChanged prevents durable bytes accepted under one local
// database/account generation from being sent through another configuration.
var ErrUploadBindingChanged = errors.New("vfs: upload metadata, mount or account binding changed")

func (f *FS) uploadBinding(ctx context.Context, m Mount) (journal.UploadBinding, error) {
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return journal.UploadBinding{}, err
	}
	if identity == "" || m.Prefix == "" || m.RootID == "" || m.AccountBinding == "" {
		return journal.UploadBinding{}, ErrUploadBindingChanged
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
		return ErrUploadBindingChanged
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return err
	}
	if identity != u.MetaIdentity {
		return ErrUploadBindingChanged
	}
	var recorded Mount
	found := false
	for _, m := range f.mounts {
		if m.Prefix == u.MountPrefix && m.Remote == u.Remote {
			if m.RootID != u.MountRootID || m.AccountBinding != u.AccountBinding {
				return ErrUploadBindingChanged
			}
			recorded, found = m, true
			break
		}
	}
	if !found || recorded.Mode == config.ModeReadonly {
		return ErrUploadBindingChanged
	}
	if u.IsDelete() {
		// The node is gone by design; the mount and account fence above
		// is all a delete has to prove.
		return nil
	}
	if u.Ino == 0 {
		return ErrUploadBindingChanged
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
			return ErrUploadBindingChanged
		}
		return err
	}
	if !uploadNodeMatches(n, u) {
		return ErrUploadBindingChanged
	}
	currentPath, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return ErrUploadBindingChanged
		}
		return err
	}
	current, ok := f.mountFor(currentPath)
	if !ok || current.Prefix != recorded.Prefix || current.Remote != recorded.Remote ||
		current.RootID != recorded.RootID || current.AccountBinding != recorded.AccountBinding ||
		current.Mode == config.ModeReadonly {
		return ErrUploadBindingChanged
	}
	parent, err := f.meta.Get(ctx, n.ParentIno)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return ErrUploadBindingChanged
		}
		return err
	}
	parentPath, err := f.meta.Path(ctx, parent.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return ErrUploadBindingChanged
		}
		return err
	}
	parentID := parent.RemoteID
	if parentPath == current.Prefix {
		parentID = current.RootID
	}
	if parentID != u.RemoteParentID {
		return ErrUploadBindingChanged
	}
	return nil
}

func (f *FS) bindingForUpload(ctx context.Context, u journal.Upload) (journal.UploadBinding, error) {
	n, err := f.meta.Get(ctx, u.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return journal.UploadBinding{}, ErrUploadBindingChanged
		}
		return journal.UploadBinding{}, err
	}
	p, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		return journal.UploadBinding{}, err
	}
	m, ok := f.mountFor(p)
	if !ok || m.Remote != u.Remote {
		return journal.UploadBinding{}, ErrUploadBindingChanged
	}
	return f.uploadBinding(ctx, m)
}
