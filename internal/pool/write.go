package pool

import (
	"context"
	"io"

	"cloudfs/internal/provider"
)

// The write path lands in the next phase: uploads stream to one member and
// the tree changes fan out to every member holding the entry. Until then a
// pool is read-only and says so.

func (p *Pool) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	return provider.UploadSession{}, provider.ErrUnsupported
}

func (p *Pool) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	return provider.PartToken{}, provider.ErrUnsupported
}

func (p *Pool) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	return provider.Entry{}, provider.ErrUnsupported
}

func (p *Pool) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	return provider.Entry{}, provider.ErrUnsupported
}

func (p *Pool) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	return provider.Entry{}, provider.ErrUnsupported
}

func (p *Pool) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	return provider.Entry{}, provider.ErrUnsupported
}

func (p *Pool) Delete(ctx context.Context, id string) error {
	return provider.ErrUnsupported
}
