package agent

import (
	"encoding/json"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/vfs"
)

// The changes table (docs/agent-first-design.md §6.1, TODO.md T-51) is
// the durable record of every change the VFS announced, whatever made it:
// a kernel write through the mount, an MCP tool (with its session), the
// control plane, WebDAV, or the remote itself as a listing or the delta
// feed found it. It answers "who last changed this file" (LastWriter),
// "what happened to it" (history) and "what changed since I last looked"
// (pull_events), which the in-memory change feed cannot: that feed is
// gone the moment nobody is listening. A rescan — the feed overflowed or
// a path could not be resolved — is a row of its own with reliable=0, so
// a reader knows the record around it may have gaps.

// Change is one row of the changes table.
type Change struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Path string    `json:"path"`
	// Kind is the vfs.ChangeKind name: write, create, mkdir, remove,
	// rename, remote, rescan. For a rename the row is the destination;
	// From holds the old path.
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	// Origin is kernel, mcp, control, webdav, remote — the adapter's own
	// name for an API change, the vfs.Origin name otherwise.
	Origin    string `json:"origin"`
	SessionID string `json:"session_id,omitempty"`
	Principal string `json:"principal,omitempty"`
	// Reliable is false on a rescan row: the feed lost changes before it.
	Reliable bool `json:"reliable"`
}

// changeRetainDefault is how long rows are kept when the caller passes 0.
const changeRetainDefault = 30 * 24 * time.Hour

// ChangesOf turns one feed event into rows: one per path (a rename is
// one row at the destination naming the source), a rescan one row at "/".
func ChangesOf(c vfs.Change, now time.Time) []Change {
	origin := c.Origin.String()
	if c.Origin == vfs.OriginAPI && c.OriginName != "" {
		origin = c.OriginName
	}
	if c.Rescan || c.Kind == vfs.KindRescan {
		return []Change{{TS: now, Path: "/", Kind: vfs.KindRescan.String(), Origin: origin, Reliable: false}}
	}
	base := Change{TS: now, Kind: c.Kind.String(), Origin: origin, SessionID: c.Actor.SessionID, Principal: c.Actor.Principal, Reliable: true}
	if c.Kind == vfs.KindRename && len(c.Paths) == 2 {
		row := base
		row.Path, row.From = Normalise(c.Paths[1]), Normalise(c.Paths[0])
		return []Change{row}
	}
	out := make([]Change, 0, len(c.Paths))
	for _, p := range c.Paths {
		row := base
		row.Path = Normalise(p)
		out = append(out, row)
	}
	return out
}

// RecordChanges appends rows in one transaction and returns the id of
// the last one. An empty batch is a no-op.
func (s *Store) RecordChanges(ctx context.Context, rows []Change) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if s.readOnly {
		return 0, errors.New("agent: the store is read-only")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	var last int64
	for _, r := range rows {
		ts := r.TS
		if ts.IsZero() {
			ts = s.now()
		}
		reliable := 0
		if r.Reliable {
			reliable = 1
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO changes(ts, path, kind, origin, session_id, principal, reliable, from_path)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ts.UnixNano(), r.Path, r.Kind, r.Origin, r.SessionID, r.Principal, reliable, r.From)
		if err != nil {
			return 0, fmt.Errorf("agent: %w", err)
		}
		last, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	return last, nil
}

// ChangesQuery selects rows for Changes.
type ChangesQuery struct {
	// After returns rows with id > After, oldest first: the cursor of a
	// pull_events reader. Zero starts from the beginning.
	After int64
	// Prefix keeps rows whose path is the prefix or under it; "" or "/"
	// keeps all. A rescan row always matches.
	Prefix string
	// Kinds keeps only these kinds; empty keeps all.
	Kinds []string
	// Limit bounds the page (default 200, max 1000).
	Limit int
}

// Changes returns rows after the cursor, oldest first, and whether more
// remain.
func (s *Store) Changes(ctx context.Context, q ChangesQuery) ([]Change, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"id > ?"}
	args := []any{q.After}
	if p := Normalise(q.Prefix); p != "/" && q.Prefix != "" {
		where = append(where, "(path = ? OR path LIKE ? ESCAPE '\\' OR kind = 'rescan')")
		args = append(args, p, likePrefix(p)+"/%")
	}
	if len(q.Kinds) > 0 {
		marks := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			marks[i] = "?"
			args = append(args, k)
		}
		where = append(where, "(kind IN ("+strings.Join(marks, ",")+") OR kind = 'rescan')")
	}
	args = append(args, limit+1)
	rows, err := s.queryChanges(ctx, changeColumns+` WHERE `+strings.Join(where, " AND ")+` ORDER BY id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil
}

// LastChangeID is the newest row's id, the cursor a reader that wants
// only future changes starts from.
func (s *Store) LastChangeID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM changes`).Scan(&id); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	return id, nil
}

// LastWriter is the newest reliable row for path, or false when the
// table has none: the file was last changed before the record began, or
// by a listing this daemon never saw.
func (s *Store) LastWriter(ctx context.Context, path string) (Change, bool, error) {
	rows, err := s.queryChanges(ctx, changeColumns+` WHERE path = ? AND reliable = 1 ORDER BY id DESC LIMIT 1`, Normalise(path))
	if err != nil || len(rows) == 0 {
		return Change{}, false, err
	}
	return rows[0], true, nil
}

// History returns the newest rows for path (and, for a directory, the
// paths directly under it), newest first, at most limit (default 50).
func (s *Store) History(ctx context.Context, path string, limit int) ([]Change, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	p := Normalise(path)
	if p == "/" {
		return s.queryChanges(ctx, changeColumns+` ORDER BY id DESC LIMIT ?`, limit)
	}
	return s.queryChanges(ctx, changeColumns+` WHERE path = ? OR from_path = ? OR path LIKE ? ESCAPE '\' ORDER BY id DESC LIMIT ?`,
		p, p, likePrefix(p)+"/%", limit)
}

// HistoryQuery pages History for a console that scrolls back in time.
type HistoryQuery struct {
	// Path is the file or directory; "" or "/" means everything.
	Path string
	// Before returns rows with id < Before, the cursor of the previous
	// page's oldest row; zero starts from the newest.
	Before int64
	// Limit bounds the page (default 50, max 500).
	Limit int
}

// HistoryPage is History with a cursor: the newest rows for the path
// (and, for a directory, the paths under it) older than Before, newest
// first, and whether more remain.
func (s *Store) HistoryPage(ctx context.Context, q HistoryQuery) ([]Change, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	where := []string{"1 = 1"}
	var args []any
	if p := Normalise(q.Path); p != "/" {
		where = append(where, `(path = ? OR from_path = ? OR path LIKE ? ESCAPE '\')`)
		args = append(args, p, p, likePrefix(p)+"/%")
	}
	if q.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, q.Before)
	}
	args = append(args, limit+1)
	rows, err := s.queryChanges(ctx, changeColumns+` WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil
}

// PruneChanges drops rows older than retain (0 means 30 days) and reports
// how many went.
func (s *Store) PruneChanges(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		retain = changeRetainDefault
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM changes WHERE ts < ?`, s.now().Add(-retain).UnixNano())
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

const changeColumns = `SELECT id, ts, path, kind, origin, session_id, principal, reliable, from_path FROM changes`

func (s *Store) queryChanges(ctx context.Context, query string, args ...any) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		var ts int64
		var reliable int
		if err := rows.Scan(&c.ID, &ts, &c.Path, &c.Kind, &c.Origin, &c.SessionID, &c.Principal, &reliable, &c.From); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		c.TS, c.Reliable = time.Unix(0, ts), reliable != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// likePrefix escapes p for a LIKE pattern.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p)
}

// recorderFlushEvery bounds how long a change waits in the recorder's
// batch before it is durable; recorderBatch how many ride in one
// transaction.
const (
	recorderFlushEvery = 200 * time.Millisecond
	recorderBatch      = 128
)

// ChangeSource is the slice of the VFS the recorder needs.
type ChangeSource interface {
	WatchChanges() (<-chan vfs.Change, func())
}

// RunChangeRecorder is the changes table's writer: a consumer of the VFS
// change feed that batches what it hears into rows until ctx ends or the
// feed closes. It runs in the owner only (a stdio process beside the
// mount has its own VFS and would record the same kernel changes twice)
// and never blocks the feed: a batch that cannot be written is logged
// and dropped, and the feed's own overflow arrives as a rescan row. It
// returns when the feed closes.
func (s *Store) RunChangeRecorder(ctx context.Context, src ChangeSource) {
	if !s.owner {
		return
	}
	ch, stop := src.WatchChanges()
	defer stop()
	// While no recorder ran — the daemon was down, or died with a batch
	// unflushed — changes went unrecorded, and nothing in the table says
	// so. A record that already has rows therefore opens with a rescan
	// row (origin restart): the same "rows before this may be missing"
	// mark a feed overflow leaves, which pull_events, history and the
	// hooks all know how to show. A first run has nothing to have missed.
	if id, err := s.LastChangeID(context.WithoutCancel(ctx)); err == nil && id > 0 {
		if _, err := s.RecordChanges(context.WithoutCancel(ctx), []Change{{Path: "/", Kind: vfs.KindRescan.String(), Origin: "restart"}}); err != nil {
			slog.Warn("agent: restart marker not recorded", "err", err)
		}
	}
	var batch []Change
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := s.RecordChanges(context.WithoutCancel(ctx), batch); err != nil {
			slog.Warn("agent: changes not recorded", "rows", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	timer := time.NewTimer(recorderFlushEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case c, ok := <-ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ChangesOf(c, s.now())...)
			if len(batch) >= recorderBatch {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(recorderFlushEvery)
		}
	}
}

// RunChangeRetention prunes rows older than retain now and then every
// interval until ctx ends, in the owner only.
func (s *Store) RunChangeRetention(ctx context.Context, retain, every time.Duration) {
	if !s.owner {
		return
	}
	if every <= 0 {
		every = 24 * time.Hour
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := s.PruneChanges(ctx, retain); err != nil {
			if ctx.Err() == nil {
				slog.Warn("agent: change retention failed", "err", err)
			}
		} else if n > 0 {
			slog.Debug("agent: pruned changes", "rows", n, "retain", retain)
		}
		if n, err := s.PruneHookCursors(ctx, retain); err != nil {
			if ctx.Err() == nil {
				slog.Warn("agent: hook cursor retention failed", "err", err)
			}
		} else if n > 0 {
			slog.Debug("agent: pruned hook cursors", "rows", n, "retain", retain)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ChangeIDBefore is the id of the newest row recorded before t, the
// cursor a reader that wants "everything since t" starts from.
func (s *Store) ChangeIDBefore(ctx context.Context, t time.Time) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM changes WHERE ts < ?`, t.UnixNano()).Scan(&id); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	return id, nil
}

// LastChangeSeen is the cursor a session's pull_events last returned, 0
// when it never pulled.
func (s *Store) LastChangeSeen(ctx context.Context, sessionID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT last_change_seen FROM sessions WHERE id = ?`, sessionID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	return id, nil
}

// SetLastChangeSeen stores a session's pull_events cursor; it only moves
// forward.
func (s *Store) SetLastChangeSeen(ctx context.Context, sessionID string, id int64) error {
	if s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_change_seen = ? WHERE id = ? AND last_change_seen < ?`, id, sessionID, id); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// Hook cursors (docs/agent-first-design.md §7): the turn-start hook of an
// agent client is not an MCP session, so its "what changed since my last
// turn" cursor lives in the meta table under the client's own session id.

func hookCursorKey(client, sessionID string) string {
	return "hook_cursor:" + client + ":" + sessionID
}

// HookCursor is the change id a hook session last saw; known is false
// on a session's first turn, which is different from a cursor of 0 (a
// session that started before any change was recorded).
func (s *Store) HookCursor(ctx context.Context, client, sessionID string) (id int64, known bool, err error) {
	var v string
	err = s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, hookCursorKey(client, sessionID)).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("agent: %w", err)
	}
	idText, _, _ := strings.Cut(v, " ")
	id, _ = strconv.ParseInt(idText, 10, 64)
	return id, true, nil
}

// SetHookCursor stores a hook session's cursor. The value carries the
// time it was set ("<id> <unix nanos>"), so PruneHookCursors can drop
// the cursors of sessions not seen for the change retention period:
// every client conversation is a new session id, and a cursor nobody
// will ask for again would otherwise stay for good.
func (s *Store) SetHookCursor(ctx context.Context, client, sessionID string, id int64) error {
	if s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	v := strconv.FormatInt(id, 10) + " " + strconv.FormatInt(s.now().UnixNano(), 10)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		hookCursorKey(client, sessionID), v); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// PruneHookCursors deletes hook cursors last set before the retention
// window (a cursor without a time is from an older shape and goes too:
// its session would be treated as new, which only costs it one turn's
// catch-up). It returns how many rows went.
func (s *Store) PruneHookCursors(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		retain = changeRetainDefault
	}
	cutoff := s.now().Add(-retain).UnixNano()
	res, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE k LIKE 'hook_cursor:%' AND (instr(v, ' ') = 0 OR CAST(substr(v, instr(v, ' ') + 1) AS INTEGER) < ?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	n, _ := res.RowsAffected()
	// The own-write sets of those sessions go the same way ("<json>\n<nanos>").
	if res, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE k LIKE 'hook_writes:%' AND (instr(v, char(10)) = 0 OR CAST(substr(v, instr(v, char(10)) + 1) AS INTEGER) < ?)`, cutoff); err == nil {
		m, _ := res.RowsAffected()
		n += m
	}
	return n, nil
}

// Hook own-writes (docs/agent-first-design.md §7.4): the client's write
// tools go through the kernel, so their changes are kernel rows like any
// other program's. The post-write hook reports the paths the client
// itself wrote; the turn-start hook then leaves those out of "changed
// since your last turn" and reports the other kernel writes — the
// terminal's, another program's — which is what a person means by
// "someone else changed this". The set lives in the meta table under the
// hook session, as a JSON list with the time it was last touched, and
// goes with the hook cursors.

func hookWritesKey(client, sessionID string) string {
	return "hook_writes:" + client + ":" + sessionID
}

// maxHookWrites bounds the set; past it the oldest paths go, and a
// forgotten own write is reported as someone else's, which is the safe
// side.
const maxHookWrites = 500

// AddHookWrites records paths the client's own tools wrote in this hook
// session.
func (s *Store) AddHookWrites(ctx context.Context, client, sessionID string, paths []string) error {
	if s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	if len(paths) == 0 {
		return nil
	}
	have, err := s.HookWrites(ctx, client, sessionID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range have {
		seen[p] = true
	}
	for _, p := range paths {
		if p = Normalise(p); !seen[p] {
			seen[p] = true
			have = append(have, p)
		}
	}
	if len(have) > maxHookWrites {
		have = have[len(have)-maxHookWrites:]
	}
	return s.setHookWrites(ctx, client, sessionID, have)
}

// HookWrites lists the paths the client's own tools wrote in this hook
// session since the set was last cleared.
func (s *Store) HookWrites(ctx context.Context, client, sessionID string) ([]string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, hookWritesKey(client, sessionID)).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent: %w", err)
	}
	body, _, _ := strings.Cut(v, "\n")
	var out []string
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, nil
	}
	return out, nil
}

// ClearHookWrites empties the set once a turn has taken it into account.
func (s *Store) ClearHookWrites(ctx context.Context, client, sessionID string) error {
	if s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE k = ?`, hookWritesKey(client, sessionID)); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

func (s *Store) setHookWrites(ctx context.Context, client, sessionID string, paths []string) error {
	body, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	v := string(body) + "\n" + strconv.FormatInt(s.now().UnixNano(), 10)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		hookWritesKey(client, sessionID), v); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}
