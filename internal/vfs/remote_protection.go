package vfs

import (
	"context"

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
	if localOnlyNode(n) {
		return true
	}
	f.mu.Lock()
	protected := f.writers[n.Ino] > 0
	f.mu.Unlock()
	return protected
}

func (f *FS) applyRemoteNode(ctx context.Context, old meta.Node, next *meta.Node) (meta.Node, bool, error) {
	f.remotePublishMu.Lock()
	defer f.remotePublishMu.Unlock()
	return f.meta.ApplyRemoteNode(ctx, old, next, f.protectRemoteNode)
}
