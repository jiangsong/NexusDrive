package pool

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
)

// Placement decides which members hold a file. A member is eligible when
// it is in service, can spell the name and has room; among the eligible,
// the ones already holding the path come first (an overwrite lands where
// the file lives), then those with the most free space, weight breaking
// ties, declaration order last. Never two replicas on one member — a
// member is one drive, and one drive failing must cost one replica.

// quotaTTL is how long a member's reported space is trusted.
const quotaTTL = time.Minute

type spaceInfo struct {
	mu      sync.Mutex
	quota   provider.Quota
	known   bool
	fetched time.Time
}

// free reports the member's free bytes, or -1 when nobody can say. The
// backend's own figure is preferred; a configured capacity minus what the
// pool has placed there is the fallback.
func (p *Pool) free(ctx context.Context, m *member) int64 {
	m.space.mu.Lock()
	fresh := m.space.known && p.now().Sub(m.space.fetched) < quotaTTL
	q := m.space.quota
	m.space.mu.Unlock()
	if !fresh {
		quota, ok, err := provider.QuotaOf(ctx, m.p)
		m.space.mu.Lock()
		m.space.fetched = p.now()
		if err == nil && ok && quota.Total > 0 {
			m.space.quota, m.space.known = quota, true
			q = quota
		} else {
			m.space.known = false
			q = provider.Quota{}
		}
		m.space.mu.Unlock()
	}
	if q.Total > 0 {
		return q.Free()
	}
	if m.capacity > 0 {
		var placed sql.NullInt64
		_ = p.db.QueryRowContext(ctx, `SELECT SUM(size) FROM replicas WHERE member = ?`, m.name).Scan(&placed)
		free := m.capacity - placed.Int64
		if free < 0 {
			free = 0
		}
		return free
	}
	return -1
}

// naming is the member's declared rules plus what it has refused at run
// time.
func (p *Pool) naming(ctx context.Context, m *member) provider.Naming {
	n := m.p.Capabilities().Naming
	m.mu.Lock()
	learned := m.learned
	m.mu.Unlock()
	if learned == nil {
		learned = p.loadLearned(ctx, m)
	}
	for _, pat := range learned {
		switch {
		case strings.HasPrefix(pat, "chars:"):
			for _, r := range strings.TrimPrefix(pat, "chars:") {
				if !strings.ContainsRune(n.ForbiddenRunes, r) {
					n.ForbiddenRunes += string(r)
				}
			}
		case pat == "trailing":
			n.NoTrailingDotSpace = true
		case strings.HasPrefix(pat, "exact:"):
			n.ReservedNames = append(n.ReservedNames, strings.TrimPrefix(pat, "exact:"))
		}
	}
	return n
}

func (p *Pool) loadLearned(ctx context.Context, m *member) []string {
	rows, err := p.db.QueryContext(ctx, `SELECT pattern FROM member_naming WHERE member = ?`, m.name)
	learned := []string{}
	if err == nil {
		for rows.Next() {
			var pat string
			if rows.Scan(&pat) == nil {
				learned = append(learned, pat)
			}
		}
		rows.Close()
	}
	m.mu.Lock()
	m.learned = learned
	m.mu.Unlock()
	return learned
}

// learnDenial records that the member refused name, as a pattern broad
// enough to spare the next name of the same kind and narrow enough not to
// refuse everything.
func (p *Pool) learnDenial(ctx context.Context, m *member, name string) {
	pat := "exact:" + name
	if bad := strings.Map(func(r rune) rune {
		if strings.ContainsRune(provider.WindowsForbiddenRunes, r) {
			return r
		}
		return -1
	}, name); bad != "" {
		pat = "chars:" + bad
	} else if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		pat = "trailing"
	}
	_, _ = p.db.ExecContext(ctx, `INSERT OR IGNORE INTO member_naming(member, pattern, learned_at) VALUES(?, ?, ?)`, m.name, pat, p.now().UnixNano())
	m.mu.Lock()
	m.learned = append(m.learned, pat)
	m.mu.Unlock()
}

// refusedName reports whether an error from a member means it will not
// hold the name (as opposed to being unreachable, or the name existing).
func refusedName(err error) bool {
	if err == nil || unreachable(err) {
		return false
	}
	return isBadName(err)
}

// canHold reports whether the member can take name under parent: the rules
// allow it, and no other case of it is already there when the member
// folds case.
func (p *Pool) canHold(ctx context.Context, m *member, parent, name string) error {
	n := p.naming(ctx, m)
	if err := provider.CheckName(n, name); err != nil {
		return err
	}
	if n.CaseInsensitive {
		rows, err := p.db.QueryContext(ctx, `SELECT member_name FROM replicas WHERE member = ? AND parent = ? UNION SELECT path FROM member_dirs WHERE member = ? AND parent = ?`, m.name, parent, m.name, parent)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var other string
				if rows.Scan(&other) == nil {
					if i := strings.LastIndex(other, "/"); i >= 0 && strings.HasPrefix(other, "/") {
						other = other[i+1:]
					}
					if other != name && strings.EqualFold(other, name) {
						return fmt.Errorf("%w: %q and %q cannot coexist on a case-folding member", provider.ErrBadName, name, other)
					}
				}
			}
		}
	}
	return nil
}

// candidates orders the members for placing a replica of pth.
func (p *Pool) candidates(ctx context.Context, pth string) []*member {
	parent, name := parentOf(pth), pth[strings.LastIndex(pth, "/")+1:]
	holding := map[string]bool{}
	rows, err := p.db.QueryContext(ctx, `SELECT member FROM replicas WHERE path = ?`, pth)
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				holding[m] = true
			}
		}
		rows.Close()
	}
	probe := p.probeInterval()
	type scored struct {
		m    *member
		free int64
	}
	var holders, others []scored
	for _, m := range p.members {
		if st := m.state(); st == provider.HealthDraining || st == provider.HealthDisabled {
			continue
		}
		if !m.usable(probe) {
			continue
		}
		if !holding[m.name] && p.canHold(ctx, m, parent, name) != nil {
			continue
		}
		sc := scored{m: m, free: p.free(ctx, m)}
		if holding[m.name] {
			holders = append(holders, sc)
		} else {
			others = append(others, sc)
		}
	}
	order := func(s []scored) {
		sort.SliceStable(s, func(i, j int) bool {
			fi, fj := s[i].free, s[j].free
			if (fi < 0) != (fj < 0) {
				return fi >= 0 // known space before unknown
			}
			if fi != fj && fi >= 0 {
				// Weight scales what "more room" means.
				return float64(fi)*s[i].m.weight > float64(fj)*s[j].m.weight
			}
			if s[i].m.weight != s[j].m.weight {
				return s[i].m.weight > s[j].m.weight
			}
			return s[i].m.order < s[j].m.order
		})
	}
	order(holders)
	order(others)
	out := make([]*member, 0, len(holders)+len(others))
	for _, s := range holders {
		out = append(out, s.m)
	}
	for _, s := range others {
		out = append(out, s.m)
	}
	return out
}

// Quota implements provider.Quotaer for the pool: what the members report
// together, divided by the replica count, is the space a file costs once.
func (p *Pool) Quota(ctx context.Context) (provider.Quota, error) {
	var total, used int64
	known := 0
	for _, m := range p.members {
		if st := m.state(); st == provider.HealthDisabled || st == provider.HealthDraining || st == provider.HealthOut {
			continue
		}
		m.space.mu.Lock()
		fresh := m.space.known && p.now().Sub(m.space.fetched) < quotaTTL
		q := m.space.quota
		m.space.mu.Unlock()
		if !fresh {
			p.free(ctx, m)
			m.space.mu.Lock()
			q = m.space.quota
			if !m.space.known {
				q = provider.Quota{}
			}
			m.space.mu.Unlock()
		}
		if q.Total <= 0 && m.capacity > 0 {
			var placed sql.NullInt64
			_ = p.db.QueryRowContext(ctx, `SELECT SUM(size) FROM replicas WHERE member = ?`, m.name).Scan(&placed)
			q = provider.Quota{Total: m.capacity, Used: placed.Int64}
		}
		if q.Total <= 0 {
			continue
		}
		known++
		total += q.Total
		used += q.Used
	}
	if known == 0 {
		return provider.Quota{}, provider.ErrUnsupported
	}
	r := int64(p.settings.Replicas)
	if r < 1 {
		r = 1
	}
	return provider.Quota{Total: total / r, Used: used / r}, nil
}

var _ provider.Quotaer = (*Pool)(nil)
