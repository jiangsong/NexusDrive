package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Session rollback (docs/agent-roadmap.md §4.8, TODO.md T-38). Every write
// tool calls beforeWrite before its first VFS write: with a session in the
// context and a preimage store configured, the state of the path is
// captured and a session_ops row recorded, then the tool runs and reports
// the outcome through done or failed. rollback_session replays a session's
// rows in reverse through agent.Sessions.Rollback. Nothing here lives in
// vfs: the VFS sees ordinary reads and writes.

// opRecord is one recorded write awaiting its outcome. A nil record (no
// session, no store, or a capture that returned an error) is inert, so
// the tools call done and failed without checking.
type opRecord struct {
	s   *Server
	seq int64
	pre agent.Pre
}

// beforeWrite captures the preimage of p and records the op under the
// caller's session. op is create | overwrite | append | edit | mkdir |
// rename | delete; to is the destination of a rename. reason, when set,
// overrides the capture's pre_reason (a recursive directory delete records
// "dir"). Capture never refuses the write: an unreadable or oversized file
// gets a reason on its row and the tool goes ahead.
func (s *Server) beforeWrite(ctx context.Context, op, p, to, reason string) *opRecord {
	pre := s.opt.Preimages
	if pre == nil {
		return nil
	}
	sess, ok := agent.FromContext(ctx)
	if !ok {
		return nil
	}
	captured, err := pre.Capture(ctx, agent.VFSOps(s.opt.FS), p)
	if err != nil {
		return nil
	}
	if reason != "" && captured.State == "dir" {
		captured.Reason = reason
	}
	seq, err := pre.Record(ctx, sess.ID, agent.Op{Op: op, Path: p, ToPath: to}.WithPre(captured))
	if err != nil {
		// The write still happens; only its undo is lost, and the log
		// says so. An agent must not lose a tool because agent.db is full.
		slog.Warn("mcp: session op not recorded", "tool", op, "path", p, "err", err)
		return nil
	}
	return &opRecord{s: s, seq: seq, pre: captured}
}

// done records what the write left behind: the ContentVersion of the
// bytes written for content ops, a version for a copy, "" otherwise.
func (r *opRecord) done(ctx context.Context, postVersion string) {
	if r == nil {
		return
	}
	if err := r.s.opt.Preimages.Store().CompleteOp(context.WithoutCancel(ctx), r.seq, postVersion); err != nil {
		slog.Warn("mcp: session op not completed", "seq", r.seq, "err", err)
	}
}

// failed marks the row of a write that returned an error, so rollback
// skips it rather than undo a write that did not happen.
func (r *opRecord) failed(ctx context.Context) {
	if r == nil {
		return
	}
	if err := r.s.opt.Preimages.Store().AbandonOp(context.WithoutCancel(ctx), r.seq, "write_failed"); err != nil {
		slog.Warn("mcp: session op not abandoned", "seq", r.seq, "err", err)
	}
}

// preContent is the bytes the preimage holds, for a post-state hash that
// depends on them (append), or nil when none was kept.
func (r *opRecord) preContent() []byte {
	if r == nil || r.pre.Blob == "" {
		return nil
	}
	data, err := r.s.opt.Preimages.Open(r.pre.Blob)
	if err != nil {
		return nil
	}
	return data
}

// postContent is the PostVersion of a write_file call: the hash of the
// bytes the file holds afterwards. An append onto a file whose preimage
// was not kept has no known post state, so its row gets "" and rollback
// skips it, as it would anyway for want of a preimage.
func postContent(rec *opRecord, mode string, content []byte) string {
	if mode != "append" {
		return agent.ContentVersion(content)
	}
	if rec == nil {
		return ""
	}
	if rec.pre.State == "absent" {
		return agent.ContentVersion(content)
	}
	before := rec.preContent()
	if before == nil {
		return ""
	}
	return agent.ContentVersion(append(before, content...))
}

// linkOps sets audit_id on the rows a call recorded, once the audit row
// exists. The audit middleware calls it with the note it gave the call.
func (s *Server) linkOps(ctx context.Context, note *agent.OpNote, auditID int64) {
	seqs := note.Seqs()
	if len(seqs) == 0 || s.opt.Preimages == nil {
		return
	}
	if err := s.opt.Preimages.Store().LinkOpsToAudit(ctx, seqs, auditID); err != nil {
		slog.Warn("mcp: session ops not linked to audit", "err", err)
	}
}

type rollbackSessionInput struct {
	SessionID string `json:"session_id" jsonschema:"Session whose writes to undo; list_sessions shows them"`
	Confirm   bool   `json:"confirm,omitempty" jsonschema:"Must be true to execute; the rollback writes through the mount and uploads like any other change"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"Only report what would be restored, skipped or conflicts; needs no confirm"`
}

func (s *Server) registerRollbackTool() {
	if s.opt.Sessions == nil || s.opt.Preimages == nil {
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "rollback_session",
		Description: "Undo the writes a session made through this server, newest first, from the content kept before each write. " +
			"A file someone changed after the session is reported as a conflict and left alone; files too large to keep, " +
			"unreadable ones and recursive directory deletes are skipped. dry_run=true previews the plan. " +
			"The rollback is a new session of its own and can be rolled back in turn. It is not a remote version restore: " +
			"the restored files upload like any other change.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, s.rollbackSession)
}

func (s *Server) rollbackSession(ctx context.Context, _ *mcp.CallToolRequest, in rollbackSessionInput) (*mcp.CallToolResult, agent.Plan, error) {
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, agent.Plan{}, nil
	}
	if in.SessionID == "" {
		r, _ := fail(errors.New("session_id is required"))
		return r, agent.Plan{}, nil
	}
	if !in.DryRun {
		if err := s.checkWrite(ctx); err != nil {
			r, _ := fail(err)
			return r, agent.Plan{}, nil
		}
		if !in.Confirm {
			r, _ := fail(fmt.Errorf("refusing to roll back session %s without confirm=true; pass dry_run=true to preview the plan first", in.SessionID))
			return r, agent.Plan{}, nil
		}
	}
	// Like finish_session: an agent handles its own principal's sessions.
	// The console and the CLI, which answer to the person, may roll back
	// any session through the control plane.
	cur, ok := agent.FromContext(ctx)
	if !ok {
		r, _ := fail(errors.New("no session on this connection"))
		return r, agent.Plan{}, nil
	}
	target, err := s.opt.Sessions.Get(ctx, in.SessionID)
	if err != nil {
		r, _ := fail(err)
		return r, agent.Plan{}, nil
	}
	if target.PrincipalID != cur.PrincipalID {
		r, _ := fail(errSessionNotYours)
		return r, agent.Plan{}, nil
	}
	plan, _, err := s.opt.Sessions.Rollback(ctx, agent.VFSOps(s.opt.FS), s.opt.Preimages, in.SessionID, in.DryRun)
	if err != nil {
		r, _ := fail(mapErr(err, in.SessionID))
		return r, agent.Plan{}, nil
	}
	verb := "rolled back"
	if in.DryRun {
		verb = "would roll back"
	}
	return text("%s session %s: %d restored, %d skipped, %d conflicts", verb, in.SessionID,
		len(plan.Restored), len(plan.Skipped), len(plan.Conflict)), plan, nil
}
