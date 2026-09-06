package pool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"

	"cloudfs/internal/provider"
)

// The pool's change feed is its members' feeds, folded through the index:
// a change on a member names a member id; the index says which pool path
// that is; the merged listing of that path's directory says what the pool
// now shows there. What comes out carries the pool's own ids and content
// tokens, so an echo of this machine's own write — the member reporting
// the upload we made — arrives with the id and version the VFS already
// holds and is a no-op there. That is what lets Caps.Delta be true without
// the refresher and the pool feeding each other.
//
// The pool's cursor is the members' cursors, one each. A member whose
// cursor is reset is re-listed by the scrub; nothing invalidates the whole
// mount for one member's lost place.

type deltaCursor map[string]string

func parseCursor(s string) deltaCursor {
	c := deltaCursor{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &c)
	}
	return c
}

func (c deltaCursor) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// Changes implements provider.ChangeLister.
func (p *Pool) Changes(ctx context.Context, cursor string) ([]provider.Change, string, error) {
	cur := parseCursor(cursor)
	next := deltaCursor{}
	for k, v := range cur {
		next[k] = v
	}
	probe := p.probeInterval()
	type memberEvent struct {
		m *member
		e provider.Change
	}
	var events []memberEvent
	for _, m := range p.members {
		cl, ok := m.p.(provider.ChangeLister)
		if !ok || !m.p.Capabilities().Delta {
			continue
		}
		if !m.usable(probe) {
			continue // keeps its cursor; nothing is lost
		}
		evs, nc, err := cl.Changes(ctx, cur[m.name])
		var reset *provider.CursorResetError
		if errors.As(err, &reset) {
			// The member lost our place. Its directories are re-listed by
			// the scrub; the rest of the pool is not disturbed.
			m.note(nil)
			m.mu.Lock()
			m.needsScrub = true
			m.mu.Unlock()
			next[m.name] = reset.Cursor
			continue
		}
		if err != nil {
			m.note(err)
			continue
		}
		m.note(nil)
		if nc != "" {
			next[m.name] = nc
		}
		for _, e := range evs {
			events = append(events, memberEvent{m, e})
		}
	}

	// Which directories the events touch, and which paths to report. A
	// member's event names a member id; the index maps it to a path. An id
	// the index does not know yet (a directory created in the app, and the
	// file inside it) becomes known once its parent is re-listed, so
	// resolution goes round until nothing new resolves.
	type touched struct {
		parent string
		name   string
		known  string // the path the index knew for the id, "" if none
	}
	var work []touched
	listed := map[string][]provider.Entry{}
	pending := events
	for round := 0; round < 3 && len(pending) > 0; round++ {
		var unresolved []memberEvent
		dirs := map[string]bool{}
		for _, ev := range pending {
			known := p.pathByMemberID(ctx, ev.m.name, ev.e.ID)
			var parent, name string
			switch {
			case known != "":
				parent, name = parentOf(known), path.Base(known)
			case ev.e.Entry != nil:
				pp := p.dirPathByMemberID(ctx, ev.m.name, ev.e.ParentID)
				if pp == "" {
					pp = p.dirPathByMemberID(ctx, ev.m.name, ev.e.Entry.ParentID)
				}
				if pp == "" {
					unresolved = append(unresolved, ev)
					continue
				}
				if hidden(ev.e.Entry.Name) {
					continue
				}
				parent, name = pp, ev.e.Entry.Name
			default:
				continue
			}
			if _, done := listed[parent]; !done {
				dirs[parent] = true
			}
			work = append(work, touched{parent: parent, name: name, known: known})
		}
		for d := range dirs {
			entries, err := p.listDir(ctx, d)
			if err != nil && !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrUnavailable) {
				return nil, cursor, err
			}
			listed[d] = entries
		}
		if len(unresolved) == len(pending) {
			break // nothing new resolved; the rest is somewhere unlisted
		}
		pending = unresolved
	}

	var out []provider.Change
	seen := map[string]bool{}
	emit := func(c provider.Change) {
		if seen[c.ID] {
			return
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	for _, w := range work {
		parentID, err := p.idFor(ctx, w.parent)
		if err != nil {
			return nil, cursor, err
		}
		entries := listed[w.parent]
		// The known path first: gone from the merged listing means deleted.
		if w.known != "" {
			id, err := p.idFor(ctx, w.known)
			if err != nil {
				return nil, cursor, err
			}
			if e, ok := findEntry(entries, path.Base(w.known)); ok {
				e.ParentID = parentID
				emit(provider.Change{Op: provider.ChangeUpsert, ID: id, ParentID: parentID, Entry: &e})
			} else {
				emit(provider.Change{Op: provider.ChangeDelete, ID: id, ParentID: parentID})
			}
		}
		// A name the member reported that the merged listing now shows.
		if e, ok := findEntry(entries, w.name); ok {
			e.ParentID = parentID
			emit(provider.Change{Op: provider.ChangeUpsert, ID: e.ID, ParentID: parentID, Entry: &e})
		} else if w.known == "" {
			// Not shown (hidden, refused, or surfaced under a conflict
			// name): tell the VFS the directory moved on.
			id, err := p.idFor(ctx, joinPath(w.parent, w.name))
			if err != nil {
				return nil, cursor, err
			}
			emit(provider.Change{Op: provider.ChangeDelete, ID: id, ParentID: parentID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, next.String(), nil
}

func findEntry(entries []provider.Entry, name string) (provider.Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return provider.Entry{}, false
}

// pathByMemberID finds the pool path of a member's file or directory id.
func (p *Pool) pathByMemberID(ctx context.Context, member, id string) string {
	if id == "" {
		return ""
	}
	var pth string
	err := p.db.QueryRowContext(ctx, `SELECT path FROM replicas WHERE member = ? AND remote_id = ? LIMIT 1`, member, id).Scan(&pth)
	if err == nil {
		return pth
	}
	if err != sql.ErrNoRows {
		return ""
	}
	return p.dirPathByMemberID(ctx, member, id)
}

func (p *Pool) dirPathByMemberID(ctx context.Context, member, id string) string {
	if id == "" {
		return ""
	}
	var pth string
	if err := p.db.QueryRowContext(ctx, `SELECT path FROM member_dirs WHERE member = ? AND remote_id = ? LIMIT 1`, member, id).Scan(&pth); err == nil {
		return pth
	}
	// The member's own root is the pool root when it is the configured
	// root of that member.
	if m := p.byName[member]; m != nil {
		if rid, ok := p.cachedDirID(ctx, m, "/"); ok && rid == id {
			return "/"
		}
	}
	return ""
}

var (
	_ provider.ChangeLister = (*Pool)(nil)
	_                       = fmt.Sprintf
)
