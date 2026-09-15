package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

var ErrCopySourceChanged = errors.New("vfs: copy source version changed or cannot be verified")
var ErrCopyBindingChanged = errors.New("vfs: copy mount or metadata binding changed")

const copyCleanupTimeout = 5 * time.Second

func (f *FS) copyAwaitingSubmit(ctx context.Context, n meta.Node) (bool, error) {
	if f.journal == nil || !IsLocalOnly(n.RemoteID) {
		return false, nil
	}
	id := strings.TrimPrefix(n.RemoteID, localIDPrefix)
	// Ordinary local writes have no preparation job.
	if _, err := f.journal.Get(ctx, id); err == nil {
		return false, nil
	} else if !errors.Is(err, journal.ErrNotFound) {
		return false, err
	}
	job, err := f.journal.GetCopy(ctx, id)
	if errors.Is(err, journal.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return job.State != journal.CopySubmitted, nil
}

func (f *FS) beginCopyJob(ctx context.Context, src, dst string, h *Handle, dm Mount, parent meta.Node) (a Attr, resultErr error) {
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return a, err
	}
	parentID, err := f.dirRemoteID(ctx, parent.Ino)
	if err != nil {
		return a, err
	}
	if parentID == "" {
		parentID = dm.RootID
	}
	spec := journal.CopySpec{MetaIdentity: identity, TargetParentIno: parent.Ino,
		SourceMount: h.Mount.Prefix, SourceRootID: h.Mount.RootID, SourceAccountBinding: h.Mount.AccountBinding,
		TargetMount: dm.Prefix, TargetRootID: dm.RootID, TargetAccountBinding: dm.AccountBinding,
		SourcePath: src, SourceRemote: h.Node.Remote, SourceID: h.Node.RemoteID, SourceVersion: h.Node.Version, Size: h.Node.Size,
		TargetPath: dst, TargetRemote: dm.Remote, TargetParentID: parentID, Mode: string(dm.Mode)}
	want := dm.Provider.Capabilities().RapidUpload
	if len(want) == 0 {
		want = dm.Provider.Capabilities().HashTypes
	}
	var whole *cache.WholeFile
	var c *journal.CopyStaging
	if whole, err = f.cache.OpenWhole(h.fileKey()); err == nil {
		defer whole.Close()
		info, err := whole.Stat()
		if err != nil {
			return a, err
		}
		if info.Size() != spec.Size {
			return a, ErrCopySourceChanged
		}
		c, err = f.journal.BeginCopyFromWhole(ctx, spec, whole.File, want)
		if errors.Is(err, journal.ErrCopyBusy) {
			return a, ErrExists
		}
		if err != nil && !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, journal.ErrLinkSourceChanged) {
			return a, err
		}
	}
	if c == nil {
		c, err = f.journal.BeginCopy(ctx, spec, want)
		if err != nil {
			if errors.Is(err, journal.ErrCopyBusy) {
				return a, ErrExists
			}
			return a, err
		}
	}
	defer func() {
		job := c.Job()
		c.Close()
		if resultErr != nil && job.State != journal.CopySubmitted {
			cleanup, cancel := context.WithTimeout(context.Background(), copyCleanupTimeout)
			defer cancel()
			_ = f.journal.FailCopy(cleanup, job.ID, "copy request failed before upload submission")
		}
	}()
	ctx, finish, err := f.trackCopy(ctx, c.Job().ID)
	if err != nil {
		return a, err
	}
	defer finish()
	if c.Job().State == journal.CopyPreparing {
		read := func(p []byte, off int64) (int, error) { return f.Read(ctx, h, p, off) }
		if whole != nil {
			read = whole.ReadAt
		}
		if err := f.fillCopy(ctx, c, read); err != nil {
			return a, err
		}
	}
	return f.publishCopy(ctx, c)
}

func (f *FS) fillCopy(ctx context.Context, c *journal.CopyStaging, read func([]byte, int64) (int, error)) error {
	job := c.Job()
	buf := make([]byte, 1<<20)
	for off := job.Checkpoint; off < job.Spec.Size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := buf[:min(int64(len(buf)), job.Spec.Size-off)]
		n, err := read(part, off)
		if n > 0 {
			written, werr := c.Write(part[:n])
			if werr != nil {
				return werr
			}
			if written != n {
				return io.ErrShortWrite
			}
			off += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 || errors.Is(err, io.EOF) && off < job.Spec.Size {
			return io.ErrUnexpectedEOF
		}
		if err := c.Checkpoint(ctx); err != nil {
			return err
		}
		if f.copyCheckpointFault != nil {
			if err := f.copyCheckpointFault(c.Job()); err != nil {
				return err
			}
		}
	}
	if c.Job().State == journal.CopyPreparing {
		return c.Checkpoint(ctx)
	}
	return nil
}

func (f *FS) copyDestination(ctx context.Context, spec journal.CopySpec) (Mount, meta.Node, error) {
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return Mount{}, meta.Node{}, err
	}
	if spec.MetaIdentity == "" || identity != spec.MetaIdentity {
		return Mount{}, meta.Node{}, ErrCopyBindingChanged
	}
	parent, err := f.resolve(ctx, path.Dir(spec.TargetPath))
	if err != nil {
		return Mount{}, parent, err
	}
	if parent.Ino != spec.TargetParentIno || !parent.IsDir() {
		return Mount{}, parent, ErrCopyBindingChanged
	}
	dm, _, err := f.MountForIno(ctx, parent.Ino)
	if err != nil {
		return dm, parent, err
	}
	if dm.Mode == config.ModeReadonly {
		return dm, parent, ErrReadOnly
	}
	if spec.TargetAccountBinding == "" || dm.Remote != spec.TargetRemote || dm.Prefix != spec.TargetMount ||
		dm.RootID != spec.TargetRootID || dm.AccountBinding != spec.TargetAccountBinding {
		return dm, parent, ErrCopyBindingChanged
	}
	parentID, err := f.dirRemoteID(ctx, parent.Ino)
	if err != nil {
		return dm, parent, err
	}
	if parentID == "" {
		parentID = dm.RootID
	}
	if parentID != spec.TargetParentID {
		return dm, parent, ErrCopyBindingChanged
	}
	return dm, parent, nil
}

func (f *FS) copySourceMount(spec journal.CopySpec) (Mount, error) {
	sm, ok := f.mountFor(spec.SourcePath)
	if !ok || spec.SourceAccountBinding == "" || sm.Remote != spec.SourceRemote ||
		sm.Prefix != spec.SourceMount || sm.RootID != spec.SourceRootID ||
		sm.AccountBinding != spec.SourceAccountBinding {
		return Mount{}, ErrCopyBindingChanged
	}
	return sm, nil
}

func (f *FS) publishCopy(ctx context.Context, c *journal.CopyStaging) (Attr, error) {
	f.copyPublishMu.Lock()
	u, node, mode, err := f.submitCopyLocked(ctx, c)
	f.copyPublishMu.Unlock()
	if err != nil {
		return Attr{}, err
	}
	if mode == config.ModeStrict {
		if err := f.flushUpload(ctx, u.ID, u.Remote); err != nil {
			return Attr{}, err
		}
		node, err = f.meta.Get(ctx, node.Ino)
		if err != nil {
			return Attr{}, err
		}
	}
	return f.attrOf(ctx, node), nil
}

func (f *FS) submitCopyLocked(ctx context.Context, c *journal.CopyStaging) (journal.Upload, meta.Node, config.Mode, error) {
	job := c.Job()
	var u journal.Upload
	dm, parent, err := f.copyDestination(ctx, job.Spec)
	if err != nil {
		return u, meta.Node{}, dm.Mode, err
	}
	payload, err := c.PayloadPath()
	if err != nil {
		return u, meta.Node{}, dm.Mode, err
	}
	n := meta.Node{ParentIno: parent.Ino, Name: path.Base(job.Spec.TargetPath), Remote: dm.Remote,
		RemoteID: localRemoteID(job.ID), Version: localVersion(job.ID), Size: job.Spec.Size, Kind: provider.KindFile,
		MTime: f.now(), Dirty: true, TTL: f.opt.AttrTTL}
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	if err := f.cache.LinkPinnedFile(key, payload, n.Size); err != nil {
		return u, n, dm.Mode, err
	}
	n, err = f.meta.BindCopy(ctx, job.ID, n)
	if err != nil {
		// A failed bind can describe a previously published inode whose
		// name changed independently. Keep its readable local version.
		// Metadata errors are not proof that an installed link is orphaned.
		aliases, lookupErr := f.meta.Aliases(ctx, key.Remote, key.RemoteID)
		referenced := lookupErr != nil
		for _, alias := range aliases {
			if alias.Version == key.Version {
				referenced = true
				break
			}
		}
		if !referenced {
			f.cache.Pin(key, false)
			f.cache.Forget(key)
		}
		if errors.Is(err, meta.ErrExists) {
			err = ErrExists
		}
		return u, n, dm.Mode, err
	}
	// BindCopy has made the complete local version visible even if the
	// subsequent upload handoff fails. Readers must learn about it now.
	// Copy never overwrites, so the destination is always a new name.
	f.changedNode(ctx, n.Ino, false, KindCreate)
	if f.copyBindFault != nil {
		if err := f.copyBindFault(); err != nil {
			return u, n, dm.Mode, err
		}
	}
	binding, err := f.uploadBinding(ctx, dm)
	if err != nil {
		return u, n, dm.Mode, err
	}
	boundUpload := journal.Upload{Remote: dm.Remote, RemoteParentID: job.Spec.TargetParentID, Name: n.Name, Ino: n.Ino, NeedsPublish: true}
	applyUploadBinding(&boundUpload, binding)
	u, err = c.Submit(ctx, boundUpload)
	if err != nil {
		return u, n, dm.Mode, err
	}
	if f.commitFault != nil {
		if err := f.commitFault(); err != nil {
			return u, n, dm.Mode, err
		}
	}
	if err := f.journal.MarkPublished(ctx, u.ID); err != nil {
		return u, n, dm.Mode, err
	}
	f.invalidateFrom(ctx, n.Ino)
	mode := dm.Mode
	if job.Spec.Mode == string(config.ModeStrict) {
		mode = config.ModeStrict
	}
	return u, n, mode, nil
}

// ResumeCopy continues a persisted preparation, never a service-side Copy.
// Already-submitted work belongs to the upload queue and must not be replayed.
func (f *FS) ResumeCopy(ctx context.Context, id string) (a Attr, resultErr error) {
	if f.journal == nil || !f.journal.Owner() {
		return a, errors.New("vfs: copy resume requires journal ownership")
	}
	c, err := f.journal.ResumeCopy(ctx, id)
	if err != nil {
		return a, err
	}
	defer c.Close()
	ctx, finish, err := f.trackCopy(ctx, id)
	if err != nil {
		return a, err
	}
	defer finish()
	job := c.Job()
	if _, _, err := f.copyDestination(ctx, job.Spec); err != nil {
		return a, err
	}
	if job.State == journal.CopyPreparing {
		sm, err := f.copySourceMount(job.Spec)
		if err != nil {
			return a, err
		}
		// A recovery opens a new read, not an already authorized file handle.
		// A stable backend ID can survive a move outside the recorded path
		// (and outside an adapter's allowlist). Do not follow that ID blindly.
		// Ready jobs need no source read and intentionally skip this check.
		source, err := f.resolve(ctx, job.Spec.SourcePath)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) {
			return a, ErrCopySourceChanged
		}
		if err != nil {
			return a, err
		}
		if source.IsDir() || source.Remote != job.Spec.SourceRemote || source.RemoteID != job.Spec.SourceID || source.Version != job.Spec.SourceVersion || source.Size != job.Spec.Size {
			return a, ErrCopySourceChanged
		}
		var read func([]byte, int64) (int, error)
		if IsLocalOnly(job.Spec.SourceID) {
			u, err := f.journal.Get(ctx, strings.TrimPrefix(job.Spec.SourceID, localIDPrefix))
			if err != nil || u.Size != job.Spec.Size {
				return a, ErrCopySourceChanged
			}
			src, err := os.Open(u.BlobPath)
			if err != nil {
				return a, err
			}
			defer src.Close()
			read = src.ReadAt
		} else {
			entry, err := sm.Provider.Stat(ctx, job.Spec.SourceID)
			if err != nil {
				return a, mapProviderErr(err)
			}
			if job.Spec.SourceVersion == "" || entry.ID != job.Spec.SourceID || entry.Version != job.Spec.SourceVersion || entry.Size != job.Spec.Size || entry.Kind != provider.KindFile {
				return a, ErrCopySourceChanged
			}
			read = func(buf []byte, off int64) (int, error) {
				r, err := sm.Provider.ReadRange(ctx, job.Spec.SourceID, job.Spec.SourceVersion, off, int64(len(buf)))
				if err != nil {
					return 0, err
				}
				n, err := io.ReadFull(r, buf)
				closeErr := r.Close()
				if err == nil {
					err = closeErr
				}
				return n, err
			}
		}
		if err := f.fillCopy(ctx, c, read); err != nil {
			return a, err
		}
	}
	return f.publishCopy(ctx, c)
}
