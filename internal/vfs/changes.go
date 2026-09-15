package vfs

import (
	"context"
	"path"
	"strings"

	"cloudfs/internal/meta"
)

// Change describes a visible VFS change, not a durable audit record. Consumers
// must re-read resources; intermediate versions may be coalesced. Paths are
// virtual paths, never provider IDs or local cache filenames.
type Change struct {
	Paths   []string
	Subtree bool // descendants may have moved, disappeared or become stale
	Rescan  bool // bounded queue overflow or an unavailable metadata path
}

// Affects reports whether a resource or its immediate directory listing may
// have changed. Renaming/deleting a directory also affects its descendants.
func (c Change) Affects(p string) bool {
	if c.Rescan {
		return true
	}
	for _, changed := range c.Paths {
		if p == changed || p == path.Dir(changed) || c.Subtree && (changed == "/" || strings.HasPrefix(p, changed+"/")) {
			return true
		}
	}
	return false
}

// WatchChanges subscribes to semantic changes independently of FUSE cache
// invalidations, including writes originating in the kernel. Delivery never
// waits for a consumer or invokes user code. A full queue collapses to a rescan
// hint so slow consumers cannot silently lose coherence or block filesystem IO.
// The caller must cancel its subscription; FS.Close closes all remaining ones.
func (f *FS) WatchChanges() (<-chan Change, func()) {
	f.changeMu.Lock()
	defer f.changeMu.Unlock()
	ch := make(chan Change, 64)
	if f.changesClosed {
		close(ch)
		return ch, func() {}
	}
	if f.changeWatchers == nil {
		f.changeWatchers = make(map[chan Change]struct{})
	}
	f.changeWatchers[ch] = struct{}{}
	f.hasChangeWatchers.Store(true)
	return ch, func() {
		f.changeMu.Lock()
		defer f.changeMu.Unlock()
		if _, ok := f.changeWatchers[ch]; ok {
			delete(f.changeWatchers, ch)
			close(ch)
			f.hasChangeWatchers.Store(len(f.changeWatchers) != 0)
		}
	}
}

func (f *FS) closeChanges() {
	f.changeMu.Lock()
	defer f.changeMu.Unlock()
	f.changesClosed = true
	for ch := range f.changeWatchers {
		close(ch)
		delete(f.changeWatchers, ch)
	}
	f.hasChangeWatchers.Store(false)
}

func (f *FS) emitChange(change Change) {
	if !f.hasChangeWatchers.Load() {
		return
	}
	if len(change.Paths) > 128 {
		change = Change{Rescan: true}
	}
	f.changeMu.Lock()
	defer f.changeMu.Unlock()
	for ch := range f.changeWatchers {
		// Each subscriber owns its slice; mutating it cannot affect peers.
		copy := change
		copy.Paths = append([]string(nil), change.Paths...)
		select {
		case ch <- copy:
		default:
			for len(ch) > 0 {
				// A concurrent reader can drain the queue between len and
				// receive, so every receive must remain nonblocking.
				select {
				case <-ch:
				default:
				}
			}
			ch <- Change{Rescan: true} // only this lock can send or close
		}
	}
}

func (f *FS) changedNode(ctx context.Context, ino uint64, subtree bool) {
	if !f.hasChangeWatchers.Load() {
		return
	}
	p, err := f.meta.Path(ctx, ino)
	if err != nil {
		f.emitChange(Change{Rescan: true})
		return
	}
	f.emitChange(Change{Paths: []string{p}, Subtree: subtree})
}

func (f *FS) changedEntry(ctx context.Context, parent uint64, name string, subtree bool) {
	f.forgetDirReadAhead(parent)
	if !f.hasChangeWatchers.Load() {
		return
	}
	p, err := f.meta.Path(ctx, parent)
	if err != nil {
		f.emitChange(Change{Rescan: true})
		return
	}
	f.emitChange(Change{Paths: []string{path.Join(p, name)}, Subtree: subtree})
}

func (f *FS) changedRename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) {
	f.forgetDirReadAhead(oldParent)
	if newParent != oldParent {
		f.forgetDirReadAhead(newParent)
	}
	if !f.hasChangeWatchers.Load() {
		return
	}
	oldDir, oldErr := f.meta.Path(ctx, oldParent)
	newDir, newErr := f.meta.Path(ctx, newParent)
	if oldErr != nil || newErr != nil {
		f.emitChange(Change{Rescan: true})
		return
	}
	f.emitChange(Change{Paths: []string{path.Join(oldDir, oldName), path.Join(newDir, newName)}, Subtree: true})
}

func (f *FS) changedListing(ctx context.Context, dir uint64, change meta.DirChange) {
	if !f.hasChangeWatchers.Load() || !change.Any() {
		return
	}
	if len(change.Added)+len(change.Removed)+len(change.Updated) > 128 {
		f.changedNode(ctx, dir, true)
		return
	}
	p, err := f.meta.Path(ctx, dir)
	if err != nil {
		f.emitChange(Change{Rescan: true})
		return
	}
	// A listing can remove a directory. Retain its old name after the node
	// was deleted so subscribers to descendants can invalidate their URI.
	c := Change{Subtree: true}
	for _, name := range change.Added {
		c.Paths = append(c.Paths, path.Join(p, name))
	}
	for _, name := range change.Removed {
		c.Paths = append(c.Paths, path.Join(p, name))
	}
	for _, ino := range change.Updated {
		p, err := f.meta.Path(ctx, ino)
		if err != nil {
			f.emitChange(Change{Rescan: true})
			return
		}
		c.Paths = append(c.Paths, p)
	}
	f.emitChange(c)
}
