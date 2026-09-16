package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Read heat (docs/agent-first-design.md §6.3, TODO.md T-53). The daemon
// knows everything about writes and, until now, nothing about reads; yet
// what agents read is the team's working set, and a file read often but
// changed long ago is the one whose staleness matters. The read_heat
// table counts reads per path, per day, per kind of reader — agent,
// kernel, console, webdav — and never per identity: heat is about files,
// not about watching people. Reads are debounced in memory for ten
// minutes per (path, kind) before they become a row, so a sequential read
// of a large file, or an agent re-reading the file it edits, counts once.

// Actor kinds a read is counted under.
const (
	ReadByAgent   = "agent"
	ReadByKernel  = "kernel"
	ReadByConsole = "console"
	ReadByWebDAV  = "webdav"
)

// ReadSample is one debounced read.
type ReadSample struct {
	Path      string
	ActorKind string
	TS        time.Time
	Count     int
}

// HotPath is one row of HotPaths: a path and its reads over the window.
type HotPath struct {
	Path     string         `json:"path"`
	Reads    int64          `json:"reads"`
	ByKind   map[string]int `json:"by_kind"`
	LastRead time.Time      `json:"last_read"`
}

// readHeatDefaultDays is the window HotPaths uses when given 0.
const readHeatDefaultDays = 7

// dayOf is the UTC day number a timestamp belongs to.
func dayOf(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// BumpReadHeat adds samples to the table in one transaction.
func (s *Store) BumpReadHeat(ctx context.Context, samples []ReadSample) error {
	if len(samples) == 0 {
		return nil
	}
	if s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	for _, r := range samples {
		ts := r.TS
		if ts.IsZero() {
			ts = s.now()
		}
		n := r.Count
		if n <= 0 {
			n = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO read_heat(path, day, actor_kind, count, last_ts) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(path, day, actor_kind) DO UPDATE SET count = count + excluded.count, last_ts = MAX(last_ts, excluded.last_ts)`,
			Normalise(r.Path), dayOf(ts), r.ActorKind, n, ts.UnixNano()); err != nil {
			return fmt.Errorf("agent: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// HotPaths lists the most-read paths at or under prefix over the last
// days (default 7), hottest first, at most limit (default 50).
func (s *Store) HotPaths(ctx context.Context, prefix string, days, limit int) ([]HotPath, error) {
	if days <= 0 {
		days = readHeatDefaultDays
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	since := dayOf(s.now()) - int64(days) + 1
	where := []string{"day >= ?"}
	args := []any{since}
	if p := Normalise(prefix); p != "/" {
		where = append(where, "(path = ? OR path LIKE ? ESCAPE '\\')")
		args = append(args, p, likePrefix(p)+"/%")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, actor_kind, SUM(count), MAX(last_ts) FROM read_heat WHERE `+strings.Join(where, " AND ")+
		` GROUP BY path, actor_kind`, args...)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	byPath := map[string]*HotPath{}
	for rows.Next() {
		var p, kind string
		var n, last int64
		if err := rows.Scan(&p, &kind, &n, &last); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		h := byPath[p]
		if h == nil {
			h = &HotPath{Path: p, ByKind: map[string]int{}}
			byPath[p] = h
		}
		h.Reads += n
		h.ByKind[kind] += int(n)
		if t := time.Unix(0, last); t.After(h.LastRead) {
			h.LastRead = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	out := make([]HotPath, 0, len(byPath))
	for _, h := range byPath {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reads != out[j].Reads {
			return out[i].Reads > out[j].Reads
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneReadHeat drops day buckets older than days (default 90).
func (s *Store) PruneReadHeat(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		days = 90
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM read_heat WHERE day < ?`, dayOf(s.now())-int64(days))
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// readDebounce is how long one (path, kind) pair's reads fold into one
// sample; readFlushEvery how often pending samples become rows.
const (
	readDebounce   = 10 * time.Minute
	readFlushEvery = 30 * time.Second
)

// ReadObserver is the in-memory front of the table: Observe is cheap and
// never touches the database; Flush writes what accumulated.
type ReadObserver struct {
	store *Store
	now   func() time.Time

	mu      sync.Mutex
	last    map[readKey]time.Time
	pending map[readKey]ReadSample
}

type readKey struct{ path, kind string }

// NewReadObserver makes an observer over store.
func NewReadObserver(store *Store) *ReadObserver {
	return &ReadObserver{store: store, now: store.now, last: map[readKey]time.Time{}, pending: map[readKey]ReadSample{}}
}

// Observe records one read of path by kind, debounced.
func (o *ReadObserver) Observe(path, kind string) {
	if path == "" || kind == "" {
		return
	}
	k := readKey{Normalise(path), kind}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if t, ok := o.last[k]; ok && now.Sub(t) < readDebounce {
		return
	}
	o.last[k] = now
	s := o.pending[k]
	s.Path, s.ActorKind, s.TS, s.Count = k.path, k.kind, now, s.Count+1
	o.pending[k] = s
}

// Pending reports how many samples wait for a flush.
func (o *ReadObserver) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// Flush writes the pending samples and drops debounce entries older
// than the window.
func (o *ReadObserver) Flush(ctx context.Context) error {
	o.mu.Lock()
	batch := make([]ReadSample, 0, len(o.pending))
	for _, s := range o.pending {
		batch = append(batch, s)
	}
	o.pending = map[readKey]ReadSample{}
	now := o.now()
	for k, t := range o.last {
		if now.Sub(t) >= readDebounce {
			delete(o.last, k)
		}
	}
	o.mu.Unlock()
	return o.store.BumpReadHeat(ctx, batch)
}

// Run flushes every interval until ctx ends, then once more, in the
// owner only: a stdio process beside the mount reports through its own
// audit rows, not the shared heat table, or the same read would count
// twice.
func (o *ReadObserver) Run(ctx context.Context, every time.Duration) {
	if !o.store.owner {
		return
	}
	if every <= 0 {
		every = readFlushEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := o.Flush(context.WithoutCancel(ctx)); err != nil {
				slog.Warn("agent: read heat not flushed", "err", err)
			}
			return
		case <-ticker.C:
			if err := o.Flush(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("agent: read heat not flushed", "err", err)
			}
		}
	}
}
