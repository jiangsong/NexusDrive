package agent

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound is returned when a session id is unknown.
var ErrSessionNotFound = errors.New("agent: session not found")

// ErrPrincipalNotFound is returned when a principal id is unknown.
var ErrPrincipalNotFound = errors.New("agent: principal not found")

// ErrInvalidCursor is returned when a paging cursor is not one this store
// handed out, so a control route can answer 400 rather than 500.
var ErrInvalidCursor = errors.New("agent: invalid cursor")

// WriteTools names the tools whose success counts as a write in Summary and
// in the per-session write counter. The audit middleware uses the same set.
var WriteTools = map[string]bool{
	"write_file": true, "edit_file": true, "create_directory": true,
	"copy": true, "move": true, "delete": true,
	// A rollback writes too; it names no path on its row, so ArtifactPaths
	// never lists it.
	"rollback_session": true,
}

// lastSeenGranularity bounds how often a busy session rewrites its
// last_seen_at: a burst of tool calls costs one row update, not one each.
const lastSeenGranularity = 30 * time.Second

// SessionOptions tunes how connections map to sessions.
type SessionOptions struct {
	// Idle is the gap after which a token or loopback session rotates: HTTP
	// without a stateful session has no connection to end, so an idle gap is
	// the only sign that one agent run ended and another began. Zero never
	// rotates.
	Idle time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// ConnInfo is what the MCP layer knows about the connection a call arrived
// on. Key identifies the connection across calls; the transport decides how
// long a session on it may live.
type ConnInfo struct {
	Key           string // "stdio:%p" | "legacy:<sdk id>" | "token:<principal id>" | "loopback:<principal id>" | "bridge:<conn id>"
	Transport     string // stdio | http-legacy | http-token | http-loopback | http-bridge
	PrincipalID   string
	ClientName    string
	ClientVersion string
	// Narrow, when set, is composed with the principal's scope when the
	// connection's session is started: a bridged stdio server runs on the
	// owner under its own scope, never the owner's wider one.
	Narrow *Scope
}

// ListQuery filters and pages a session listing.
type ListQuery struct {
	Cursor  string
	Limit   int    // default 50, max 200
	State   string // "" | active | finished | expired | rolled_back
	Sandbox bool
	// Path keeps the sessions that touched this path: those whose
	// workspace contains it and those that recorded an op on it (as the
	// source, or the destination of a rename).
	Path string
	// Since keeps the sessions last seen at or after this instant.
	Since time.Time
	// opsOnly narrows Path to the sessions that recorded an op on it;
	// SessionsTouching sets it.
	opsOnly bool
	// PrincipalID keeps only the sessions of one principal; the MCP
	// list_sessions tool sets it so an agent sees its own sessions only.
	PrincipalID string
}

// BeginOptions is what begin_session asks for.
type BeginOptions struct {
	// Name is a short label for the task; it is kept as the session's
	// summary until finish_session replaces it with a real one.
	Name string
	// Sandbox narrows the session's writes to its own directory.
	Sandbox bool
	// Workspace is the root the session directory is created under; Begin
	// appends SessionDirName.
	Workspace string
}

// Sessions maps connections to sessions and principals to their scope.
type Sessions struct {
	store *Store
	opt   SessionOptions
	// mu serialises Resolve so that two calls racing on a new connection
	// cannot both miss the lookup and each insert a session.
	mu sync.Mutex
}

// NewSessions wraps a store.
func NewSessions(st *Store, opt SessionOptions) *Sessions {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Sessions{store: st, opt: opt}
}

// Store returns the underlying agent.db.
func (m *Sessions) Store() *Store { return m.store }

// EnsurePrincipal returns the live principal with this kind and name,
// creating it when missing and updating its scope when present, so a server
// restart with a different --allow takes effect on the next session.
func (m *Sessions) EnsurePrincipal(ctx context.Context, kind, name string, sc Scope) (Principal, error) {
	scope, err := json.Marshal(sc)
	if err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM principals WHERE kind = ? AND name = ? AND revoked_at = 0`, kind, name).Scan(&id)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE principals SET scope = ? WHERE id = ?`, string(scope), id); err != nil {
			return Principal{}, fmt.Errorf("agent: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		id = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO principals(id, kind, name, scope, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, kind, name, string(scope), m.opt.Now().UnixNano()); err != nil {
			return Principal{}, fmt.Errorf("agent: %w", err)
		}
	default:
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	p, err := scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE id = ?`, id))
	if err != nil {
		return Principal{}, err
	}
	if err := tx.Commit(); err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	return p, nil
}

// Principal returns one principal by id.
func (m *Sessions) Principal(ctx context.Context, id string) (Principal, error) {
	return scanPrincipal(m.store.db.QueryRowContext(ctx, principalColumns+` WHERE id = ?`, id))
}

const principalColumns = `SELECT id, kind, name, scope, token_prefix, created_at, expires_at, revoked_at, last_used_at FROM principals`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPrincipal(row rowScanner) (Principal, error) {
	var p Principal
	var scope string
	var created, expires, revoked, lastUsed int64
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &scope, &p.TokenPrefix, &created, &expires, &revoked, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrPrincipalNotFound
	}
	if err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	if err := json.Unmarshal([]byte(scope), &p.Scope); err != nil {
		return Principal{}, fmt.Errorf("agent: principal %s scope: %w", p.ID, err)
	}
	p.CreatedAt, p.ExpiresAt, p.RevokedAt, p.LastUsedAt = fromNanos(created), fromNanos(expires), fromNanos(revoked), fromNanos(lastUsed)
	return p, nil
}

// Resolve returns the active session for a connection, starting one when
// the connection has none. A token or loopback session that has been idle
// longer than Idle is expired and replaced, because nothing else marks the
// end of an agent run on a connectionless transport.
func (m *Sessions) Resolve(ctx context.Context, c ConnInfo) (Session, error) {
	if c.Key == "" || c.PrincipalID == "" {
		return Session{}, errors.New("agent: a connection key and principal are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opt.Now()
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	s, err := scanSession(tx.QueryRowContext(ctx, sessionColumns+` WHERE conn_key = ? AND state = 'active'`, c.Key))
	var expired *Session
	switch {
	case err == nil:
		if m.rotates(s.Transport) && now.Sub(s.LastSeenAt) > m.opt.Idle {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'expired', finished_at = ? WHERE id = ?`, now.UnixNano(), s.ID); err != nil {
				return Session{}, fmt.Errorf("agent: %w", err)
			}
			s.State, s.FinishedAt = "expired", now
			old := s
			expired = &old
			break
		}
		if now.Sub(s.LastSeenAt) >= lastSeenGranularity || s.ClientName == "" && c.ClientName != "" {
			if s.ClientName == "" {
				s.ClientName, s.ClientVersion = c.ClientName, c.ClientVersion
			}
			s.LastSeenAt = now
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, client_name = ?, client_version = ? WHERE id = ?`,
				now.UnixNano(), s.ClientName, s.ClientVersion, s.ID); err != nil {
				return Session{}, fmt.Errorf("agent: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("agent: %w", err)
		}
		return s, nil
	case errors.Is(err, ErrSessionNotFound):
	default:
		return Session{}, err
	}
	p, err := scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE id = ?`, c.PrincipalID))
	if err != nil {
		return Session{}, err
	}
	sc := p.Scope
	if c.Narrow != nil {
		sc = sc.Narrow(*c.Narrow)
	}
	scope, err := json.Marshal(sc)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	s = Session{
		ID: uuid.NewString(), PrincipalID: p.ID, ConnKey: c.Key,
		ClientName: c.ClientName, ClientVersion: c.ClientVersion, Transport: c.Transport,
		Scope: sc, State: "active", StartedAt: now, LastSeenAt: now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, principal_id, conn_key, client_name, client_version, transport, scope, state, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		s.ID, s.PrincipalID, s.ConnKey, s.ClientName, s.ClientVersion, s.Transport, string(scope), now.UnixNano(), now.UnixNano()); err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	if expired != nil {
		m.store.publish(Event{Kind: "session", Session: expired})
	}
	m.store.publish(Event{Kind: "session", Session: &s})
	return s, nil
}

// rotates reports whether sessions on this transport end by idling out. A
// stdio session ends with its process and a legacy HTTP session with the
// SDK session, so neither rotates. A bridged session stands for a stdio
// process on another side of HTTP, whose end the owner never sees, so it
// rotates like a token's.
func (m *Sessions) rotates(transport string) bool {
	return m.opt.Idle > 0 && (transport == "http-token" || transport == "http-loopback" || transport == "http-bridge")
}

// Get returns one session by id.
func (m *Sessions) Get(ctx context.Context, id string) (Session, error) {
	return scanSession(m.store.db.QueryRowContext(ctx, sessionColumns+` WHERE id = ?`, id))
}

// List returns sessions newest first, with an opaque cursor for the next
// page when more remain. It has the same shape as the upload listing so the
// console pages both the same way.
func (m *Sessions) List(ctx context.Context, q ListQuery) ([]Session, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	where := []string{"1 = 1"}
	args := []any{}
	if q.Cursor != "" {
		startedAt, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(started_at < ? OR (started_at = ? AND id > ?))")
		args = append(args, startedAt, startedAt, id)
	}
	if q.State != "" {
		where = append(where, "state = ?")
		args = append(args, q.State)
	}
	if q.Sandbox {
		where = append(where, "sandbox = 1")
	}
	if q.Path != "" {
		p := Normalise(q.Path)
		touched := "EXISTS (SELECT 1 FROM session_ops o WHERE o.session_id = sessions.id AND (o.path = ? OR o.to_path = ?))"
		if q.opsOnly {
			where = append(where, touched)
			args = append(args, p, p)
		} else {
			where = append(where, "((workspace != '' AND (workspace = ? OR substr(?, 1, length(workspace) + 1) = workspace || '/')) OR "+touched+")")
			args = append(args, p, p, p, p)
		}
	}
	if !q.Since.IsZero() {
		where = append(where, "last_seen_at >= ?")
		args = append(args, q.Since.UnixNano())
	}
	if q.PrincipalID != "" {
		where = append(where, "principal_id = ?")
		args = append(args, q.PrincipalID)
	}
	args = append(args, limit+1)
	rows, err := m.store.db.QueryContext(ctx, sessionColumns+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY started_at DESC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.StartedAt.UnixNano(), last.ID)
	}
	return out, next, nil
}

func encodeCursor(startedAt int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(startedAt, 10) + ":" + id))
}

func decodeCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", ErrInvalidCursor
	}
	ns, id, ok := strings.Cut(string(raw), ":")
	startedAt, perr := strconv.ParseInt(ns, 10, 64)
	if !ok || perr != nil || id == "" {
		return 0, "", ErrInvalidCursor
	}
	return startedAt, id, nil
}

// Begin opens an explicit session on the connection of current, with its
// own delivery directory under opt.Workspace. The connection's active
// session (current itself, normally) is finished first, so a connection
// still has exactly one active session and the next call resolves to the
// new one. The new session inherits the principal, connection and client
// of current and its scope; with Sandbox the scope's writes are narrowed to
// the new directory, and without it a sandbox inherited from an earlier
// explicit session is lifted, since the principal's scope never had one.
// The directory itself is created by the caller: this store never touches
// the mount.
func (m *Sessions) Begin(ctx context.Context, current Session, opt BeginOptions) (Session, error) {
	if current.ConnKey == "" || current.PrincipalID == "" {
		return Session{}, errors.New("agent: the current session has no connection")
	}
	if opt.Workspace == "" {
		return Session{}, ErrWorkspaceUnset
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opt.Now()
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	client := current.ClientName
	if current.Transport == "http-token" {
		// Over HTTP the token's own name says who this is; the client name
		// is whatever SDK the token holder happened to use.
		if p, err := scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE id = ?`, current.PrincipalID)); err == nil && p.Kind == "token" {
			client = p.Name
		}
	}
	s := Session{
		ID: uuid.NewString(), PrincipalID: current.PrincipalID, ConnKey: current.ConnKey,
		ClientName: current.ClientName, ClientVersion: current.ClientVersion, Transport: current.Transport,
		Scope: current.Scope, Sandbox: opt.Sandbox, State: "active", StartedAt: now, LastSeenAt: now,
		Summary: opt.Name,
	}
	s.Workspace = path.Join(Normalise(opt.Workspace), SessionDirName(client, now, s.ID))
	s.Scope.Sandbox = ""
	if opt.Sandbox {
		s.Scope.Sandbox = s.Workspace
	}
	scope, err := json.Marshal(s.Scope)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	var finished []Session
	rows, err := tx.QueryContext(ctx, sessionColumns+` WHERE conn_key = ? AND state = 'active'`, current.ConnKey)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	for rows.Next() {
		old, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return Session{}, err
		}
		old.State, old.FinishedAt = "finished", now
		finished = append(finished, old)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'finished', finished_at = ? WHERE conn_key = ? AND state = 'active'`,
		now.UnixNano(), current.ConnKey); err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, principal_id, conn_key, client_name, client_version, transport, scope, workspace, sandbox, state, started_at, last_seen_at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		s.ID, s.PrincipalID, s.ConnKey, s.ClientName, s.ClientVersion, s.Transport, string(scope), s.Workspace, boolInt(s.Sandbox),
		now.UnixNano(), now.UnixNano(), s.Summary); err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	for i := range finished {
		m.store.publish(Event{Kind: "session", Session: &finished[i]})
	}
	m.store.publish(Event{Kind: "session", Session: &s})
	return s, nil
}

// insertSession writes a session row as given. Begin and Resolve build
// their rows inside a transaction of their own; a rollback session has no
// connection to reconcile with and goes in directly.
func (m *Sessions) insertSession(ctx context.Context, s Session) error {
	scope, err := json.Marshal(s.Scope)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if _, err := m.store.db.ExecContext(ctx, `INSERT INTO sessions(id, principal_id, conn_key, client_name, client_version, transport, scope, workspace, sandbox, state, started_at, last_seen_at, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.PrincipalID, s.ConnKey, s.ClientName, s.ClientVersion, s.Transport, string(scope), s.Workspace, boolInt(s.Sandbox),
		s.State, s.StartedAt.UnixNano(), s.LastSeenAt.UnixNano(), s.Summary); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// markRolledBack makes rolled_back the session's terminal state and
// remembers when. The sessions table has no column for that instant and
// the phase-two schema is frozen, so it lives in the meta table under
// "rolled_back_at:<id>", which scanSession reads back. A session that
// was still active is finished by the same update.
func (m *Sessions) markRolledBack(ctx context.Context, id string) error {
	now := m.opt.Now().UnixNano()
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'rolled_back', finished_at = CASE WHEN finished_at = 0 THEN ? ELSE finished_at END WHERE id = ?`, now, id); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, rolledBackKey+id, strconv.FormatInt(now, 10)); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if s, err := m.Get(ctx, id); err == nil {
		m.store.publish(Event{Kind: "session", Session: &s})
	}
	return nil
}

// rolledBackKey prefixes the meta rows that hold rollback instants.
const rolledBackKey = "rolled_back_at:"

// setRollbackResult records what a rollback did with one row; restored
// rows are marked so a rerun skips them.
func (s *Store) setRollbackResult(ctx context.Context, seq int64, restored bool, result string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE session_ops SET rolled_back = ?, rollback_result = ? WHERE seq = ?`, boolInt(restored), result, seq); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Finish closes a session with a summary. Finishing is terminal: the next
// call on the same connection starts a new session. A session that is no
// longer active is returned as it is.
func (m *Sessions) Finish(ctx context.Context, id, summary string) (Session, error) {
	now := m.opt.Now()
	res, err := m.store.db.ExecContext(ctx, `UPDATE sessions SET state = 'finished', finished_at = ?, summary = ? WHERE id = ? AND state = 'active'`,
		now.UnixNano(), summary, id)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	return m.finished(ctx, id, res)
}

// FinishConn finishes the active session of a connection, if it has one,
// and reports whether it did. A stdio server calls it for its own
// connection when its transport closes: the process is the session, and a
// session that outlives its process would sit in the console as active
// until the idle sweep — which a stdio session, having no idle rotation,
// never reaches.
func (m *Sessions) FinishConn(ctx context.Context, key, summary string) (Session, bool, error) {
	s, err := scanSession(m.store.db.QueryRowContext(ctx, sessionColumns+` WHERE conn_key = ? AND state = 'active'`, key))
	if errors.Is(err, ErrSessionNotFound) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	s, err = m.Finish(ctx, s.ID, summary)
	return s, err == nil && s.State == "finished", err
}

// FinishWith is Finish for an explicit session: it also records the
// artifacts the session delivered, and an empty summary keeps the label
// the session was begun with rather than blanking it.
func (m *Sessions) FinishWith(ctx context.Context, id, summary string, arts []Artifact) (Session, error) {
	if arts == nil {
		arts = []Artifact{}
	}
	artifacts, err := json.Marshal(arts)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	now := m.opt.Now()
	res, err := m.store.db.ExecContext(ctx, `UPDATE sessions SET state = 'finished', finished_at = ?, artifacts = ?,
		summary = CASE WHEN ? = '' THEN summary ELSE ? END WHERE id = ? AND state = 'active'`,
		now.UnixNano(), string(artifacts), summary, summary, id)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	return m.finished(ctx, id, res)
}

// finished reads a session back after a finishing update and publishes it
// when the update took effect.
func (m *Sessions) finished(ctx context.Context, id string, res sql.Result) (Session, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		m.store.publish(Event{Kind: "session", Session: &s})
	}
	return s, nil
}

// WriteCounts reports, for each given session id, how many successful
// write-tool calls its audit rows record. A session with no writes is absent
// from the map. The listing fills Session.Writes from it in one query rather
// than one per row.
func (m *Sessions) WriteCounts(ctx context.Context, ids []string) (map[string]int, error) {
	out := map[string]int{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{}
	tools := make([]string, 0, len(WriteTools))
	for tool := range WriteTools {
		tools = append(tools, "?")
		args = append(args, tool)
	}
	marks := make([]string, 0, len(ids))
	for _, id := range ids {
		marks = append(marks, "?")
		args = append(args, id)
	}
	rows, err := m.store.db.QueryContext(ctx, `SELECT session_id, count(*) FROM audit WHERE result = 'ok' AND tool IN (`+
		strings.Join(tools, ",")+`) AND session_id IN (`+strings.Join(marks, ",")+`) GROUP BY session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// Summary counts active sessions and, since the given instant, successful
// writes and denied calls.
func (m *Sessions) Summary(ctx context.Context, since time.Time) (Summary, error) {
	var out Summary
	if err := m.store.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE state = 'active'`).Scan(&out.Active); err != nil {
		return Summary{}, fmt.Errorf("agent: %w", err)
	}
	tools := make([]string, 0, len(WriteTools))
	args := []any{since.UnixNano()}
	for tool := range WriteTools {
		tools = append(tools, "?")
		args = append(args, tool)
	}
	if err := m.store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE ts >= ? AND result = 'ok' AND tool IN (`+strings.Join(tools, ",")+`)`, args...).Scan(&out.WritesToday); err != nil {
		return Summary{}, fmt.Errorf("agent: %w", err)
	}
	if err := m.store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE ts >= ? AND result = 'denied'`, since.UnixNano()).Scan(&out.DeniedToday); err != nil {
		return Summary{}, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// sessionColumns is the row every session query selects: the columns of
// the table, the number of ops the session recorded, and the rollback
// instant kept in meta (see markRolledBack). The two subqueries cost a
// lookup per row on indexed keys, which a page of at most 200 rows bears.
const sessionColumns = `SELECT id, principal_id, conn_key, client_name, client_version, transport, scope, workspace, sandbox, state, started_at, last_seen_at, finished_at, summary, artifacts,
	(SELECT count(*) FROM session_ops WHERE session_ops.session_id = sessions.id),
	COALESCE((SELECT v FROM meta WHERE meta.k = 'rolled_back_at:' || sessions.id), '0') FROM sessions`

func scanSession(row rowScanner) (Session, error) {
	var s Session
	var scope, artifacts, rolledBack string
	var sandbox int
	var started, lastSeen, finished int64
	err := row.Scan(&s.ID, &s.PrincipalID, &s.ConnKey, &s.ClientName, &s.ClientVersion, &s.Transport, &scope,
		&s.Workspace, &sandbox, &s.State, &started, &lastSeen, &finished, &s.Summary, &artifacts, &s.OpsCount, &rolledBack)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	if err := json.Unmarshal([]byte(scope), &s.Scope); err != nil {
		return Session{}, fmt.Errorf("agent: session %s scope: %w", s.ID, err)
	}
	if err := json.Unmarshal([]byte(artifacts), &s.Artifacts); err != nil {
		return Session{}, fmt.Errorf("agent: session %s artifacts: %w", s.ID, err)
	}
	s.Sandbox = sandbox != 0
	s.StartedAt, s.LastSeenAt, s.FinishedAt = fromNanos(started), fromNanos(lastSeen), fromNanos(finished)
	if ns, err := strconv.ParseInt(rolledBack, 10, 64); err == nil {
		s.RolledBackAt = fromNanos(ns)
	}
	return s, nil
}

// fromNanos turns a stored Unix-nanosecond column into a time, keeping the
// "not yet" zero as a zero time.
func fromNanos(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

type sessionContextKey struct{}

// WithSession attaches the resolved session to a request context.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, s)
}

// FromContext returns the session a request runs under, if any.
func FromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(Session)
	return s, ok
}
