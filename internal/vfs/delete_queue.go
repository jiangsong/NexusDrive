package vfs

import (
	"context"
	"errors"
	"strings"
	"sync"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// queuedDeletes is the set of backend entries whose removal is in the
// queue: the tree has forgotten them, the backend still has them. It is what
// a listing consults so the interval between the two — seconds on a quiet
// drive, minutes behind an rm -rf of a large tree — does not bring the
// names back. The journal rows are the durable form; this is their index,
// rebuilt from them when the write backend is attached.
type queuedDeletes struct {
	mu  sync.Mutex
	ids map[string]map[string]struct{} // remote -> backend id
}

func (q *queuedDeletes) add(remote, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids == nil {
		q.ids = map[string]map[string]struct{}{}
	}
	set := q.ids[remote]
	if set == nil {
		set = map[string]struct{}{}
		q.ids[remote] = set
	}
	set[id] = struct{}{}
}

func (q *queuedDeletes) drop(remote, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.ids[remote], id)
}

func (q *queuedDeletes) has(remote, id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.ids[remote][id]
	return ok
}

// under lists the queued ids that are paths below dir — for a backend whose
// ids are paths, the deletes a move of dir would leave pointing at nothing.
func (q *queuedDeletes) under(remote, dir string) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for id := range q.ids[remote] {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return out
}

// loadQueuedDeletes rebuilds the index from the journal, at startup and
// whenever the write backend is attached.
func (f *FS) loadQueuedDeletes(ctx context.Context, j *journal.Journal) error {
	rows, err := j.QueuedDeletes(ctx)
	if err != nil {
		return err
	}
	for _, u := range rows {
		f.deleting.add(u.Remote, u.RemoteID)
	}
	return nil
}

// queuesDeletes reports whether a removal on m is a local commit that the
// queue carries to the backend, the way a write or a mkdir is. Strict
// mounts do it in the caller's thread; so does a VFS without a queue.
func (f *FS) queuesDeletes(m Mount) bool {
	return m.Mode == config.ModeWriteback && f.journal != nil && f.uploader != nil
}

// queueDelete commits the removal of n's backend entry to the queue and
// records it in the index — in that order's opposite: the index first, so
// that a listing running between the two cannot bring the entry back, then
// the row. A row the journal refused is taken out of the index again.
func (f *FS) queueDelete(ctx context.Context, m Mount, n meta.Node, parentRemoteID, orderName string) error {
	kind := journal.KindDelete
	if n.IsDir() {
		kind = journal.KindRmdir
	}
	if orderName == "" {
		orderName = remoteName(n.Name, n.Kind)
	}
	row := journal.Upload{
		ID: journal.NewID(), Kind: kind, Remote: m.Remote,
		RemoteParentID: parentRemoteID, Name: orderName, RemoteID: n.RemoteID,
	}
	binding, err := f.uploadBinding(ctx, m)
	if err != nil {
		return err
	}
	applyUploadBinding(&row, binding)
	f.deleting.add(m.Remote, n.RemoteID)
	if err := f.journal.Commit(ctx, row); err != nil {
		f.deleting.drop(m.Remote, n.RemoteID)
		return err
	}
	return nil
}

// queueDeletesBelow queues the removal of every backend entry under dir,
// children before their directory so the queue's ordering holds each
// directory back until what was in it has gone. Nodes the backend never had
// are cancelled instead. The directory itself is the caller's.
func (f *FS) queueDeletesBelow(ctx context.Context, m Mount, dir meta.Node) error {
	kids, err := f.meta.Children(ctx, dir.Ino)
	if err != nil {
		return err
	}
	parentID := dir.RemoteID
	if parentID == "" {
		parentID = m.RootID
	}
	for _, k := range kids {
		if k.IsDir() {
			if err := f.queueDeletesBelow(ctx, m, k); err != nil {
				return err
			}
		}
		switch {
		case IsLocalOnly(k.RemoteID):
			if err := f.cancelQueued(ctx, k); err != nil {
				return err
			}
		case k.RemoteID != "":
			if err := f.queueDelete(ctx, m, k, parentID, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// settleDeletesUnder runs, in the caller's thread, the queued deletes whose
// targets are paths under id — before a move or rename of id on a backend
// whose ids are paths, where the move would otherwise leave them naming
// entries that no longer exist and the files would come back under the new
// name. A row a worker already holds is waited for.
func (f *FS) settleDeletesUnder(ctx context.Context, m Mount, id string) error {
	if !m.Provider.Capabilities().PathIDs || id == "" {
		return nil
	}
	ids := f.deleting.under(m.Remote, id)
	if len(ids) == 0 {
		return nil
	}
	rows, err := f.journal.QueuedDeletes(ctx)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	// Deepest first: a directory's row is not claimable before its
	// children's, and the rows were queued in that order.
	for _, u := range rows {
		if u.Remote != m.Remote || !want[u.RemoteID] {
			continue
		}
		if err := f.runQueuedNow(ctx, u.ID); err != nil {
			return err
		}
	}
	return nil
}

// runQueuedNow claims one row and runs it here, or waits for the worker
// that has it, the way ensureRemoteDir does for a directory a caller needs.
func (f *FS) runQueuedNow(ctx context.Context, rowID string) error {
	row, err := f.journal.ClaimID(ctx, rowID)
	if err == nil {
		f.uploader.RunOne(ctx, row)
	} else if !errors.Is(err, journal.ErrInFlight) && !errors.Is(err, journal.ErrNotFound) {
		return err
	}
	return f.awaitUpload(ctx, rowID)
}

// nodeForRemoteDir finds the directory node a backend id names, including
// a mount's root, which carries no id of its own.
func (f *FS) nodeForRemoteDir(ctx context.Context, remote, id string) (meta.Node, bool) {
	if n, err := f.meta.ByRemoteID(ctx, remote, id); err == nil {
		return n, true
	}
	for _, m := range f.mounts {
		if m.Remote == remote && m.RootID == id {
			if n, err := f.resolve(ctx, m.Prefix); err == nil {
				return n, true
			}
		}
	}
	return meta.Node{}, false
}
