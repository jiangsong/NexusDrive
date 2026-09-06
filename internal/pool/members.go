package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// member is one backend inside the pool, with what the pool has learnt
// about it at runtime.
type member struct {
	name     string
	p        provider.Provider
	root     string
	weight   float64
	capacity int64
	adopt    bool
	order    int

	mu sync.Mutex
	// dirIDs caches path → directory id on this member; the member_dirs
	// table is the durable copy.
	dirIDs   map[string]string
	lastOK   time.Time
	lastErr  error
	lastTry  time.Time
	failures int
}

// note records the outcome of one call for health tracking.
func (m *member) note(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTry = time.Now()
	if err == nil {
		m.lastOK = m.lastTry
		m.lastErr = nil
		m.failures = 0
		return
	}
	switch retry.Classify(err) {
	case retry.ClassRetryable, retry.ClassAuth, retry.ClassRiskControl:
		m.lastErr = err
		m.failures++
	}
}

// unreachable reports whether err means the member could not be talked to,
// as opposed to answering about the data.
func unreachable(err error) bool {
	switch retry.Classify(err) {
	case retry.ClassRetryable, retry.ClassAuth, retry.ClassRiskControl:
		return true
	}
	return false
}

// listAll enumerates one directory on the member completely, through the
// streaming interface when it offers one and by paging otherwise.
func (m *member) listAll(ctx context.Context, dirID string) ([]provider.Entry, error) {
	var out []provider.Entry
	if sl, ok := m.p.(provider.StreamLister); ok && m.p.Capabilities().StreamList {
		delivered := false
		err := sl.ListStream(ctx, dirID, func(e provider.Entry) error {
			delivered = true
			out = append(out, e)
			return nil
		})
		if err == nil {
			m.note(nil)
			return out, nil
		}
		if delivered || !errors.Is(err, provider.ErrUnsupported) {
			m.note(err)
			return nil, err
		}
		out = out[:0]
	}
	cursor := ""
	for {
		entries, next, err := m.p.List(ctx, dirID, cursor)
		if err != nil {
			m.note(err)
			return nil, err
		}
		out = append(out, entries...)
		if next == "" {
			m.note(nil)
			return out, nil
		}
		cursor = next
	}
}

// rootDirID resolves the member's configured root to a directory id on the
// member, walking from the provider's own root. A missing segment is not
// created here: reading never changes a member.
func (p *Pool) rootDirID(ctx context.Context, m *member) (string, error) {
	if id, ok := p.cachedDirID(ctx, m, "/"); ok {
		return id, nil
	}
	id := provider.RootOf(m.p)
	if m.root != "/" {
		for _, seg := range strings.Split(strings.Trim(m.root, "/"), "/") {
			entries, err := m.listAll(ctx, id)
			if err != nil {
				return "", err
			}
			found := ""
			for _, e := range entries {
				if e.Name == seg && e.Kind == provider.KindDir {
					found = e.ID
					break
				}
			}
			if found == "" {
				return "", fmt.Errorf("%w: pool root %s on member %s", provider.ErrNotFound, m.root, m.name)
			}
			id = found
		}
	}
	p.rememberDirID(ctx, m, "/", id)
	return id, nil
}

// dirID resolves a pool path to the directory id on one member. It answers
// from the index when it can, and otherwise enumerates the parent on the
// member (learning every sibling directory on the way). ErrNotFound means
// the member does not hold that directory — known without a call when the
// parent was already enumerated on that member.
func (p *Pool) dirID(ctx context.Context, m *member, pth string) (string, error) {
	if pth == "/" {
		return p.rootDirID(ctx, m)
	}
	if id, ok := p.cachedDirID(ctx, m, pth); ok {
		return id, nil
	}
	parent := parentOf(pth)
	if p.parentEnumerated(ctx, m, parent) {
		return "", fmt.Errorf("%w: %s on member %s", provider.ErrNotFound, pth, m.name)
	}
	parentID, err := p.dirID(ctx, m, parent)
	if err != nil {
		return "", err
	}
	if id, ok := p.cachedDirID(ctx, m, pth); ok {
		return id, nil
	}
	entries, err := m.listAll(ctx, parentID)
	if err != nil {
		return "", err
	}
	found := ""
	for _, e := range entries {
		if e.Kind != provider.KindDir {
			continue
		}
		p.rememberDirID(ctx, m, joinPath(parent, e.Name), e.ID)
		if e.Name == pth[strings.LastIndex(pth, "/")+1:] {
			found = e.ID
		}
	}
	p.markEnumerated(ctx, m, parent)
	if found == "" {
		return "", fmt.Errorf("%w: %s on member %s", provider.ErrNotFound, pth, m.name)
	}
	return found, nil
}

// parentEnumerated reports whether dir's children were enumerated on m, so
// the absence of a subdirectory from the index is knowledge, not ignorance.
func (p *Pool) parentEnumerated(ctx context.Context, m *member, dir string) bool {
	var listed int64
	if err := p.db.QueryRowContext(ctx, `SELECT listed_at FROM member_dirs WHERE member = ? AND path = ?`, m.name, dir).Scan(&listed); err != nil {
		return false
	}
	return listed > 0
}

func (p *Pool) markEnumerated(ctx context.Context, m *member, dir string) {
	_, _ = p.db.ExecContext(ctx, `UPDATE member_dirs SET listed_at = ? WHERE member = ? AND path = ?`, p.now().UnixNano(), m.name, dir)
}

func (p *Pool) cachedDirID(ctx context.Context, m *member, pth string) (string, bool) {
	m.mu.Lock()
	id, ok := m.dirIDs[pth]
	m.mu.Unlock()
	if ok {
		return id, true
	}
	err := p.db.QueryRowContext(ctx, `SELECT remote_id FROM member_dirs WHERE member = ? AND path = ?`, m.name, pth).Scan(&id)
	if err != nil {
		if err != sql.ErrNoRows {
			return "", false
		}
		return "", false
	}
	m.mu.Lock()
	m.dirIDs[pth] = id
	m.mu.Unlock()
	return id, true
}

func (p *Pool) rememberDirID(ctx context.Context, m *member, pth, id string) {
	m.mu.Lock()
	m.dirIDs[pth] = id
	m.mu.Unlock()
	_, _ = p.db.ExecContext(ctx, `INSERT INTO member_dirs(member, path, parent, remote_id, verified_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(member, path) DO UPDATE SET remote_id = excluded.remote_id, verified_at = excluded.verified_at`,
		m.name, pth, dirParent(pth), id, p.now().UnixNano())
}

func (p *Pool) forgetDirID(ctx context.Context, m *member, pth string) {
	m.mu.Lock()
	delete(m.dirIDs, pth)
	m.mu.Unlock()
	_, _ = p.db.ExecContext(ctx, `DELETE FROM member_dirs WHERE member = ? AND path = ?`, m.name, pth)
}
