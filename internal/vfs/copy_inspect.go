package vfs

import (
	"context"
	"errors"

	"cloudfs/internal/journal"
)

// CopyInfo is a public projection, never the persisted recovery specification.
// In particular it excludes backend IDs, versions, credentials, payload paths,
// database identity, hashes and arbitrary backend/database error strings.
type CopyInfo struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`
	Target     string            `json:"target"`
	State      journal.CopyState `json:"state"`
	Size       int64             `json:"size"`
	Checkpoint int64             `json:"checkpoint"`
	UploadID   string            `json:"upload_id,omitempty"`
}

func copyInfo(job journal.CopyJob) CopyInfo {
	info := CopyInfo{ID: job.ID, Source: job.Spec.SourcePath, Target: job.Spec.TargetPath,
		State: job.State, Size: job.Spec.Size, Checkpoint: job.Checkpoint}
	if job.State == journal.CopySubmitted {
		info.UploadID = job.ID
	}
	return info
}

// copyVisibleInMounts prevents an old journal's paths from being interpreted as
// authorization under a different database or remounted account/root. This is
// metadata-only: a missing source/target remains inspectable, with no Stat IO.
func (f *FS) copyVisibleInMounts(identity string, spec journal.CopySpec) bool {
	if spec.MetaIdentity == "" || spec.MetaIdentity != identity {
		return false
	}
	source, sourceOK := f.mountFor(spec.SourcePath)
	target, targetOK := f.mountFor(spec.TargetPath)
	return sourceOK && targetOK && spec.SourceAccountBinding != "" && spec.TargetAccountBinding != "" &&
		source.Remote == spec.SourceRemote && source.Prefix == spec.SourceMount && source.RootID == spec.SourceRootID &&
		source.AccountBinding == spec.SourceAccountBinding && target.Remote == spec.TargetRemote &&
		target.Prefix == spec.TargetMount && target.RootID == spec.TargetRootID &&
		target.AccountBinding == spec.TargetAccountBinding
}

// InspectCopy exposes only jobs belonging to the current namespace. Callers
// with narrower path permissions must authorize BOTH Source and Target before
// returning the result or invoking a mutation. Job specifications are immutable.
func (f *FS) InspectCopy(ctx context.Context, id string) (CopyInfo, error) {
	if f.journal == nil {
		return CopyInfo{}, errors.New("vfs: no copy journal")
	}
	job, err := f.journal.GetCopy(ctx, id)
	if err != nil {
		return CopyInfo{}, err
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return CopyInfo{}, err
	}
	if !f.copyVisibleInMounts(identity, job.Spec) {
		return CopyInfo{}, ErrNotFound
	}
	return copyInfo(job), nil
}

// InspectCopyPage projects at most limit persisted jobs (plus one lookahead)
// and performs no
// recovery or provider calls. Filtering obsolete bindings may produce an empty
// page with a continuation. The raw cursor is internal and may identify a
// hidden job; restricted adapters must conceal it, not merely base64-encode it.
func (f *FS) InspectCopyPage(ctx context.Context, after string, limit int) ([]CopyInfo, string, error) {
	if f.journal == nil {
		return nil, "", errors.New("vfs: no copy journal")
	}
	jobs, next, err := f.journal.ListCopyJobs(ctx, after, limit)
	if err != nil {
		return nil, "", err
	}
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return nil, "", err
	}
	infos := make([]CopyInfo, 0, len(jobs))
	for _, job := range jobs {
		if f.copyVisibleInMounts(identity, job.Spec) {
			infos = append(infos, copyInfo(job))
		}
	}
	return infos, next, nil
}
