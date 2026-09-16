package vfs

import (
	"context"
	"path"
	"strings"

	"cloudfs/internal/meta"
)

// ChangeKind names the operation a Change announces. The zero value is
// deliberately not a real kind: a Change built without one (older tests, a
// missed emit point) reads as unknown rather than as a kernel write.
type ChangeKind uint8

const (
	_          ChangeKind = iota
	KindWrite             // a write handle committed (FLUSH), WriteFile, truncate
	KindCreate            // a new file name appeared through a local operation
	KindMkdir             // a new directory
	KindRemove            // a name was unlinked
	KindRename            // Paths[0] moved to Paths[1]
	KindRemote            // a listing, the delta feed or an upload landing changed the tree
	KindRescan            // the queue overflowed or a path could not be resolved
)

// String returns the lower-case name trigger rules and the console use.
func (k ChangeKind) String() string {
	switch k {
	case KindWrite:
		return "write"
	case KindCreate:
		return "create"
	case KindMkdir:
		return "mkdir"
	case KindRemove:
		return "remove"
	case KindRename:
		return "rename"
	case KindRemote:
		return "remote"
	case KindRescan:
		return "rescan"
	}
	return "unknown"
}

// Origin says who made a change: the kernel through the mount, an API
// adapter (MCP, the control plane, WebDAV), or the remote itself. As with
// ChangeKind the zero value is unknown, not kernel.
type Origin uint8

const (
	_            Origin = iota
	OriginKernel        // a request the kernel made, see FromKernel
	OriginAPI           // a request tagged by WithOrigin
	OriginRemote        // discovered rather than requested: listings, delta, uploads landing
)

// String returns the lower-case name trigger rules and the console use.
func (o Origin) String() string {
	switch o {
	case OriginKernel:
		return "kernel"
	case OriginAPI:
		return "api"
	case OriginRemote:
		return "remote"
	}
	return "unknown"
}

const apiOriginKey ctxKey = 2

// WithOrigin marks a context as carrying a request from a named API adapter
// ("mcp", "control", "webdav"). Changes made under it are reported with
// OriginAPI, which is what lets a trigger rule exclude the agent's own
// writes. The name is kept for later audit use, see OriginName; an empty
// name marks nothing.
func WithOrigin(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, apiOriginKey, name)
}

// OriginName returns the adapter name WithOrigin stored, or "".
func OriginName(ctx context.Context) string {
	v, _ := ctx.Value(apiOriginKey).(string)
	return v
}

const actorKey ctxKey = 3

// Actor is who, within an API adapter, made a change: the agent session
// and the principal it belongs to. The MCP server sets it once it has
// resolved the session; the control plane and WebDAV have no session and
// leave it empty. Changes carry it so the record of who changed a file
// (docs/agent-first-design.md §6.1) needs no join with the audit trail.
type Actor struct {
	SessionID string
	Principal string
}

// WithActor marks a context with the session behind its requests. Empty
// fields mark nothing.
func WithActor(ctx context.Context, sessionID, principal string) context.Context {
	if sessionID == "" && principal == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey, Actor{SessionID: sessionID, Principal: principal})
}

// ActorOf returns the actor WithActor stored, or the zero Actor.
func ActorOf(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey).(Actor)
	return a
}

// stamped fills the attribution of a change from its request context: the
// adapter's name and the actor, for changes an adapter made. A remote
// change was discovered, not made, and stays unattributed.
func stamped(ctx context.Context, c Change) Change {
	if c.Origin == OriginAPI {
		c.OriginName, c.Actor = OriginName(ctx), ActorOf(ctx)
	}
	return c
}

// originOf classifies the request behind ctx. Anything neither the kernel
// nor an API adapter tagged is the daemon's own background work (refresh,
// uploads, recovery), whose changes come from the remote's point of view.
func originOf(ctx context.Context) Origin {
	switch {
	case fromKernel(ctx):
		return OriginKernel
	case OriginName(ctx) != "":
		return OriginAPI
	}
	return OriginRemote
}

// originFor is originOf for one emit point. A remote change is discovered,
// not made, by the request that ran the listing or the poll — a kernel
// readdir that finds a new file did not create it — so those kinds always
// carry OriginRemote whatever ctx says.
func originFor(ctx context.Context, kind ChangeKind) Origin {
	if kind == KindRemote || kind == KindRescan {
		return OriginRemote
	}
	return originOf(ctx)
}

// rescanChange is the change every "start over" path emits.
func rescanChange() Change {
	return Change{Rescan: true, Kind: KindRescan, Origin: OriginRemote}
}

// Change describes a visible VFS change, not a durable audit record. Consumers
// must re-read resources; intermediate versions may be coalesced. Paths are
// virtual paths, never provider IDs or local cache filenames. Kind and Origin
// classify the change for consumers that filter (trigger rules); Affects
// ignores them.
type Change struct {
	Paths   []string
	Subtree bool // descendants may have moved, disappeared or become stale
	Rescan  bool // bounded queue overflow or an unavailable metadata path
	Kind    ChangeKind
	Origin  Origin
	// OriginName is the adapter WithOrigin named ("mcp", "control",
	// "webdav") for an OriginAPI change; Actor the session behind it, when
	// the adapter set one.
	OriginName string
	Actor      Actor
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
		change = rescanChange()
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
			ch <- rescanChange() // only this lock can send or close
		}
	}
}

// changedNode announces kind happening to the node ino itself. Each emit
// point passes the operation it performed; the origin comes from ctx.
func (f *FS) changedNode(ctx context.Context, ino uint64, subtree bool, kind ChangeKind) {
	if !f.hasChangeWatchers.Load() {
		return
	}
	p, err := f.meta.Path(ctx, ino)
	if err != nil {
		f.emitChange(rescanChange())
		return
	}
	f.emitChange(stamped(ctx, Change{Paths: []string{p}, Subtree: subtree, Kind: kind, Origin: originFor(ctx, kind)}))
}

// changedEntry announces kind happening to the name under parent, which may
// no longer resolve (a removal) — hence parent plus name, not an inode.
func (f *FS) changedEntry(ctx context.Context, parent uint64, name string, subtree bool, kind ChangeKind) {
	f.forgetDirReadAhead(parent)
	if !f.hasChangeWatchers.Load() {
		return
	}
	p, err := f.meta.Path(ctx, parent)
	if err != nil {
		f.emitChange(rescanChange())
		return
	}
	f.emitChange(stamped(ctx, Change{Paths: []string{path.Join(p, name)}, Subtree: subtree, Kind: kind, Origin: originFor(ctx, kind)}))
}

// changedRename announces a move: Paths[0] is the old name, Paths[1] the new.
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
		f.emitChange(rescanChange())
		return
	}
	f.emitChange(stamped(ctx, Change{Paths: []string{path.Join(oldDir, oldName), path.Join(newDir, newName)}, Subtree: true,
		Kind: KindRename, Origin: originFor(ctx, KindRename)}))
}

// changedListing announces what a directory listing found different from
// the cached view. That is always a remote change: the listing ran on
// behalf of some reader, but the reader did not make the difference.
func (f *FS) changedListing(ctx context.Context, dir uint64, change meta.DirChange) {
	if !f.hasChangeWatchers.Load() || !change.Any() {
		return
	}
	if len(change.Added)+len(change.Removed)+len(change.Updated) > 128 {
		f.changedNode(ctx, dir, true, KindRemote)
		return
	}
	p, err := f.meta.Path(ctx, dir)
	if err != nil {
		f.emitChange(rescanChange())
		return
	}
	// A listing can remove a directory. Retain its old name after the node
	// was deleted so subscribers to descendants can invalidate their URI.
	c := Change{Subtree: true, Kind: KindRemote, Origin: OriginRemote}
	for _, name := range change.Added {
		c.Paths = append(c.Paths, path.Join(p, name))
	}
	for _, name := range change.Removed {
		c.Paths = append(c.Paths, path.Join(p, name))
	}
	for _, ino := range change.Updated {
		p, err := f.meta.Path(ctx, ino)
		if err != nil {
			f.emitChange(rescanChange())
			return
		}
		c.Paths = append(c.Paths, p)
	}
	f.emitChange(c)
}
