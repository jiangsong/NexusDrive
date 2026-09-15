package vfs

import (
	"context"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// addWriterLocked requires f.mu. Every successful write Open/Create owns one
// reference until Release finishes its commit, including the publication gap
// after the handle has been removed from the public FH map.
func (f *FS) addWriterLocked(ino uint64) {
	if f.writers == nil {
		f.writers = make(map[uint64]int)
	}
	f.writers[ino]++
}

func (f *FS) removeWriter(ino uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writers[ino] <= 1 {
		delete(f.writers, ino)
	} else {
		f.writers[ino]--
	}
}

// An existing remote file need not be metadata-dirty while its open writer
// is editing staging. Keep its original conflict baseline and ancestors, even
// before the first write; the next write can still arrive on this handle.
func (f *FS) protectRemoteNode(n meta.Node) bool {
	if f.hasWriter(n.Ino) {
		return true
	}
	if localOnlyNode(n) {
		return !f.conflictLoser(n)
	}
	return false
}

// hasWriter reports whether a write handle on ino is open or committing.
func (f *FS) hasWriter(ino uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writers[ino] > 0
}

// conflictLoser reports whether a local-only node has lost its blob: the
// upload landed as a conflict copy (the upload hook released the local
// cache entry and invalidated the parent so the next listing could restore
// the remote's version), or the blob is gone for some other reason and
// the remote's version is the only content left to show. Every other
// local-only node keeps its cache entry, pinned, until its upload is
// adopted, so protecting it from a listing is still right.
func (f *FS) conflictLoser(n meta.Node) bool {
	if !IsLocalOnly(n.RemoteID) || f.hasWriter(n.Ino) {
		return false
	}
	_, known := f.cache.Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
	return known == 0
}

func (f *FS) applyRemoteNode(ctx context.Context, old meta.Node, next *meta.Node) (meta.Node, bool, error) {
	f.remotePublishMu.Lock()
	defer f.remotePublishMu.Unlock()
	return f.meta.ApplyRemoteNode(ctx, old, next, f.protectRemoteNode)
}
