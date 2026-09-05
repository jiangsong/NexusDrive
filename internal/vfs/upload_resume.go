package vfs

import (
	"context"
	"errors"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

var ErrUploadResumeTarget = errors.New("vfs: retained upload is no longer the current writable local version at its recorded target")

// ResumeUpload explicitly starts a fresh upload attempt of retained contents.
// Confirmation accepts possible duplicate/overwritten remote data; it is not
// evidence that the cancelled request had no remote effect.
func (f *FS) ResumeUpload(ctx context.Context, id string, confirm bool) error {
	return f.resumeUpload(ctx, id, "", confirm)
}

// ResumeUploadAt is the scoped-management form of ResumeUpload. It performs
// the path check under the same publication gate as the journal transition.
func (f *FS) ResumeUploadAt(ctx context.Context, id, expectedPath string, confirm bool) error {
	if expectedPath == "" {
		return ErrUploadManagementTarget
	}
	return f.resumeUpload(ctx, id, expectedPath, confirm)
}

func (f *FS) resumeUpload(ctx context.Context, id, expectedPath string, confirm bool) error {
	if !confirm {
		return errors.New("vfs: upload resume requires explicit confirmation of remote replay risk")
	}
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: upload resume requires journal ownership")
	}
	// The first scoped check is gated, but the potentially large CRC pass is
	// intentionally not: PrepareUploadResume is read-only, and holding this
	// gate would stall unrelated opens for the size of the retained object.
	if expectedPath != "" {
		f.copyPublishMu.Lock()
		u, err := f.journal.Get(ctx, id)
		if err == nil {
			err = f.checkUploadResumeAt(ctx, u, expectedPath)
		}
		f.copyPublishMu.Unlock()
		if err != nil {
			return err
		}
	} else {
		u, err := f.journal.Get(ctx, id)
		if err != nil {
			return err
		}
		if err := f.checkUploadResume(ctx, u); err != nil {
			return err
		}
	}
	p, err := f.journal.PrepareUploadResume(ctx, id)
	if err != nil {
		return err
	}
	if f.uploadResumeFault != nil {
		f.uploadResumeFault()
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	if err := f.checkUploadResumeAt(ctx, p.Upload(), expectedPath); err != nil {
		return err
	}
	binding, err := f.bindingForUpload(ctx, p.Upload())
	if err != nil {
		return err
	}
	return p.CommitBound(ctx, binding)
}

func (f *FS) checkUploadResumeAt(ctx context.Context, u journal.Upload, expectedPath string) error {
	if err := f.checkUploadResume(ctx, u); err != nil {
		return err
	}
	if expectedPath != "" {
		if current, err := f.meta.Path(ctx, u.Ino); err != nil || current != expectedPath {
			return ErrUploadManagementTarget
		}
	}
	return nil
}

func (f *FS) checkUploadResume(ctx context.Context, u journal.Upload) error {
	if u.State != journal.StateCancelled || u.NeedsPublish || u.Ino == 0 || u.Tombstone {
		return ErrUploadResumeTarget
	}
	n, err := f.meta.Get(ctx, u.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return ErrUploadResumeTarget
		}
		return err
	}
	if n.IsDir() || n.Remote != u.Remote || n.RemoteID != localRemoteID(u.ID) || n.Version != localVersion(u.ID) || n.Name != u.Name || n.Size != u.Size {
		return ErrUploadResumeTarget
	}
	path, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		return err
	}
	m, ok := f.mountFor(path)
	if !ok || m.Remote != u.Remote {
		return ErrUploadResumeTarget
	}
	if m.Mode == config.ModeReadonly {
		return ErrReadOnly
	}
	parent, err := f.meta.Get(ctx, n.ParentIno)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			return ErrUploadResumeTarget
		}
		return err
	}
	parentPath, err := f.meta.Path(ctx, parent.Ino)
	if err != nil {
		return err
	}
	parentID := parent.RemoteID
	if parentPath == m.Prefix {
		parentID = m.RootID
	}
	if parentID != u.RemoteParentID {
		return ErrUploadResumeTarget
	}
	// Copy preparations additionally carry the original metadata and mount
	// identity, which must still hold for a submitted copy's upload.
	if job, err := f.journal.GetCopy(ctx, u.ID); err == nil {
		if _, _, err := f.copyDestination(ctx, job.Spec); err != nil {
			return err
		}
	} else if !errors.Is(err, journal.ErrNotFound) {
		return err
	}
	return nil
}
