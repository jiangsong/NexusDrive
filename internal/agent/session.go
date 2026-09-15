package agent

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	Key           string // "stdio:%p" | "legacy:<sdk id>" | "token:<principal id>" | "loopback:<principal id>"
	Transport     string // stdio | http-legacy | http-token | http-loopback
	PrincipalID   string
	ClientName    string
	ClientVersion string
}

// ListQuery filters and pages a session listing.
type ListQuery struct {
	Cursor  string
	Limit   int    // default 50, max 200
	State   string // "" | active | finished | expired
	Sandbox bool
	Path    string // sessions whose workspace contains this path
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
	scope, err := json.Marshal(p.Scope)
	if err != nil {
		return Session{}, fmt.Errorf("agent: %w", err)
	}
	s = Session{
		ID: uuid.NewString(), PrincipalID: p.ID, ConnKey: c.Key,
		ClientName: c.ClientName, ClientVersion: c.ClientVersion, Transport: c.Transport,
		Scope: p.Scope, State: "active", StartedAt: now, LastSeenAt: now,
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
// SDK session, so neither rotates.
func (m *Sessions) rotates(transport string) bool {
	return m.opt.Idle > 0 && (transport == "http-token" || transport == "http-loopback")
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
		where = append(where, "workspace != '' AND (workspace = ? OR substr(?, 1, length(workspace) + 1) = workspace || '/')")
		args = append(args, p, p)
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

const sessionColumns = `SELECT id, principal_id, conn_key, client_name, client_version, transport, scope, workspace, sandbox, state, started_at, last_seen_at, finished_at, summary, artifacts FROM sessions`

func scanSession(row rowScanner) (Session, error) {
	var s Session
	var scope, artifacts string
	var sandbox int
	var started, lastSeen, finished int64
	err := row.Scan(&s.ID, &s.PrincipalID, &s.ConnKey, &s.ClientName, &s.ClientVersion, &s.Transport, &scope,
		&s.Workspace, &sandbox, &s.State, &started, &lastSeen, &finished, &s.Summary, &artifacts)
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
