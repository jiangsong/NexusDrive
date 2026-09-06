package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// Refresher keeps the metadata cache in step with remotes that publish a
// change feed. Without it, a change made elsewhere is only noticed when the
// directory's TTL expires; with it, the tree follows the remote within one
// poll interval and long attribute timeouts stay safe (docs/DESIGN.md §4.3).
//
// Remotes with no change feed are left to the TTL path: polling a full listing
// would cost far more than it saves, and on the rate-limited Chinese drives it
// is exactly the traffic pattern that triggers risk control.
type Refresher struct {
	fs       *FS
	interval time.Duration

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// applied counts change events applied, for tests and metrics.
	appliedMu sync.Mutex
	applied   int
	polls     int
}

// NewRefresher builds a refresher. interval is how often each delta-capable
// remote is polled; zero means one minute.
func NewRefresher(f *FS, interval time.Duration) *Refresher {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Refresher{fs: f, interval: interval}
}

// Start runs one poller per delta-capable remote until Stop or ctx ends.
func (r *Refresher) Start(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	rctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	mounts := r.fs.Mounts()
	r.mu.Unlock()

	seen := map[string]bool{}
	for _, m := range mounts {
		if seen[m.Remote] {
			continue // several prefixes can share one remote
		}
		if _, ok := m.Provider.(provider.ChangeLister); !ok {
			continue
		}
		if !m.Provider.Capabilities().Delta {
			// The capability matrix is the contract; a provider that
			// implements the interface but does not advertise it is opting out.
			continue
		}
		seen[m.Remote] = true
		r.wg.Add(1)
		go func(m Mount) {
			defer r.wg.Done()
			r.poll(rctx, m)
		}(m)
	}
}

// Stop ends polling and waits for the pollers.
func (r *Refresher) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Stats returns how many polls ran and how many events were applied.
func (r *Refresher) Stats() (polls, applied int) {
	r.appliedMu.Lock()
	defer r.appliedMu.Unlock()
	return r.polls, r.applied
}

func (r *Refresher) poll(ctx context.Context, m Mount) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.PollOnce(ctx, m); err != nil && ctx.Err() == nil {
				// A failed poll is not fatal: the TTL path still refreshes.
				// The next tick tries again from the same cursor.
				continue
			}
		}
	}
}

// PollOnce fetches and applies one batch of changes for a mount. It returns
// how many events were applied. Tests and `cloudfs status` drive it directly.
func (r *Refresher) PollOnce(ctx context.Context, m Mount) (int, error) {
	lister, ok := m.Provider.(provider.ChangeLister)
	if !ok {
		return 0, fmt.Errorf("vfs: remote %q has no change feed", m.Remote)
	}
	store := r.fs.meta
	cursor, err := store.Cursor(ctx, m.Remote)
	if err != nil {
		return 0, err
	}
	events, next, err := lister.Changes(ctx, cursor)
	var reset *provider.CursorResetError
	if errors.As(err, &reset) {
		if reset.Cursor == "" || reset.Cursor == cursor {
			return 0, fmt.Errorf("vfs: remote %q returned an invalid replacement change cursor", m.Remote)
		}
		// A reset means the provider can no longer prove which cached directories
		// changed. Mark every listing stale before adopting the new baseline; this
		// is rare, and broad invalidation is safer than leaving a long-TTL subtree
		// permanently stale. Nodes, pending writes, and cached file bytes remain.
		if err := store.InvalidateAll(ctx); err != nil {
			return 0, err
		}
		if err := store.SetCursor(ctx, m.Remote, reset.Cursor); err != nil {
			return 0, err
		}
		r.fs.listingNotificationFallback(meta.RootIno)
		r.appliedMu.Lock()
		r.polls++
		r.appliedMu.Unlock()
		return 0, nil
	}
	if err != nil {
		return 0, mapProviderErr(err)
	}

	r.appliedMu.Lock()
	r.polls++
	r.appliedMu.Unlock()

	applied := 0
	for _, e := range events {
		if ctx.Err() != nil {
			return applied, ctx.Err()
		}
		n, err := r.apply(ctx, m, e)
		if err != nil {
			// Stop before advancing the cursor so the failed event is retried.
			return applied, err
		}
		if n {
			applied++
		}
	}
	if next != "" && next != cursor {
		if err := store.SetCursor(ctx, m.Remote, next); err != nil {
			return applied, err
		}
	}
	r.appliedMu.Lock()
	r.applied += applied
	r.appliedMu.Unlock()
	return applied, nil
}

// apply folds one change event into the local tree. It reports whether
// anything changed.
func (r *Refresher) apply(ctx context.Context, m Mount, e provider.Change) (bool, error) {
	store := r.fs.meta

	// Find the local node for the remote id, if we have one cached at all.
	local, haveLocal, err := r.nodeByRemoteID(ctx, m.Remote, e.ID)
	if err != nil {
		return false, err
	}

	if e.Op == provider.ChangeDelete {
		if !haveLocal {
			return false, nil // never cached; nothing to do
		}
		// A local write that has not been uploaded must not be destroyed by a
		// delete event for the version it was based on.
		_, applied, err := r.fs.applyRemoteNode(ctx, local, nil)
		if err != nil {
			if !errors.Is(err, meta.ErrNodeChanged) {
				r.fs.listingNotificationFallback(local.ParentIno)
			}
			return false, err
		}
		if !applied {
			return false, nil
		}
		parent := local.ParentIno
		r.fs.cache.Forget(cache.FileKey{Remote: local.Remote, RemoteID: local.RemoteID, Version: local.Version})
		r.fs.dropPaths()
		r.fs.invalidateListing(parent, meta.DirChange{Removed: []string{local.Name}, Updated: []uint64{local.Ino}})
		r.fs.changedEntry(ctx, parent, local.Name, local.IsDir())
		r.fs.wakePins()
		return true, nil
	}

	if e.Entry == nil {
		return false, nil
	}
	if !haveLocal {
		// The entry is somewhere we have not listed. Marking the parent stale
		// is enough: the next readdir picks it up, and we avoid inventing a
		// tree path we cannot verify.
		p, ok, err := r.nodeByRemoteID(ctx, m.Remote, e.ParentID)
		if err != nil {
			return false, err
		}
		if !ok && m.Prefix == "/" && e.ParentID != "" && e.ParentID == m.RootID {
			// A remote mounted at the root: the mount root is the meta
			// root, which carries no remote id of its own. Without this a
			// file created at the top of the drive never surfaced through
			// the feed.
			if root, rerr := store.Get(ctx, meta.RootIno); rerr == nil {
				p, ok = root, true
			}
		}
		if ok {
			if err := store.Invalidate(ctx, p.Ino); err != nil {
				return false, err
			}
			r.fs.invalidate(p.Ino)
			r.fs.changedNode(ctx, p.Ino, true)
			return true, nil
		}
		return false, nil
	}
	// Unchanged version: nothing to do, and importantly no cache churn.
	if e.Entry.Version != "" && e.Entry.Version == local.RemoteVersion && e.Entry.Kind == local.Kind && e.Entry.ID == local.RemoteID {
		return false, nil
	}
	// A pending local write wins locally until it uploads; the upload's own
	// conflict check decides what happens on the remote.
	if r.fs.protectRemoteNode(local) {
		return false, nil
	}

	updated := nodeFromEntry(m.Remote, *e.Entry, r.fs.opt.AttrTTL)
	updated.Ino = local.Ino
	updated.ParentIno = local.ParentIno
	updated.Name = local.Name // a rename arrives as a listing change, not here
	updated, applied, err := r.fs.applyRemoteNode(ctx, local, &updated)
	if err != nil {
		if !errors.Is(err, meta.ErrNodeChanged) {
			r.fs.listingNotificationFallback(local.ParentIno)
		}
		return false, err
	}
	if !applied {
		return false, nil
	}
	// The content changed, so the cached blocks for the old version are stale.
	if local.Version != updated.Version || local.RemoteID != updated.RemoteID || local.Ino != updated.Ino {
		r.fs.cache.Forget(cache.FileKey{Remote: local.Remote, RemoteID: local.RemoteID, Version: local.Version})
	}
	r.fs.invalidate(local.Ino)
	if local.Ino != updated.Ino {
		r.fs.dropPaths()
		r.fs.invalidateListing(local.ParentIno, meta.DirChange{Removed: []string{local.Name}})
		r.fs.changedEntry(ctx, local.ParentIno, local.Name, true)
	} else {
		r.fs.changedNode(ctx, local.Ino, false)
	}
	r.fs.wakePins()
	return true, nil
}

// nodeByRemoteID looks up a cached node by its provider id.
func (r *Refresher) nodeByRemoteID(ctx context.Context, remote, remoteID string) (meta.Node, bool, error) {
	if remoteID == "" {
		return meta.Node{}, false, nil
	}
	n, err := r.fs.meta.ByRemoteID(ctx, remote, remoteID)
	if errors.Is(err, meta.ErrNotFound) {
		return meta.Node{}, false, nil
	}
	if err != nil {
		return meta.Node{}, false, err
	}
	return n, true, nil
}
