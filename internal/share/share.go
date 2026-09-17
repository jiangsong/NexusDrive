// Package share is the one policy behind the share MCP tool and the
// console's POST /share (docs/agent-first-design.md §8.2, T-55): a public
// link is an irreversible exposure, so it takes an explicit confirmation,
// is refused for a file the drive does not hold yet, is refused for a
// file the cache does not hold in full (the credential scan must not be
// the reason a file is downloaded — pin it first), is refused when the
// content looks like a credential unless forced, and then costs exactly
// one provider call: CreateShare.
package share

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/secrets"
	"cloudfs/internal/vfs"
)

// FS is what the policy needs from the VFS; *vfs.FS satisfies it.
type FS interface {
	ShareTargetOf(ctx context.Context, p string) (vfs.ShareTarget, error)
	ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error)
}

// Request is one share request.
type Request struct {
	Path string
	// Expires is how long the link lives; zero means DefaultExpiry.
	Expires time.Duration
	// Confirm must be true.
	Confirm bool
	// Force shares despite credential-looking content.
	Force bool
	// Code is an optional extraction code (国内网盘 提取码).
	Code string
}

// Result is what a share produced.
type Result struct {
	Path      string             `json:"path"`
	URL       string             `json:"url"`
	Code      string             `json:"code,omitempty"`
	ShareID   string             `json:"share_id,omitempty"`
	ExpiresAt time.Time          `json:"expires_at,omitzero"`
	Findings  []secrets.Finding `json:"findings,omitempty"`
	Forced    bool               `json:"forced,omitempty"`
}

// DefaultExpiry is the link lifetime when the request names none.
const DefaultExpiry = 7 * 24 * time.Hour

// The refusals, each with the code an agent can act on.
var (
	ErrConfirm     = errors.New("share needs confirm=true: a public link is an exposure that cannot be taken back")
	ErrNotSynced   = errors.New("the file has not reached the drive yet; wait for the upload (flush_uploads) and share again")
	ErrNotCached   = errors.New("the file is not fully cached, so its content cannot be checked for credentials without downloading it; pin it first, then share")
	ErrCredentials = errors.New("the content looks like it contains credentials; share with force=true only if you are sure")
	ErrUnsupported = errors.New("this remote cannot create public links")
)

// Create runs the policy and, when it passes, asks the provider for the
// link. The credential findings are returned with a forced share so the
// caller can say what was overridden.
func Create(ctx context.Context, fsys FS, req Request) (Result, error) {
	if !req.Confirm {
		return Result{}, ErrConfirm
	}
	t, err := fsys.ShareTargetOf(ctx, req.Path)
	if errors.Is(err, vfs.ErrNoShare) {
		return Result{}, ErrUnsupported
	}
	if err != nil {
		return Result{}, err
	}
	if !t.Synced {
		return Result{}, ErrNotSynced
	}
	if t.Cached < 1 {
		return Result{}, ErrNotCached
	}
	var findings []secrets.Finding
	if t.Size > 0 {
		data, err := fsys.ReadFileRange(ctx, t.Path, 0, min(t.Size, secrets.ScanLimit))
		if err != nil {
			return Result{}, err
		}
		findings = secrets.Scan(data)
	}
	if len(findings) > 0 && !req.Force {
		return Result{Path: t.Path, Findings: findings}, fmt.Errorf("%w (%s at line %d)", ErrCredentials, findings[0].Rule, findings[0].Line)
	}
	expires := req.Expires
	if expires <= 0 {
		expires = DefaultExpiry
	}
	sh, err := t.Sharer.CreateShare(ctx, t.RemoteID, provider.ShareOptions{Expires: expires, Code: req.Code})
	if err != nil {
		return Result{}, err
	}
	return Result{Path: t.Path, URL: sh.URL, Code: sh.Code, ShareID: sh.ID, ExpiresAt: sh.ExpiresAt, Findings: findings, Forced: len(findings) > 0}, nil
}
