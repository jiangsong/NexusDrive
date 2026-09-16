package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Op is one row of session_ops: a write an MCP tool made inside a session,
// with the state the path had before it (the preimage) and, once the tool
// returned, the version it left behind. Rollback replays these rows in
// reverse (docs/agent-roadmap.md §4.8). The JSON view is what
// GET /sessions/{id} lists as ops[].
type Op struct {
	Seq       int64  `json:"seq"`
	SessionID string `json:"session_id"`
	// TS is when the row was recorded, which is when the write began.
	TS time.Time `json:"ts"`
	// AuditID links the row to the audit row of the tool call that made
	// it; 0 until the audit middleware, which only knows the id after the
	// call, links them.
	AuditID int64 `json:"audit_id,omitempty"`
	// Op is create | overwrite | append | edit | mkdir | rename | delete.
	Op     string `json:"op"`
	Path   string `json:"path"`
	ToPath string `json:"to_path,omitempty"` // rename only
	// PreState is absent | file | dir.
	PreState    string `json:"pre_state"`
	PreRemote   string `json:"pre_remote,omitempty"`
	PreRemoteID string `json:"pre_remote_id,omitempty"`
	PreVersion  string `json:"pre_version,omitempty"`
	PreSize     int64  `json:"pre_size,omitempty"`
	PreHash     string `json:"pre_hash,omitempty"`
	// PreBlob names the preimage under the preimage directory (its sha256);
	// "" when no content was kept, in which case PreReason says why.
	PreBlob string `json:"-"`
	// PreReason is "" | too_large | not_cached | dir. A file over the size
	// limit or that could not be read in full has no preimage; "dir" marks
	// a recursive directory delete whose contents were not kept.
	PreReason string `json:"pre_reason,omitempty"`
	// PostVersion is what the write left: for content writes the
	// ContentVersion of the bytes written (a version string would change
	// once the upload queue publishes the file, a hash does not); for a
	// copy the destination's version; "" for mkdir, rename and delete.
	PostVersion    string `json:"post_version,omitempty"`
	RolledBack     bool   `json:"rolled_back"`
	RollbackResult string `json:"rollback_result,omitempty"`
}

// contentPrefix marks a PostVersion that is a hash of the written bytes
// rather than a provider version.
const contentPrefix = "sha256:"

// ContentVersion is the PostVersion of a content write: the sha256 of the
// bytes the file holds after it. Rollback compares it against the current
// bytes, so a file the upload queue re-versioned in between still counts
// as unchanged when nobody else wrote to it.
func ContentVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return contentPrefix + hex.EncodeToString(sum[:])
}

// withPre fills the preimage columns of an op from a capture.
func (o Op) withPre(p Pre) Op {
	o.PreState, o.PreRemote, o.PreRemoteID, o.PreVersion = p.State, p.Remote, p.RemoteID, p.Version
	o.PreSize, o.PreHash, o.PreBlob, o.PreReason = p.Size, p.Hash, p.Blob, p.Reason
	return o
}

// WithPre is withPre for callers outside the package.
func (o Op) WithPre(p Pre) Op { return o.withPre(p) }

// RecordOp inserts one row and returns its seq. The caller has already
// captured the preimage: the blob exists before the row that names it, so
// a crash in between leaves an orphan blob (which Recover removes) and
// never a row whose preimage is missing.
func (s *Store) RecordOp(ctx context.Context, sessionID string, op Op) (int64, error) {
	if s.readOnly {
		return 0, errors.New("agent: the store is read-only")
	}
	if sessionID == "" || op.Op == "" || op.Path == "" {
		return 0, errors.New("agent: an op needs a session, an op name and a path")
	}
	switch op.PreState {
	case "absent", "file", "dir":
	default:
		return 0, fmt.Errorf("agent: unknown pre_state %q", op.PreState)
	}
	ts := op.TS
	if ts.IsZero() {
		ts = s.now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO session_ops(session_id, ts, audit_id, op, path, to_path, pre_state, pre_remote, pre_remote_id, pre_version, pre_size, pre_hash, pre_blob, pre_reason, post_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, ts.UnixNano(), op.AuditID, op.Op, Normalise(op.Path), normaliseOrEmpty(op.ToPath), op.PreState, op.PreRemote, op.PreRemoteID, op.PreVersion,
		op.PreSize, op.PreHash, op.PreBlob, op.PreReason, op.PostVersion)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	noteOp(ctx, seq)
	return seq, nil
}

func normaliseOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	return Normalise(p)
}

// CompleteOp records what the write left behind once the tool returned.
func (s *Store) CompleteOp(ctx context.Context, seq int64, postVersion string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_ops SET post_version = ? WHERE seq = ?`, postVersion, seq)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// AbandonOp marks a row whose write failed after it was recorded. Nothing
// is known to have changed, so rollback skips it with the given reason
// rather than guessing at what a half-failed write left.
func (s *Store) AbandonOp(ctx context.Context, seq int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_ops SET rolled_back = 1, rollback_result = ? WHERE seq = ?`, reason, seq)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// LinkOpsToAudit sets audit_id on the given rows. The audit middleware
// calls it after the audit row of the call exists.
func (s *Store) LinkOpsToAudit(ctx context.Context, seqs []int64, auditID int64) error {
	if len(seqs) == 0 || auditID == 0 {
		return nil
	}
	marks := make([]string, 0, len(seqs))
	args := []any{auditID}
	for _, seq := range seqs {
		marks = append(marks, "?")
		args = append(args, seq)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE session_ops SET audit_id = ? WHERE seq IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

const opColumns = `SELECT seq, session_id, ts, audit_id, op, path, to_path, pre_state, pre_remote, pre_remote_id, pre_version, pre_size, pre_hash, pre_blob, pre_reason, post_version, rolled_back, rollback_result FROM session_ops`

// OpsOf returns the rows of one session in the order they happened.
func (s *Store) OpsOf(ctx context.Context, sessionID string) ([]Op, error) {
	rows, err := s.db.QueryContext(ctx, opColumns+` WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []Op{}
	for rows.Next() {
		var o Op
		var rolledBack int
		var ts int64
		if err := rows.Scan(&o.Seq, &o.SessionID, &ts, &o.AuditID, &o.Op, &o.Path, &o.ToPath, &o.PreState, &o.PreRemote, &o.PreRemoteID,
			&o.PreVersion, &o.PreSize, &o.PreHash, &o.PreBlob, &o.PreReason, &o.PostVersion, &rolledBack, &o.RollbackResult); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		o.RolledBack = rolledBack != 0
		if ts != 0 {
			o.TS = time.Unix(0, ts)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// SessionsTouching lists, newest first, the sessions that recorded an op
// on path (as its source or, for a rename, its destination) and were last
// seen at or after since. The inspector's "changed by an agent" line and
// GET /sessions?path= read it; the session_ops_path index carries it.
func (m *Sessions) SessionsTouching(ctx context.Context, p string, since time.Time) ([]Session, error) {
	sessions, _, err := m.List(ctx, ListQuery{Path: p, Since: since, Limit: maxSessionsPage, opsOnly: true})
	return sessions, err
}

// maxSessionsPage is the largest page List hands out.
const maxSessionsPage = 200

type opNoteKey struct{}

// OpNote collects the seqs of the ops a call recorded, so the audit
// middleware can link them to the call's audit row once that exists.
type OpNote struct {
	mu   sync.Mutex
	seqs []int64
}

// WithOpNote attaches a fresh note to ctx and returns it.
func WithOpNote(ctx context.Context) (context.Context, *OpNote) {
	n := &OpNote{}
	return context.WithValue(ctx, opNoteKey{}, n), n
}

// Seqs returns the seqs recorded so far.
func (n *OpNote) Seqs() []int64 {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]int64(nil), n.seqs...)
}

func noteOp(ctx context.Context, seq int64) {
	if n, ok := ctx.Value(opNoteKey{}).(*OpNote); ok && n != nil {
		n.mu.Lock()
		n.seqs = append(n.seqs, seq)
		n.mu.Unlock()
	}
}
