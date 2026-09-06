package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"cloudfs/internal/provider"
)

// replicasOf lists the live replicas of a file carrying the given content
// token, in member declaration order.
func (p *Pool) replicasOf(ctx context.Context, pth, ctoken string) ([]replicaRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT path, parent, member, remote_id, version, size, mtime_ns, hash_type, hash, ctoken, state, member_name FROM replicas WHERE path = ? AND state IN ('live', 'pending')`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []replicaRow
	for rows.Next() {
		var r replicaRow
		if err := rows.Scan(&r.path, &r.parent, &r.member, &r.remoteID, &r.version, &r.size, &r.mtimeNS, &r.hashType, &r.hash, &r.ctoken, &r.state, &r.memberName); err != nil {
			return nil, fmt.Errorf("pool: %w", err)
		}
		if ctoken != "" && r.ctoken != ctoken {
			continue
		}
		// A row naming a member the pool no longer has is not a replica:
		// there is no provider to read it through, and repair already
		// counts it as missing. Dropping it here keeps every caller — and
		// the ranking below, which reaches into the member — on members
		// that exist.
		if p.byName[r.member] == nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Healthy members first, then the fastest, then declaration order.
	sortReplicas(out, p.byName)
	return out, nil
}

func sortReplicas(rs []replicaRow, byName map[string]*member) {
	sort.SliceStable(rs, func(i, j int) bool {
		ri, li, oi := byName[rs[i].member].rank()
		rj, lj, oj := byName[rs[j].member].rank()
		if ri != rj {
			return ri < rj
		}
		if li != lj {
			// An unmeasured member sorts first: it gets its chance to be
			// the fastest.
			return li < lj
		}
		return oi < oj
	})
}

// resolveFile finds the file at id and the replicas that can serve version.
func (p *Pool) resolveFile(ctx context.Context, id, version string) (string, entryRow, []replicaRow, error) {
	pth, err := p.pathOf(ctx, id)
	if err != nil {
		return "", entryRow{}, nil, err
	}
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return pth, row, nil, err
	}
	if !ok || row.kind != provider.KindFile {
		return pth, row, nil, fmt.Errorf("%w: %s", provider.ErrNotFound, pth)
	}
	if version != "" && row.ctoken != version {
		return pth, row, nil, fmt.Errorf("%w: %s changed", provider.ErrConflict, pth)
	}
	reps, err := p.replicasOf(ctx, pth, row.ctoken)
	return pth, row, reps, err
}

// tryReplicas runs one read against the replicas in order until one serves
// it. A replica whose member is unreachable is skipped; one whose member no
// longer has the file is forgotten; one whose member reports a different
// version means the content changed under us and the caller must refresh.
func (p *Pool) tryReplicas(ctx context.Context, pth string, reps []replicaRow, do func(m *member, r replicaRow) error) error {
	if len(reps) == 0 {
		return fmt.Errorf("%w: %s has no replica", provider.ErrNotFound, pth)
	}
	var lastUnreachable error
	probe := p.probeInterval()
	for _, r := range reps {
		m := p.byName[r.member]
		if m == nil {
			continue
		}
		if !m.usable(probe) {
			lastUnreachable = fmt.Errorf("member %s is %s", m.name, m.state())
			continue
		}
		start := time.Now()
		err := do(m, r)
		if err == nil {
			m.note(nil)
			m.noteLatency(time.Since(start))
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if errors.Is(err, provider.ErrUnsupported) {
			// Nothing was asked of the member; the caller falls back to
			// another path. Says nothing about health.
			return err
		}
		switch {
		case unreachable(err):
			m.note(err)
			lastUnreachable = err
		case errors.Is(err, provider.ErrNotFound):
			m.note(nil)
			_, _ = p.db.ExecContext(ctx, `UPDATE replicas SET state = 'missing' WHERE path = ? AND member = ?`, r.path, r.member)
		case errors.Is(err, provider.ErrConflict):
			m.note(nil)
			return err
		default:
			m.note(nil)
			return err
		}
	}
	if lastUnreachable != nil {
		return fmt.Errorf("%w: %s (last: %v)", provider.ErrUnavailable, pth, lastUnreachable)
	}
	return fmt.Errorf("%w: every replica of %s is gone", provider.ErrNotFound, pth)
}

func (p *Pool) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	pth, _, reps, err := p.resolveFile(ctx, id, version)
	if err != nil {
		return nil, err
	}
	var rc io.ReadCloser
	err = p.tryReplicas(ctx, pth, reps, func(m *member, r replicaRow) error {
		var err error
		rc, err = m.p.ReadRange(ctx, r.remoteID, r.version, off, n)
		return err
	})
	return rc, err
}

// ReadRangeAt implements provider.RangeReaderAt when the member serving the
// read does; otherwise it reports ErrUnsupported and the VFS falls back.
func (p *Pool) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	pth, _, reps, err := p.resolveFile(ctx, id, version)
	if err != nil {
		return 0, err
	}
	var n int
	err = p.tryReplicas(ctx, pth, reps, func(m *member, r replicaRow) error {
		ra, ok := m.p.(provider.RangeReaderAt)
		if !ok {
			return provider.ErrUnsupported
		}
		var err error
		n, err = ra.ReadRangeAt(ctx, r.remoteID, r.version, off, buf)
		return err
	})
	return n, err
}

func (p *Pool) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	pth, _, reps, err := p.resolveFile(ctx, id, "")
	if err != nil {
		return provider.Link{}, err
	}
	var link provider.Link
	err = p.tryReplicas(ctx, pth, reps, func(m *member, r replicaRow) error {
		var err error
		link, err = m.p.DownloadURL(ctx, r.remoteID)
		return err
	})
	return link, err
}
