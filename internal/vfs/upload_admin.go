package vfs

import (
	"context"
	"errors"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// UploadInfo is the non-secret part of an upload record. Path is empty when
// the durable row can no longer be tied to its current namespace entry (for
// example a tombstone or a cleanup whose metadata phase already completed).
// It deliberately excludes the blob path, hashes, provider session, expected
// remote version and raw backend error.
type UploadInfo struct {
	ID           string        `json:"id"`
	Path         string        `json:"path,omitempty"`
	Remote       string        `json:"remote"`
	Name         string        `json:"name"`
	State        journal.State `json:"state"`
	Size         int64         `json:"size"`
	Attempt      int           `json:"attempt"`
	NextRetryAt  string        `json:"next_retry_at,omitempty"`
	NeedsPublish bool          `json:"needs_publish,omitempty"`
	Tombstone    bool          `json:"tombstone,omitempty"`
}

var ErrUploadManagementTarget = errors.New("vfs: upload is no longer bound to its current namespace target")

// InspectUpload returns one sanitized upload record without provider IO.
func (f *FS) InspectUpload(ctx context.Context, id string) (UploadInfo, error) {
	if f.journal == nil {
		return UploadInfo{}, errors.New("vfs: upload journal is unavailable")
	}
	u, err := f.journal.Get(ctx, id)
	if err != nil {
		return UploadInfo{}, err
	}
	return f.inspectUpload(ctx, u)
}

// InspectUploadPage returns a stable-ID page of sanitized active records.
func (f *FS) InspectUploadPage(ctx context.Context, after string, limit int) ([]UploadInfo, string, error) {
	if f.journal == nil {
		return nil, "", errors.New("vfs: upload journal is unavailable")
	}
	rows, next, err := f.journal.ListActive(ctx, after, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]UploadInfo, 0, len(rows))
	for _, u := range rows {
		info, err := f.inspectUpload(ctx, u)
		if err != nil {
			return nil, "", err
		}
		out = append(out, info)
	}
	return out, next, nil
}

func (f *FS) inspectUpload(ctx context.Context, u journal.Upload) (UploadInfo, error) {
	info := UploadInfo{ID: u.ID, Remote: u.Remote, Name: u.Name, State: u.State,
		Size: u.Size, Attempt: u.Attempt,
		NeedsPublish: u.NeedsPublish, Tombstone: u.Tombstone}
	if !u.NextRetryAt.IsZero() {
		info.NextRetryAt = u.NextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	if u.Ino == 0 || u.NeedsPublish || u.Tombstone {
		return info, nil
	}
	if err := f.validateUploadBinding(ctx, u); err != nil {
		if errors.Is(err, ErrUploadBindingChanged) {
			return info, nil
		}
		return UploadInfo{}, err
	}
	n, err := f.meta.Get(ctx, u.Ino)
	if errors.Is(err, meta.ErrNotFound) {
		return info, nil
	}
	if err != nil {
		return UploadInfo{}, err
	}
	if n.IsDir() || n.Remote != u.Remote || n.RemoteID != localRemoteID(u.ID) ||
		n.Version != localVersion(u.ID) || n.Name != u.Name || n.Size != u.Size {
		return info, nil
	}
	p, err := f.meta.Path(ctx, n.Ino)
	if errors.Is(err, meta.ErrNotFound) {
		return info, nil
	}
	if err != nil {
		return UploadInfo{}, err
	}
	m, ok := f.mountFor(p)
	if !ok || m.Remote != u.Remote {
		return info, nil
	}
	parent, err := f.meta.Get(ctx, n.ParentIno)
	if errors.Is(err, meta.ErrNotFound) {
		return info, nil
	}
	if err != nil {
		return UploadInfo{}, err
	}
	parentPath, err := f.meta.Path(ctx, parent.Ino)
	if errors.Is(err, meta.ErrNotFound) {
		return info, nil
	}
	if err != nil {
		return UploadInfo{}, err
	}
	parentID := parent.RemoteID
	if parentPath == m.Prefix {
		parentID = m.RootID
	}
	if parentID == u.RemoteParentID {
		info.Path = p
	}
	return info, nil
}

// RetryUploadAt requeues a dead upload only if it is still bound to the path
// authorized by the caller. The gate prevents a concurrent rename from moving
// the target outside a scoped management request between checking and commit.
func (f *FS) RetryUploadAt(ctx context.Context, id, expectedPath string) error {
	if f.journal == nil || !f.journal.Owner() {
		return errors.New("vfs: upload retry requires journal ownership")
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	info, err := f.InspectUpload(ctx, id)
	if err != nil {
		return err
	}
	if info.Path == "" || info.Path != expectedPath {
		return ErrUploadManagementTarget
	}
	if m, ok := f.mountFor(info.Path); !ok {
		return ErrUploadManagementTarget
	} else if m.Mode == config.ModeReadonly {
		return ErrReadOnly
	}
	return f.journal.Requeue(ctx, id)
}

// CancelUploadAt durably stops one authorized upload without undoing remote
// effects. The uploader owns the claim-to-transfer registration race.
func (f *FS) CancelUploadAt(ctx context.Context, id, expectedPath string) (journal.State, error) {
	if expectedPath == "" {
		return "", ErrUploadManagementTarget
	}
	f.copyPublishMu.Lock()
	defer f.copyPublishMu.Unlock()
	info, err := f.InspectUpload(ctx, id)
	if err != nil {
		return "", err
	}
	if info.Path == "" || info.Path != expectedPath {
		return "", ErrUploadManagementTarget
	}
	return f.cancelUpload(ctx, id)
}

// CancelUpload is the unrestricted administrative form. Cancellation is safe
// even when a deleted/tombstoned target has no namespace path: it only stops
// further attempts and does not start or replay provider work.
func (f *FS) CancelUpload(ctx context.Context, id string) (journal.State, error) {
	return f.cancelUpload(ctx, id)
}

func (f *FS) cancelUpload(ctx context.Context, id string) (journal.State, error) {
	if f.journal == nil || !f.journal.Owner() || f.uploader == nil {
		return "", errors.New("vfs: upload cancellation coordinator is unavailable")
	}
	return f.uploader.Cancel(ctx, id)
}

// FlushUploads waits for the current queue snapshot through the configured
// uploader. Callers must enforce any administrative scope restrictions.
func (f *FS) FlushUploads(ctx context.Context) (journal.Stats, error) {
	if f.uploader == nil {
		return journal.Stats{}, errors.New("vfs: upload coordinator is unavailable")
	}
	return f.uploader.Flush(ctx)
}
