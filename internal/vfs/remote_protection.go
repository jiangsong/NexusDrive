package vfs

import (
	"context"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// addWriterLocked requires f.mu. Every successful write Open/Create owns one
// reference until Release finishes its commit, including the publication gap
// after the handle has been removed from the public FH map. It returns readers
// that predate this writer; the caller marks them after dropping f.mu so the
// handle lock order stays acyclic.
func (f *FS) addWriterLocked(ino uint64, existingReaders bool) []*Handle {
	if f.writers == nil {
		f.writers = make(map[uint64]int)
	}
	readers := make([]*Handle, 0)
	if existingReaders {
		for _, h := range f.handles {
			if h.Ino == ino && h.writer == nil {
				readers = append(readers, h)
			}
		}
	}
	f.writers[ino]++
	return readers
}

func markReadersFollowing(readers []*Handle) {
	for _, h := range readers {
		h.mu.Lock()
		h.followWrites = true
		h.mu.Unlock()
	}
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
//
// A directory waiting to be created remotely has no blob and no cache entry
// at all; it has nothing to lose and is never a conflict loser.
func (f *FS) conflictLoser(n meta.Node) bool {
	if !IsLocalOnly(n.RemoteID) || n.IsDir() || f.hasWriter(n.Ino) {
		return false
	}
	_, known := f.cache.Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
	return known == 0
}

func (f *FS) applyRemoteNode(ctx context.Context, old meta.Node, next *meta.Node) (meta.Node, bool, error) {
	// Removing a directory walks its subtree, so a child whose upload has not
	// gone out yet can be reached from here too. Load the queued set before
	// the transaction: protect runs per node inside the metadata writer.
	queued, err := f.queuedUploadInos(ctx)
	if err != nil {
		return meta.Node{}, false, err
	}
	return f.applyRemoteNodeWith(ctx, queued, old, next)
}

// applyRemoteNodeWith is applyRemoteNode with the queued set already in hand.
// A caller that applies a batch of changes — the delta feed walks one event at
// a time — loads the set once for the whole batch rather than paying a scan of
// the upload queue, and a map the size of it, per node. The snapshot is the
// same one a directory listing commits against, and it ages the same way: an
// upload queued after it was taken is still held by protectRemoteNode, which
// runs live on every node.
func (f *FS) applyRemoteNodeWith(ctx context.Context, queued map[uint64]bool, old meta.Node, next *meta.Node) (meta.Node, bool, error) {
	protect := func(n meta.Node) bool {
		// The bytes are still in the journal, so this is a pending write
		// whatever the cache holds.
		return queued[n.Ino] || f.protectRemoteNode(n)
	}
	f.remotePublishMu.Lock()
	defer f.remotePublishMu.Unlock()
	return f.meta.ApplyRemoteNode(ctx, old, next, protect)
}
