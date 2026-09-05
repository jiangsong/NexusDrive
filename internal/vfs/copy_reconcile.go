package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// A server-side copy is asked for over a network and answered over a network.
// A timeout, a reset connection or a 5xx does not say whether the object was
// created: the request may have been executed and the answer lost. The same is
// true of a crash between the provider's success and the local metadata write.
//
// So the intent is durable, and what is left over afterwards is resolved by
// asking the provider what is actually there rather than by assuming. That is
// the whole of this file: issue under an intent, and answer the question the
// intent represents.

// ErrCopyUnresolved reports that a server-side copy may or may not have taken
// effect on the account. The destination is reserved until reconciliation
// settles it, so a retry cannot create a second object.
var ErrCopyUnresolved = errors.New("vfs: the server-side copy result is unknown; it will be reconciled against the destination")

// serverCopy performs a same-account copy under a durable intent.
//
// The three outcomes that matter:
//   - the provider answered and the local insert succeeded: the intent is dropped
//   - the provider answered but the insert failed: the object exists and is
//     untracked, so the intent stays and reconciliation adopts it
//   - the provider did not answer usefully: the intent stays and reconciliation
//     looks at the destination before anything else is attempted
func (f *FS) serverCopy(ctx context.Context, copier provider.ServerCopier, sm, dm Mount,
	source meta.Node, parent meta.Node, parentID, dst, name string) (Attr, error) {
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return Attr{}, err
	}
	// An unresolved earlier attempt at this destination is settled first;
	// issuing a second copy on top of one would make neither answerable.
	if err := f.reconcileDestination(ctx, dm, parent, parentID, name); err != nil {
		return Attr{}, err
	}
	// Settling it may have published an object an earlier attempt created. The
	// destination is taken, and copying again would put a second one on the
	// account — which is the duplicate this whole mechanism exists to prevent.
	if _, err := f.lookupNode(ctx, parent.Ino, name); err == nil {
		return Attr{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return Attr{}, err
	}
	spec := journal.ServerCopySpec{
		MetaIdentity: identity, TargetMount: dm.Prefix, TargetRootID: dm.RootID,
		TargetAccountBinding: dm.AccountBinding,
		SourcePath:           sm.Prefix, SourceRemote: source.Remote, SourceID: source.RemoteID,
		SourceVersion: source.Version, Size: source.Size,
		TargetPath: dst, TargetRemote: dm.Remote, TargetParentID: parentID,
		TargetParentIno: parent.Ino, TargetName: name,
	}
	id, err := f.journal.BeginServerCopy(ctx, spec)
	if errors.Is(err, journal.ErrServerCopyBusy) {
		return Attr{}, ErrCopyUnresolved
	}
	if err != nil {
		return Attr{}, err
	}

	entry, copyErr := copier.Copy(ctx, source.RemoteID, parentID, name)
	if copyErr != nil {
		// Only a refusal the provider makes before acting proves nothing
		// happened. Everything else leaves the question open.
		if errors.Is(copyErr, provider.ErrUnsupported) || errors.Is(copyErr, provider.ErrNotFound) ||
			errors.Is(copyErr, provider.ErrExists) || errors.Is(copyErr, provider.ErrAuth) {
			_ = f.journal.FinishServerCopy(context.WithoutCancel(ctx), id)
			return Attr{}, mapProviderErr(copyErr)
		}
		f.noteUnresolved(ctx, id, copyErr)
		return Attr{}, fmt.Errorf("%w: %v", ErrCopyUnresolved, copyErr)
	}
	if entry.ID == "" || entry.Kind != provider.KindFile || entry.Size < 0 {
		// The copy may well have happened; the description of it is unusable.
		f.noteUnresolved(ctx, id, errors.New("the provider described the copy in a way this driver cannot use"))
		return Attr{}, ErrCopyUnresolved
	}
	attr, err := f.adoptCopiedEntry(ctx, dm, parent, name, entry)
	if err != nil {
		f.noteUnresolved(ctx, id, err)
		return Attr{}, fmt.Errorf("%w: the copy exists on the account but could not be recorded: %v", ErrCopyUnresolved, err)
	}
	// Published: the question is answered and the reservation is released.
	if err := f.journal.FinishServerCopy(context.WithoutCancel(ctx), id); err != nil {
		return attr, fmt.Errorf("vfs: copy completed but its intent could not be cleared: %w", err)
	}
	return attr, nil
}

// noteUnresolved records why an intent is still open. The context is detached:
// the reason is most worth writing down exactly when the caller's context is
// the thing that died.
func (f *FS) noteUnresolved(ctx context.Context, id string, cause error) {
	_ = f.journal.NoteServerCopyError(context.WithoutCancel(ctx), id, cause.Error())
}

// adoptCopiedEntry records a copy that exists on the account.
func (f *FS) adoptCopiedEntry(ctx context.Context, dm Mount, parent meta.Node, name string, entry provider.Entry) (Attr, error) {
	out := nodeFromEntry(dm.Remote, entry, f.opt.AttrTTL)
	out.ParentIno, out.Name = parent.Ino, name
	out, err := f.meta.Insert(ctx, out)
	if err != nil {
		return Attr{}, err
	}
	f.invalidateFrom(ctx, parent.Ino)
	f.changedEntry(ctx, parent.Ino, name, false)
	return f.attrOf(ctx, out), nil
}

// reconcileDestination settles any outstanding intent for one destination.
func (f *FS) reconcileDestination(ctx context.Context, dm Mount, parent meta.Node, parentID, name string) error {
	record, ok, err := f.journal.ServerCopyAt(ctx, dm.Remote, parentID, name)
	if err != nil || !ok {
		return err
	}
	return f.resolveServerCopy(ctx, record)
}

// ReconcileServerCopies answers every outstanding server-side copy by looking
// at its destination. The daemon runs it at startup, which is where an
// interrupted copy is discovered, and the copy path runs it for one destination
// before issuing a new request there.
//
// It reports the first failure but tries them all: one destination that cannot
// be listed must not hide the rest.
func (f *FS) ReconcileServerCopies(ctx context.Context) error {
	if f.journal == nil {
		return nil
	}
	records, err := f.journal.ServerCopies(ctx)
	if err != nil {
		return err
	}
	var first error
	unresolved := 0
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.resolveServerCopy(ctx, record); err != nil {
			unresolved++
			if first == nil {
				first = err
			}
		}
	}
	// An object may exist on the account that nothing local points at. That is
	// worth saying in status rather than only in a log line nobody reads.
	warning := ""
	if unresolved > 0 {
		warning = fmt.Sprintf("%d server-side copies are unresolved and may exist on the account: %v", unresolved, first)
	}
	f.copyWorkerMu.Lock()
	f.serverCopyError = warning
	f.copyWorkerMu.Unlock()
	return first
}

// resolveServerCopy asks the provider whether the destination exists.
//
//   - present and a file: the copy happened; adopt it if the tree does not know
//     it yet, then drop the intent
//   - absent: the copy did not happen; drop the intent so the destination is
//     free again
//   - anything else: leave the intent, record why, and report it
func (f *FS) resolveServerCopy(ctx context.Context, record journal.ServerCopy) error {
	spec := record.Spec
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		return err
	}
	// A journal opened against a different metadata store, mount or account
	// must not adopt an object into a tree the intent was not made for. Such a
	// row is left alone rather than resolved or deleted: it is evidence about
	// an account this process is no longer talking to.
	if spec.MetaIdentity != identity {
		return fmt.Errorf("vfs: server copy %s belongs to another metadata store", record.ID)
	}
	m, ok := f.mountFor(spec.TargetPath)
	if !ok || m.Remote != spec.TargetRemote || m.RootID != spec.TargetRootID ||
		m.Prefix != spec.TargetMount || m.AccountBinding != spec.TargetAccountBinding {
		return fmt.Errorf("vfs: server copy %s no longer matches the configured destination mount", record.ID)
	}
	entry, found, err := f.findRemoteChild(ctx, m, spec.TargetParentID, spec.TargetName)
	if err != nil {
		f.noteUnresolved(ctx, record.ID, err)
		return fmt.Errorf("vfs: server copy %s could not be reconciled: %w", record.ID, err)
	}
	if !found {
		// The provider does not have it, so the request did not take effect.
		return f.journal.FinishServerCopy(ctx, record.ID)
	}
	if entry.Kind != provider.KindFile {
		f.noteUnresolved(ctx, record.ID, errors.New("the destination name is held by a directory"))
		return fmt.Errorf("vfs: server copy %s cannot be reconciled: %q is a directory on the account", record.ID, spec.TargetPath)
	}
	parent, err := f.meta.Get(ctx, spec.TargetParentIno)
	if errors.Is(err, meta.ErrNotFound) {
		// The destination directory is gone locally. The object on the account
		// is real; leaving the intent keeps that visible instead of losing it.
		f.noteUnresolved(ctx, record.ID, errors.New("the destination directory no longer exists locally"))
		return fmt.Errorf("vfs: server copy %s has no local destination directory", record.ID)
	}
	if err != nil {
		return err
	}
	if _, err := f.lookupNode(ctx, parent.Ino, spec.TargetName); err == nil {
		// Already in the tree: a previous attempt published it, or a directory
		// refresh picked it up. Nothing to adopt.
		return f.journal.FinishServerCopy(ctx, record.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := f.adoptCopiedEntry(ctx, m, parent, spec.TargetName, entry); err != nil {
		if errors.Is(err, meta.ErrExists) {
			return f.journal.FinishServerCopy(ctx, record.ID)
		}
		f.noteUnresolved(ctx, record.ID, err)
		return fmt.Errorf("vfs: server copy %s exists on the account but could not be recorded: %w", record.ID, err)
	}
	return f.journal.FinishServerCopy(ctx, record.ID)
}

// findRemoteChild looks for one name in a remote directory. The Provider
// interface has no lookup-by-name, so this enumerates; it runs once per
// unresolved copy, not on any hot path.
func (f *FS) findRemoteChild(ctx context.Context, m Mount, parentID, name string) (provider.Entry, bool, error) {
	if streamer, ok := m.Provider.(provider.StreamLister); ok {
		var found provider.Entry
		var ok bool
		stop := errors.New("found")
		err := streamer.ListStream(ctx, parentID, func(e provider.Entry) error {
			if e.Name == name {
				found, ok = e, true
				return stop
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			return provider.Entry{}, false, err
		}
		return found, ok, nil
	}
	cursor := ""
	for pages := 0; pages < maxReconcilePages; pages++ {
		entries, next, err := m.Provider.List(ctx, parentID, cursor)
		if err != nil {
			return provider.Entry{}, false, err
		}
		for _, e := range entries {
			if e.Name == name {
				return e, true, nil
			}
		}
		if next == "" {
			return provider.Entry{}, false, nil
		}
		cursor = next
	}
	return provider.Entry{}, false, fmt.Errorf("vfs: reconciling %q exceeded %d listing pages", path.Join("/", name), maxReconcilePages)
}

// maxReconcilePages bounds the enumeration so a provider that never ends its
// pagination cannot make reconciliation run forever.
const maxReconcilePages = 10_000
