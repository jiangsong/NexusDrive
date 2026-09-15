package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"cloudfs/internal/vfs"

	"github.com/google/uuid"
)

// PlanItem is one op of a rollback plan and what became of it.
type PlanItem struct {
	Seq    int64  `json:"seq"`
	Op     string `json:"op"`
	Path   string `json:"path"`
	ToPath string `json:"to_path,omitempty"`
	// Reason says why an item was skipped (already, too_large, not_cached,
	// dir, not_empty, missing, incomplete) or is a conflict (modified,
	// exists, from_exists, missing, or the error a write returned).
	Reason string `json:"reason,omitempty"`
}

// Plan is what a rollback would do, or did: every op of the session in
// the reverse order of its seq, sorted into the three outcomes of
// docs/agent-roadmap.md §4.8. The slices are never nil, so the console's
// grouping sees three lists on an empty plan too.
type Plan struct {
	SessionID string `json:"session_id"`
	DryRun    bool   `json:"dry_run"`
	// RollbackSessionID is the session the rollback's own writes were
	// recorded under, so it can be rolled back in turn; "" on a dry run.
	RollbackSessionID string     `json:"rollback_session_id,omitempty"`
	Restored          []PlanItem `json:"restored"`
	Skipped           []PlanItem `json:"skipped"`
	Conflict          []PlanItem `json:"conflict"`
}

// ErrRollbackUnavailable is returned when no VFS or preimage store is
// wired to the caller, as in a read-only CLI.
var ErrRollbackUnavailable = errors.New("agent: rollback needs the running daemon")

// rollbackTransport is the transport a rollback session records when the
// caller has no MCP session of its own (the control plane or the CLI).
const rollbackTransport = "console"

// Rollback undoes the ops of session id in reverse seq order through fs,
// restoring content from pre, and returns the plan and the session as it
// is afterwards. Each op is checked before it is touched: a file someone
// changed after the session (its content no longer what the session
// wrote) is a conflict and is left alone; an op with no preimage, a
// non-empty directory or a recursive directory delete is skipped with its
// reason; a row an earlier rollback already restored is skipped as
// "already", which is what makes an interrupted rollback safe to run
// again. With dryRun the plan is computed and nothing is written: no
// session is opened and no row changes.
//
// The writes of a real rollback are recorded under a new session named
// "rollback of <id>", with preimages of their own, so a rollback can be
// rolled back. It belongs to the principal of the session in ctx when
// there is one, else to the console principal. When every row has been
// looked at, the original session's state becomes rolled_back.
func (m *Sessions) Rollback(ctx context.Context, fs FSOps, pre *Preimages, id string, dryRun bool) (Plan, Session, error) {
	if fs == nil || pre == nil {
		return Plan{}, Session{}, ErrRollbackUnavailable
	}
	sess, err := m.Get(ctx, id)
	if err != nil {
		return Plan{}, Session{}, err
	}
	ops, err := m.store.OpsOf(ctx, id)
	if err != nil {
		return Plan{}, Session{}, err
	}
	plan := Plan{SessionID: id, DryRun: dryRun, Restored: []PlanItem{}, Skipped: []PlanItem{}, Conflict: []PlanItem{}}
	r := &rollback{m: m, fs: fs, pre: pre, dryRun: dryRun}
	if !dryRun {
		r.session, err = m.beginRollbackSession(ctx, sess)
		if err != nil {
			return Plan{}, Session{}, err
		}
		plan.RollbackSessionID = r.session.ID
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return plan, sess, err
		}
		op := ops[i]
		item := PlanItem{Seq: op.Seq, Op: op.Op, Path: op.Path, ToPath: op.ToPath}
		outcome, reason := r.undo(ctx, op)
		item.Reason = reason
		switch outcome {
		case restored:
			plan.Restored = append(plan.Restored, item)
		case skipped:
			plan.Skipped = append(plan.Skipped, item)
		default:
			plan.Conflict = append(plan.Conflict, item)
		}
		// A row an earlier run already settled (restored, or abandoned
		// because its write failed) keeps that record: writing "skipped:
		// already" over it would clear rolled_back and make the next run
		// try the undo again, against content that is no longer what the
		// session wrote. That is what makes a rerun idempotent beyond the
		// second run.
		if dryRun || op.RolledBack {
			continue
		}
		result := outcome.String()
		if reason != "" {
			result += ": " + reason
		}
		if err := m.store.setRollbackResult(ctx, op.Seq, outcome == restored, result); err != nil {
			return plan, sess, err
		}
	}
	if dryRun {
		return plan, sess, nil
	}
	summary := fmt.Sprintf("rollback of %s: %d restored, %d skipped, %d conflicts", id, len(plan.Restored), len(plan.Skipped), len(plan.Conflict))
	if _, err := m.Finish(ctx, r.session.ID, summary); err != nil {
		return plan, sess, err
	}
	if err := m.markRolledBack(ctx, id); err != nil {
		return plan, sess, err
	}
	sess, err = m.Get(ctx, id)
	return plan, sess, err
}

type outcome int

const (
	restored outcome = iota
	skipped
	conflict
)

func (o outcome) String() string {
	switch o {
	case restored:
		return "restored"
	case skipped:
		return "skipped"
	}
	return "conflict"
}

// rollback is the state of one Rollback call.
type rollback struct {
	m       *Sessions
	fs      FSOps
	pre     *Preimages
	dryRun  bool
	session Session
}

// undo decides what to do about one op and, unless this is a dry run,
// does it. The checks are the table of docs/agent-roadmap.md §4.8.
func (r *rollback) undo(ctx context.Context, op Op) (outcome, string) {
	if op.RolledBack {
		if op.RollbackResult != "" && op.RollbackResult != "restored" {
			return skipped, op.RollbackResult
		}
		return skipped, "already"
	}
	switch op.Op {
	case "rename":
		return r.undoRename(ctx, op)
	case "mkdir":
		return r.undoMkdir(ctx, op)
	case "delete":
		return r.undoDelete(ctx, op)
	}
	// create, overwrite, append, edit: what to do depends on what was
	// there before, not on the tool's name for the write.
	switch op.PreState {
	case "absent":
		return r.undoCreate(ctx, op)
	case "dir":
		return skipped, "dir"
	}
	return r.undoOverwrite(ctx, op)
}

// unchanged reports whether the file at p still holds what the op wrote:
// its bytes hash to a content PostVersion, or its version matches a plain
// one. A missing PostVersion means the write never completed.
func (r *rollback) unchanged(ctx context.Context, p string, op Op) (bool, string) {
	if op.PostVersion == "" {
		return false, "incomplete"
	}
	info, err := r.fs.StatPath(ctx, p)
	if errors.Is(err, vfs.ErrNotFound) {
		return false, "missing"
	}
	if err != nil {
		return false, err.Error()
	}
	if info.IsDir {
		return false, "modified"
	}
	if len(op.PostVersion) > len(contentPrefix) && op.PostVersion[:len(contentPrefix)] == contentPrefix {
		data, err := r.fs.ReadFileRange(ctx, p, 0, 0)
		if err != nil {
			return false, err.Error()
		}
		sum := sha256.Sum256(data)
		if contentPrefix+hex.EncodeToString(sum[:]) != op.PostVersion {
			return false, "modified"
		}
		return true, ""
	}
	if info.Version != op.PostVersion {
		return false, "modified"
	}
	return true, ""
}

// undoOverwrite writes the preimage back over a file the session
// overwrote, edited or appended to.
func (r *rollback) undoOverwrite(ctx context.Context, op Op) (outcome, string) {
	if op.PreBlob == "" {
		if op.PreReason != "" {
			return skipped, op.PreReason
		}
		return skipped, "not_cached"
	}
	if ok, reason := r.unchanged(ctx, op.Path, op); !ok {
		return conflict, reason
	}
	return r.restoreContent(ctx, op, "overwrite")
}

// undoCreate removes a file the session created, if it is still what the
// session wrote.
func (r *rollback) undoCreate(ctx context.Context, op Op) (outcome, string) {
	if _, err := r.fs.StatPath(ctx, op.Path); errors.Is(err, vfs.ErrNotFound) {
		return skipped, "missing"
	}
	if ok, reason := r.unchanged(ctx, op.Path, op); !ok {
		return conflict, reason
	}
	if r.dryRun {
		return restored, ""
	}
	if err := r.recordAndDo(ctx, Op{Op: "delete", Path: op.Path}, func() (string, error) {
		return "", r.fs.Remove(ctx, op.Path, false)
	}); err != nil {
		return conflict, err.Error()
	}
	return restored, ""
}

// undoMkdir removes a directory the session made, if it is empty.
func (r *rollback) undoMkdir(ctx context.Context, op Op) (outcome, string) {
	info, err := r.fs.StatPath(ctx, op.Path)
	if errors.Is(err, vfs.ErrNotFound) {
		return skipped, "missing"
	}
	if err != nil {
		return conflict, err.Error()
	}
	if !info.IsDir {
		return conflict, "modified"
	}
	if r.dryRun {
		// A dry run cannot tell an empty directory from a full one without
		// a Remove; it says what a real run would attempt.
		return restored, ""
	}
	err = r.recordAndDo(ctx, Op{Op: "delete", Path: op.Path}, func() (string, error) {
		return "", r.fs.Remove(ctx, op.Path, false)
	})
	if errors.Is(err, vfs.ErrNotEmpty) {
		return skipped, "not_empty"
	}
	if err != nil {
		return conflict, err.Error()
	}
	return restored, ""
}

// undoRename moves the path back, unless something now sits where it
// came from or the moved path is gone.
func (r *rollback) undoRename(ctx context.Context, op Op) (outcome, string) {
	if _, err := r.fs.StatPath(ctx, op.Path); err == nil {
		return conflict, "from_exists"
	} else if !errors.Is(err, vfs.ErrNotFound) {
		return conflict, err.Error()
	}
	if _, err := r.fs.StatPath(ctx, op.ToPath); errors.Is(err, vfs.ErrNotFound) {
		return conflict, "missing"
	} else if err != nil {
		return conflict, err.Error()
	}
	if r.dryRun {
		return restored, ""
	}
	if err := r.recordAndDo(ctx, Op{Op: "rename", Path: op.ToPath, ToPath: op.Path}, func() (string, error) {
		return "", r.fs.Rename(ctx, op.ToPath, op.Path)
	}); err != nil {
		return conflict, err.Error()
	}
	return restored, ""
}

// undoDelete puts back a file from its preimage, or an empty directory,
// unless the path exists again.
func (r *rollback) undoDelete(ctx context.Context, op Op) (outcome, string) {
	if op.PreState == "dir" && op.PreReason != "" {
		return skipped, op.PreReason
	}
	if op.PreState == "file" && op.PreBlob == "" {
		if op.PreReason != "" {
			return skipped, op.PreReason
		}
		return skipped, "not_cached"
	}
	if op.PreState == "absent" {
		return skipped, "missing"
	}
	if _, err := r.fs.StatPath(ctx, op.Path); err == nil {
		return conflict, "exists"
	} else if !errors.Is(err, vfs.ErrNotFound) {
		return conflict, err.Error()
	}
	if op.PreState == "dir" {
		if r.dryRun {
			return restored, ""
		}
		if err := r.recordAndDo(ctx, Op{Op: "mkdir", Path: op.Path}, func() (string, error) {
			return "", r.fs.Mkdir(ctx, op.Path)
		}); err != nil {
			return conflict, err.Error()
		}
		return restored, ""
	}
	return r.restoreContent(ctx, op, "create")
}

// restoreContent writes op's preimage to op.Path, recording the write as
// inverse under the rollback session.
func (r *rollback) restoreContent(ctx context.Context, op Op, inverse string) (outcome, string) {
	data, err := r.pre.Open(op.PreBlob)
	if err != nil {
		// A blob that is gone or does not match its hash cannot be
		// written back; the row is honest about it either way.
		slog.Warn("agent: preimage unreadable", "blob", op.PreBlob, "err", err)
		return skipped, "not_cached"
	}
	if r.dryRun {
		return restored, ""
	}
	if err := r.recordAndDo(ctx, Op{Op: inverse, Path: op.Path}, func() (string, error) {
		_, err := r.fs.WriteFile(ctx, op.Path, data, false)
		return ContentVersion(data), err
	}); err != nil {
		return conflict, err.Error()
	}
	return restored, ""
}

// recordAndDo captures the preimage of op.Path, records op under the
// rollback session, runs the write and completes or abandons the row, the
// same sequence a write tool follows.
func (r *rollback) recordAndDo(ctx context.Context, op Op, write func() (string, error)) error {
	pre, err := r.pre.Capture(ctx, r.fs, op.Path)
	if err != nil {
		return err
	}
	seq, err := r.pre.Record(ctx, r.session.ID, op.withPre(pre))
	if err != nil {
		return err
	}
	post, err := write()
	if err != nil {
		_ = r.m.store.AbandonOp(context.WithoutCancel(ctx), seq, "write_failed")
		return err
	}
	return r.m.store.CompleteOp(ctx, seq, post)
}

// beginRollbackSession opens the session a rollback's writes are recorded
// under: the caller's principal, client and transport when ctx carries an
// MCP session, otherwise the console principal. Its connection key is
// unique so it never becomes some connection's current session.
func (m *Sessions) beginRollbackSession(ctx context.Context, of Session) (Session, error) {
	now := m.opt.Now()
	s := Session{
		ID: uuid.NewString(), Transport: rollbackTransport, State: "active",
		StartedAt: now, LastSeenAt: now, Summary: "rollback of " + of.ID,
	}
	if caller, ok := FromContext(ctx); ok && caller.PrincipalID != "" {
		s.PrincipalID, s.ClientName, s.ClientVersion, s.Transport, s.Scope = caller.PrincipalID, caller.ClientName, caller.ClientVersion, caller.Transport, caller.Scope
	} else {
		p, err := m.EnsurePrincipal(ctx, "console", "console", Scope{})
		if err != nil {
			return Session{}, err
		}
		s.PrincipalID, s.ClientName, s.Scope = p.ID, "console", p.Scope
	}
	s.ConnKey = "rollback:" + s.ID
	if err := m.insertSession(ctx, s); err != nil {
		return Session{}, err
	}
	m.store.publish(Event{Kind: "session", Session: &s})
	return s, nil
}
