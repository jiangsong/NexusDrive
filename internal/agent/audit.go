package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// AuditQuery filters and pages the audit trail. Rows come newest first;
// Cursor is the id of the last row of the previous page.
type AuditQuery struct {
	Cursor  string
	Limit   int // default 200, max 1000
	Session string
	Tool    string
	Result  string // ok | denied | error
	Since   time.Time
}

// AppendAudit records one tool call and publishes it to watchers. A zero
// TS becomes the store clock. A row that cannot be written is counted in
// AuditWriteFailures and the error returned; the caller decides whether
// that fails the call it describes (the MCP middleware does not: an agent
// must not lose a tool because the audit disk is full).
func (s *Store) AppendAudit(ctx context.Context, row AuditRow) (int64, error) {
	id, err := s.appendAudit(ctx, &row)
	if err != nil {
		s.auditFailures.Add(1)
		return 0, err
	}
	s.publish(Event{Kind: "audit", Audit: &row})
	return id, nil
}

func (s *Store) appendAudit(ctx context.Context, row *AuditRow) (int64, error) {
	if s.readOnly {
		return 0, errors.New("agent: the store is read-only")
	}
	if s.appendFault != nil {
		if err := s.appendFault(); err != nil {
			return 0, fmt.Errorf("agent: %w", err)
		}
	}
	if row.TS.IsZero() {
		row.TS = s.now()
	}
	if row.Paths == nil {
		row.Paths = []string{}
	}
	if len(row.Args) == 0 {
		row.Args = json.RawMessage(`{}`)
	}
	paths, err := json.Marshal(row.Paths)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO audit(ts, principal_id, session_id, transport, tool, paths, args, bytes_in, bytes_out, result, error, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TS.UnixNano(), row.PrincipalID, row.SessionID, row.Transport, row.Tool, string(paths), string(row.Args),
		row.BytesIn, row.BytesOut, row.Result, row.Error, row.DurationMS)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	row.ID = id
	return id, nil
}

// AuditWriteFailures reports how many rows could not be appended since the
// store was opened. Status and metrics surface it, because an audit trail
// that silently stopped is worse than none.
func (s *Store) AuditWriteFailures() int64 { return s.auditFailures.Load() }

const auditColumns = `SELECT id, ts, principal_id, session_id, transport, tool, paths, args, bytes_in, bytes_out, result, error, duration_ms FROM audit`

// Audit returns rows newest first and, when more remain, the cursor for the
// next page.
func (s *Store) Audit(ctx context.Context, q AuditQuery) ([]AuditRow, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"1 = 1"}
	args := []any{}
	if q.Cursor != "" {
		last, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || last <= 0 {
			return nil, "", errors.New("agent: invalid audit cursor")
		}
		where = append(where, "id < ?")
		args = append(args, last)
	}
	if q.Session != "" {
		where = append(where, "session_id = ?")
		args = append(args, q.Session)
	}
	if q.Tool != "" {
		where = append(where, "tool = ?")
		args = append(args, q.Tool)
	}
	if q.Result != "" {
		where = append(where, "result = ?")
		args = append(args, q.Result)
	}
	if !q.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, q.Since.UnixNano())
	}
	args = append(args, limit+1)
	rows, err := s.queryAudit(ctx, auditColumns+` WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	return rows, next, nil
}

// AuditForSession returns the newest rows of one session, at most limit
// (default 50), for the session detail view.
func (s *Store) AuditForSession(ctx context.Context, sessionID string, limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryAudit(ctx, auditColumns+` WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit)
}

func (s *Store) queryAudit(ctx context.Context, query string, args ...any) ([]AuditRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		var ts int64
		var paths, args string
		if err := rows.Scan(&r.ID, &ts, &r.PrincipalID, &r.SessionID, &r.Transport, &r.Tool, &paths, &args,
			&r.BytesIn, &r.BytesOut, &r.Result, &r.Error, &r.DurationMS); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		r.TS = time.Unix(0, ts)
		if err := json.Unmarshal([]byte(paths), &r.Paths); err != nil {
			return nil, fmt.Errorf("agent: audit row %d paths: %w", r.ID, err)
		}
		if r.Paths == nil {
			r.Paths = []string{}
		}
		if json.Valid([]byte(args)) {
			r.Args = json.RawMessage(args)
		} else {
			r.Args = json.RawMessage(`{}`)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// PurgeAudit deletes rows recorded before the instant and reports how many.
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	if s.readOnly {
		return 0, errors.New("agent: the store is read-only")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE ts < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	return n, nil
}

// RunAuditRetention purges rows older than retain now and then every
// interval, until ctx ends. Only the owner of agent.db runs it: a non-owner
// (a stdio MCP process beside the daemon) returns at once, so two processes
// never race over the same rows. A retain of zero or less keeps everything.
func (s *Store) RunAuditRetention(ctx context.Context, retain, every time.Duration) {
	if !s.owner || retain <= 0 {
		return
	}
	if every <= 0 {
		every = time.Hour
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := s.PurgeAudit(ctx, s.now().Add(-retain)); err != nil {
			if ctx.Err() == nil {
				slog.Warn("agent: audit retention failed", "err", err)
			}
		} else if n > 0 {
			slog.Debug("agent: audit retention", "purged", n, "retain", retain)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
