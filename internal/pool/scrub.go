package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"cloudfs/internal/provider"
)

// Scrub is how the pool notices what happened behind its back: a file
// deleted or edited in the vendor's app, a copy gone from a member, a
// member that missed more ops than the log kept. It reconciles by
// re-listing the directories concerned — the listing already knows how to
// merge, surface conflicts and record divergences — so scrub never has a
// second opinion about the truth.

// ScrubOnce re-lists a sample of the directories the index knows, and every
// directory of a member flagged for a full scrub. It reports how many
// directories it looked at.
func (p *Pool) ScrubOnce(ctx context.Context) (int, error) {
	full := map[string]bool{}
	for _, m := range p.members {
		m.mu.Lock()
		if m.needsScrub && m.health.Snapshot().Usable() {
			full[m.name] = true
			m.needsScrub = false
		}
		m.mu.Unlock()
	}
	var dirs []string
	seen := map[string]bool{"/": true}
	dirs = append(dirs, "/")
	rows, err := p.db.QueryContext(ctx, `SELECT DISTINCT path, member FROM member_dirs WHERE path <> '/' ORDER BY path`)
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	sample := p.settings.ScrubSample
	for rows.Next() {
		var pth, member string
		if err := rows.Scan(&pth, &member); err != nil {
			rows.Close()
			return 0, err
		}
		if seen[pth] {
			continue
		}
		if full[member] || sample >= 1 || rand.Float64() < sample {
			seen[pth] = true
			dirs = append(dirs, pth)
		}
	}
	rows.Close()
	looked := 0
	for _, d := range dirs {
		if ctx.Err() != nil {
			return looked, ctx.Err()
		}
		if _, err := p.listDir(ctx, d); err != nil && !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrUnavailable) {
			return looked, err
		}
		looked++
	}
	// What the re-listing found missing is queued for repair.
	if p.anyMultiReplica() {
		if _, err := p.ScanOnce(ctx); err != nil {
			return looked, err
		}
	}
	return looked, nil
}

// ScrubPath re-lists one directory (or the parent of a file) on demand.
func (p *Pool) ScrubPath(ctx context.Context, pth string) error {
	pth, err := cleanPath(pth)
	if err != nil {
		return err
	}
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return err
	}
	if ok && row.kind != provider.KindDir {
		pth = parentOf(pth)
	}
	_, err = p.listDir(ctx, pth)
	return err
}

// TrimOnce deletes surplus replicas: a file with more live copies than the
// target loses them from the last-declared members, but only when every
// copy is known by hash to be the same content and the surplus has been
// seen for longer than trim_grace — two machines sharing the members may
// both be repairing, and a copy that just appeared may be the other's
// work in progress.
func (p *Pool) TrimOnce(ctx context.Context) (int, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT path, ctoken FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile))
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	type file struct{ path, ctoken string }
	var files []file
	for rows.Next() {
		var f file
		if err := rows.Scan(&f.path, &f.ctoken); err != nil {
			rows.Close()
			return 0, err
		}
		files = append(files, f)
	}
	rows.Close()
	trimmed := 0
	grace := p.settings.TrimGrace
	cutoff := p.now().Add(-grace).UnixNano()
	probe := p.probeInterval()
	for _, f := range files {
		if len(f.ctoken) < 3 || f.ctoken[:3] != "h1:" {
			continue // only a hash proves two copies are one content
		}
		live, err := p.liveReplicas(ctx, f.path, f.ctoken)
		if err != nil {
			return trimmed, err
		}
		target := p.wantReplicas(f.path)
		if len(live) <= target {
			continue
		}
		// Newest-seen copies are the surplus candidates, last-declared
		// members first.
		for i := len(live) - 1; i >= 0 && len(live) > target; i-- {
			r := live[i]
			var seenAt int64
			_ = p.db.QueryRowContext(ctx, `SELECT seen_at FROM replicas WHERE path = ? AND member = ?`, r.path, r.member).Scan(&seenAt)
			if seenAt > cutoff {
				continue
			}
			m := p.byName[r.member]
			if m == nil || !m.usable(probe) {
				continue
			}
			err := m.p.Delete(ctx, r.remoteID)
			if errors.Is(err, provider.ErrNotFound) {
				err = nil
			}
			m.note(err)
			if err != nil {
				continue
			}
			_, _ = p.execIndex(ctx, `DELETE FROM replicas WHERE path = ? AND member = ?`, r.path, r.member)
			live = append(live[:i], live[i+1:]...)
			trimmed++
		}
	}
	return trimmed, nil
}

// Divergence is one thing the pool could not reconcile on its own.
type Divergence struct {
	Path   string
	Member string
	Kind   string
	Detail string
	SeenAt time.Time
}

// Divergences lists what needs a person's decision, newest first.
func (p *Pool) Divergences(ctx context.Context, limit int) ([]Divergence, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := p.db.QueryContext(ctx, `SELECT path, member, kind, detail, seen_at FROM divergences ORDER BY seen_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []Divergence
	for rows.Next() {
		var d Divergence
		var at int64
		if err := rows.Scan(&d.Path, &d.Member, &d.Kind, &d.Detail, &at); err != nil {
			return nil, err
		}
		d.SeenAt = time.Unix(0, at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearDivergence forgets one record once a person has dealt with it.
func (p *Pool) ClearDivergence(ctx context.Context, pth, member, kind string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM divergences WHERE path = ? AND member = ? AND kind = ?`, pth, member, kind)
	return err
}

var _ = sql.ErrNoRows
