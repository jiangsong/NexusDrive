package pool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"cloudfs/internal/provider"
)

// The op log holds tree operations a member could not be given at the
// time — it was down — to be replayed when it answers again. An op is
// expressed as a target state and replayed idempotently: a rename whose
// destination already exists with the same content is done, a delete of
// something already gone is done, and a delete of content that changed
// since is refused and reported, never applied. The other machine sharing
// the members may have applied the same operation first; that must be a
// no-op here, not an error.

// opMaxAttempts is how many times an op that fails for a reason of its own
// (not the member being unreachable) is retried before it is parked.
const opMaxAttempts = 5

type pendingOp struct {
	seq      int64
	member   string
	op       string
	path     string
	args     map[string]string
	attempts int
	created  int64
}

// ReplayOnce replays due ops on every member that can be reached, oldest
// first per member, and reports how many it completed.
func (p *Pool) ReplayOnce(ctx context.Context) (int, error) {
	p.expireOps(ctx)
	done := 0
	probe := p.probeInterval()
	for _, m := range p.members {
		if !m.usable(probe) {
			continue
		}
		ops, err := p.pendingOpsFor(ctx, m.name)
		if err != nil {
			return done, err
		}
		for _, op := range ops {
			if ctx.Err() != nil {
				return done, ctx.Err()
			}
			err := p.replay(ctx, m, op)
			switch {
			case err == nil:
				done++
				_, _ = p.db.ExecContext(ctx, `DELETE FROM pending_ops WHERE seq = ?`, op.seq)
			case unreachable(err):
				// Down again; the rest of its ops wait with it.
				m.note(err)
				goto next
			default:
				attempts := op.attempts + 1
				state := "pending"
				if attempts >= opMaxAttempts {
					state = "failed"
					_ = p.tx(ctx, func(tx *sql.Tx) error {
						recordDivergence(tx, op.path, m.name, "op-failed-"+op.op, err.Error(), p.now().UnixNano())
						return nil
					})
				}
				delay := 30 * time.Second << uint(min(attempts, 6))
				_, _ = p.db.ExecContext(ctx, `UPDATE pending_ops SET attempts = ?, next_at = ?, last_error = ?, state = ? WHERE seq = ?`, attempts, p.now().Add(delay).UnixNano(), err.Error(), state, op.seq)
			}
		}
	next:
	}
	return done, nil
}

func (p *Pool) pendingOpsFor(ctx context.Context, member string) ([]pendingOp, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT seq, member, op, path, args, attempts, created_at FROM pending_ops WHERE member = ? AND state = 'pending' AND next_at <= ? ORDER BY seq`, member, p.now().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []pendingOp
	for rows.Next() {
		var op pendingOp
		var args string
		if err := rows.Scan(&op.seq, &op.member, &op.op, &op.path, &args, &op.attempts, &op.created); err != nil {
			return nil, err
		}
		op.args = map[string]string{}
		_ = json.Unmarshal([]byte(args), &op.args)
		out = append(out, op)
	}
	return out, rows.Err()
}

// expireOps drops ops older than op_ttl. A member that missed that much is
// reconciled by a full scrub instead, which the flag on it requests.
func (p *Pool) expireOps(ctx context.Context) {
	ttl := p.settings.OpTTL
	if ttl <= 0 {
		return
	}
	cutoff := p.now().Add(-ttl).UnixNano()
	rows, err := p.db.QueryContext(ctx, `SELECT DISTINCT member FROM pending_ops WHERE state = 'pending' AND created_at < ?`, cutoff)
	if err != nil {
		return
	}
	var expired []string
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil {
			expired = append(expired, m)
		}
	}
	rows.Close()
	if len(expired) == 0 {
		return
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM pending_ops WHERE state = 'pending' AND created_at < ?`, cutoff)
	for _, name := range expired {
		if m := p.byName[name]; m != nil {
			m.mu.Lock()
			m.needsScrub = true
			m.mu.Unlock()
		}
	}
}

// replay applies one op to one member.
func (p *Pool) replay(ctx context.Context, m *member, op pendingOp) error {
	switch op.op {
	case "mkdir":
		pth := op.path
		if id := op.args["id"]; id != "" {
			if cur, err := p.pathOf(ctx, id); err == nil {
				pth = cur
			}
		}
		if _, ok, _ := p.entryAt(ctx, pth); !ok {
			return nil // removed since
		}
		_, err := p.ensureDir(ctx, m, pth)
		return err
	case "rename", "move":
		return p.replayRelocate(ctx, m, op)
	case "delete":
		return p.replayDelete(ctx, m, op)
	}
	return fmt.Errorf("pool: unknown op %q", op.op)
}

// replayRelocate brings the member's copy to the entry's current path. The
// index row for that copy sits at the new path with the old physical name
// (state pending); the physical file may also be gone, or already there.
func (p *Pool) replayRelocate(ctx context.Context, m *member, op pendingOp) error {
	from, to := op.args["from"], op.args["to"]
	if from == "" || to == "" {
		return errors.New("pool: relocate op without paths")
	}
	// The entry may have moved on since; its id says where it is now.
	current := to
	if id := op.args["id"]; id != "" {
		if cur, err := p.pathOf(ctx, id); err == nil {
			current = cur
		}
	}
	// A directory: the member's copy of the directory is the pending
	// member_dirs row at the current path.
	var dirID string
	if err := p.db.QueryRowContext(ctx, `SELECT remote_id FROM member_dirs WHERE member = ? AND path = ?`, m.name, current).Scan(&dirID); err == nil {
		return p.relocateOnMember(ctx, m, dirID, current, provider.KindDir)
	}
	var remoteID, physical string
	err := p.db.QueryRowContext(ctx, `SELECT remote_id, member_name FROM replicas WHERE member = ? AND path = ?`, m.name, current).Scan(&remoteID, &physical)
	if err == sql.ErrNoRows {
		// The copy was dropped meanwhile (deleted, or the file was
		// rewritten and this member's copy is stale): nothing to move.
		return nil
	}
	if err != nil {
		return err
	}
	if physical == path.Base(current) {
		var state string
		_ = p.db.QueryRowContext(ctx, `SELECT state FROM replicas WHERE member = ? AND path = ?`, m.name, current).Scan(&state)
		if state != "pending" {
			return nil // already where it should be
		}
	}
	return p.relocateOnMember(ctx, m, remoteID, current, provider.KindFile)
}

// relocateOnMember renames/moves one object on a member to match current.
func (p *Pool) relocateOnMember(ctx context.Context, m *member, remoteID, current string, kind provider.Kind) error {
	parentID, err := p.ensureDir(ctx, m, parentOf(current))
	if err != nil {
		return err
	}
	name := path.Base(current)
	e, err := m.p.Stat(ctx, remoteID)
	if err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			// Gone on the member. If something with the right name is
			// there already, adopt it as done; otherwise the copy is lost.
			if found, ok := p.findChild(ctx, m, parentID, name); ok {
				return p.adoptRelocated(ctx, m, found, current, kind)
			}
			return p.dropMemberCopy(ctx, m, current, kind)
		}
		return err
	}
	m.note(nil)
	// Move first when the parent differs, then rename when the name does.
	if e.ParentID != "" && e.ParentID != parentID {
		if e, err = m.p.Move(ctx, remoteID, parentID); err != nil {
			if errors.Is(err, provider.ErrExists) {
				return p.tx(ctx, func(tx *sql.Tx) error {
					recordDivergence(tx, current, m.name, "relocate-conflict", "another item with that name exists on the member", p.now().UnixNano())
					return nil
				})
			}
			return err
		}
	}
	if e.Name != name {
		if e, err = m.p.Rename(ctx, remoteID, name); err != nil {
			if errors.Is(err, provider.ErrExists) {
				return p.tx(ctx, func(tx *sql.Tx) error {
					recordDivergence(tx, current, m.name, "relocate-conflict", "another item with that name exists on the member", p.now().UnixNano())
					return nil
				})
			}
			return err
		}
	}
	return p.adoptRelocated(ctx, m, e, current, kind)
}

func (p *Pool) findChild(ctx context.Context, m *member, parentID, name string) (provider.Entry, bool) {
	entries, err := m.listAll(ctx, parentID)
	if err != nil {
		return provider.Entry{}, false
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return provider.Entry{}, false
}

// adoptRelocated records that the member's copy now sits at current.
func (p *Pool) adoptRelocated(ctx context.Context, m *member, e provider.Entry, current string, kind provider.Kind) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UnixNano()
	if kind == provider.KindDir {
		m.mu.Lock()
		m.dirIDs = map[string]string{}
		m.mu.Unlock()
		return p.tx(ctx, func(tx *sql.Tx) error {
			// Descendant ids of a path-addressed member changed with the
			// move; forget them and let them be resolved again.
			if _, err := tx.Exec(`DELETE FROM member_dirs WHERE member = ? AND path LIKE ? ESCAPE '\'`, m.name, likePrefix(current)); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE replicas SET state = 'pending' WHERE member = ? AND path LIKE ? ESCAPE '\' AND state = 'live'`, m.name, likePrefix(current)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO member_dirs(member, path, parent, remote_id, verified_at, listed_at) VALUES(?, ?, ?, ?, ?, 0)
				ON CONFLICT(member, path) DO UPDATE SET remote_id = excluded.remote_id, verified_at = excluded.verified_at, listed_at = 0`, m.name, current, dirParent(current), e.ID, now)
			return err
		})
	}
	return p.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE replicas SET remote_id = ?, member_name = ?, version = ?, state = 'live', seen_at = ? WHERE member = ? AND path = ?`, e.ID, e.Name, e.Version, now, m.name, current)
		return err
	})
}

func (p *Pool) dropMemberCopy(ctx context.Context, m *member, current string, kind provider.Kind) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tx(ctx, func(tx *sql.Tx) error {
		if kind == provider.KindDir {
			p.forgetDirIDTx(tx, m, current)
			return nil
		}
		_, err := tx.Exec(`DELETE FROM replicas WHERE member = ? AND path = ?`, m.name, current)
		return err
	})
}

// replayDelete removes the member's copy at the path, provided it still
// holds what was deleted. Content that changed since is somebody's work
// and is left alone — it reappears in the listing as an adopted file.
func (p *Pool) replayDelete(ctx context.Context, m *member, op pendingOp) error {
	parentID, err := p.dirID(ctx, m, parentOf(op.path))
	if err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			return nil // the whole parent is gone on the member
		}
		return err
	}
	e, ok := p.findChild(ctx, m, parentID, path.Base(op.path))
	if !ok {
		return nil
	}
	if e.Kind == provider.KindDir {
		children, err := m.listAll(ctx, e.ID)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return p.tx(ctx, func(tx *sql.Tx) error {
				recordDivergence(tx, op.path, m.name, "delete-skipped", "the directory is not empty on the member; its content reappears as adopted", p.now().UnixNano())
				return nil
			})
		}
	} else if want := op.args["ctoken"]; want != "" {
		ht, hv := bestHash(e.Hashes)
		got := timeToken(e.Size, e.ModTime)
		if ht != "" {
			got = hashToken(ht, hv)
		}
		if got != want && !sameToken(want, e) {
			return p.tx(ctx, func(tx *sql.Tx) error {
				recordDivergence(tx, op.path, m.name, "delete-skipped", "the content changed on the member after the delete; it is kept and reappears as adopted", p.now().UnixNano())
				return nil
			})
		}
	}
	err = m.p.Delete(ctx, e.ID)
	if errors.Is(err, provider.ErrNotFound) {
		err = nil
	}
	m.note(err)
	if err == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = p.tx(ctx, func(tx *sql.Tx) error { return dropSubtree(tx, op.path) })
	}
	return err
}

// sameToken reports whether an observation could carry the token: a
// hash token matches on the hash it names; a time token is recomputed.
func sameToken(token string, e provider.Entry) bool {
	for _, ht := range preferredHashes {
		if v := e.Hashes[ht]; v != "" && hashToken(ht, v) == token {
			return true
		}
	}
	return timeToken(e.Size, e.ModTime) == token
}

// PendingOps counts the ops waiting per member, for status.
func (p *Pool) PendingOps(ctx context.Context) (map[string]int, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, COUNT(*) FROM pending_ops WHERE state = 'pending' GROUP BY member`)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var m string
		var n int
		if err := rows.Scan(&m, &n); err != nil {
			return nil, err
		}
		out[m] = n
	}
	return out, rows.Err()
}
