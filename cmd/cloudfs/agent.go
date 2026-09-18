package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
)

// auditQueryFromArgs turns the `cloudfs audit` flags into a store query.
// --since takes a duration back from now ("1h", "7d" spelled as "168h") or
// an RFC 3339 instant.
func auditQueryFromArgs(args []string, now time.Time) (agent.AuditQuery, bool, error) {
	f := parseFlags(args, "json")
	for k := range f.values {
		switch k {
		case "config", "session", "tool", "result", "since", "limit", "cursor", "timeout":
		default:
			return agent.AuditQuery{}, false, fmt.Errorf("audit: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" {
			return agent.AuditQuery{}, false, fmt.Errorf("audit: --%s requires a value or is unknown", k)
		}
	}
	if len(f.args) > 0 {
		return agent.AuditQuery{}, false, errors.New("audit: unexpected positional argument")
	}
	q := agent.AuditQuery{Session: f.str("session", ""), Tool: f.str("tool", ""), Result: f.str("result", ""), Cursor: f.str("cursor", "")}
	switch q.Result {
	case "", "ok", "denied", "error":
	default:
		return agent.AuditQuery{}, false, errors.New("audit: --result must be ok, denied or error")
	}
	if raw := f.str("since", ""); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			q.Since = now.Add(-d)
		} else if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			q.Since = ts
		} else {
			return agent.AuditQuery{}, false, errors.New("audit: --since takes a duration such as 1h or an RFC 3339 time")
		}
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return agent.AuditQuery{}, false, errors.New("audit: --limit must be between 1 and 1000")
		}
		q.Limit = n
	}
	return q, f.bools["json"], nil
}

// runAudit lists audit rows: from the running daemon when there is one,
// otherwise straight from agent.db, read-only.
func runAudit(ctx context.Context, args []string, out io.Writer) error {
	q, asJSON, err := auditQueryFromArgs(args, time.Now())
	if err != nil {
		return err
	}
	f := parseFlags(args, "json")
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("audit: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	result, online, err := control.CallAudit(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if !online {
		view, err := openAgentOffline(cfg.StateDir())
		if errors.Is(err, os.ErrNotExist) {
			return printAudit(out, control.AuditResponse{Rows: []control.AuditView{}}, asJSON)
		}
		if err != nil {
			return err
		}
		defer view.close()
		rows, next, err := view.Audit(ctx, q)
		if err != nil {
			return err
		}
		result = control.AuditResponse{Rows: view.auditViews(ctx, rows), NextCursor: next}
	}
	return printAudit(out, result, asJSON)
}

func printAudit(out io.Writer, result control.AuditResponse, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(out, "no audit rows")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tCLIENT\tSESSION\tTOOL\tRESULT\tPATHS\tBYTES IN\tBYTES OUT\tMS")
	for _, r := range result.Rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\n",
			r.TS.Local().Format("2006-01-02 15:04:05"), r.Client, shortID(r.SessionID), r.Tool, r.Result,
			strings.Join(r.Paths, ","), r.BytesIn, r.BytesOut, r.DurationMS)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if result.NextCursor != "" {
		fmt.Fprintf(out, "more rows: use audit --cursor %s\n", result.NextCursor)
	}
	return nil
}

// runSessions lists, shows, finishes or rolls back agent sessions. Listing
// and showing work offline against agent.db; finishing and rolling back
// change state the daemon owns.
func runSessions(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json", "sandbox", "confirm", "dry-run")
	for k := range f.values {
		switch k {
		case "config", "timeout", "limit", "cursor", "state", "path", "summary":
		default:
			return fmt.Errorf("sessions: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		switch k {
		case "json", "sandbox", "confirm", "dry-run":
		default:
			return fmt.Errorf("sessions: --%s requires a value or is unknown", k)
		}
	}
	action := f.arg(0)
	if action == "" {
		action = "list"
	}
	id := f.arg(1)
	switch action {
	case "list":
		if len(f.args) > 1 {
			return errors.New("sessions list: unexpected argument")
		}
	case "show", "finish", "rollback":
		if len(f.args) != 2 || id == "" {
			return fmt.Errorf("sessions %s: give one session ID", action)
		}
	default:
		return fmt.Errorf("sessions: unknown action %q", action)
	}
	if action == "rollback" && !f.bools["confirm"] && !f.bools["dry-run"] {
		return errors.New("sessions rollback: pass --dry-run to preview the plan, or --confirm to execute it")
	}
	q := agent.ListQuery{Cursor: f.str("cursor", ""), State: f.str("state", ""), Path: f.str("path", ""), Sandbox: f.bools["sandbox"]}
	switch q.State {
	case "", "active", "finished", "expired", "rolled_back":
	default:
		return errors.New("sessions: --state must be active, finished, expired or rolled_back")
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return errors.New("sessions: --limit must be between 1 and 200")
		}
		q.Limit = n
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("sessions: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	socket, tcp := cfg.Control.Socket, cfg.Control.Metrics
	asJSON := f.bools["json"]
	switch action {
	case "rollback":
		dryRun := f.bools["dry-run"]
		plan, online, err := control.CallRollbackSession(ctx, socket, tcp, id, dryRun)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("sessions rollback requires the running daemon; start `cloudfs mount` or `cloudfs mcp --http` first")
		}
		return printRollbackPlan(out, plan, asJSON)
	case "finish":
		finished, online, err := control.CallFinishSession(ctx, socket, tcp, id, f.str("summary", ""))
		if err != nil {
			return err
		}
		if !online {
			return errors.New("sessions finish requires the running daemon; start `cloudfs mount` or `cloudfs mcp --http` first")
		}
		if asJSON {
			return json.NewEncoder(out).Encode(finished)
		}
		fmt.Fprintf(out, "session %s: %s\n", finished.ID, finished.State)
		return nil
	case "show":
		detail, online, err := control.CallSession(ctx, socket, tcp, id)
		if err != nil {
			return err
		}
		if !online {
			view, err := openAgentOffline(cfg.StateDir())
			if err != nil {
				return err
			}
			defer view.close()
			detail, err = view.detail(ctx, id)
			if err != nil {
				return err
			}
		}
		return printSessionDetail(out, detail, asJSON)
	}
	result, online, err := control.CallSessions(ctx, socket, tcp, q)
	if err != nil {
		return err
	}
	if !online {
		view, err := openAgentOffline(cfg.StateDir())
		if errors.Is(err, os.ErrNotExist) {
			return printSessions(out, control.SessionsResponse{Sessions: []control.SessionView{}}, asJSON)
		}
		if err != nil {
			return err
		}
		defer view.close()
		result, err = view.list(ctx, q)
		if err != nil {
			return err
		}
	}
	return printSessions(out, result, asJSON)
}

func printSessions(out io.Writer, result control.SessionsResponse, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	if len(result.Sessions) == 0 {
		fmt.Fprintln(out, "no agent sessions")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCLIENT\tTRANSPORT\tSTATE\tSTARTED\tLAST SEEN\tWRITES\tSCOPE")
	for _, s := range result.Sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", s.ID, s.Client, s.Transport, s.State,
			s.StartedAt.Local().Format("2006-01-02 15:04:05"), s.LastSeenAt.Local().Format("15:04:05"), s.Writes, scopeSummary(s.Scope))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if result.NextCursor != "" {
		fmt.Fprintf(out, "more sessions: use sessions list --cursor %s\n", result.NextCursor)
	}
	return nil
}

// printRollbackPlan prints the three groups of a plan: what was (or would
// be) restored, what was skipped and why, and what conflicts.
func printRollbackPlan(out io.Writer, plan agent.Plan, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(plan)
	}
	restore, skip, conflict := "restored", "skipped", "conflict"
	if plan.DryRun {
		fmt.Fprintf(out, "dry run of session %s: nothing was written\n", plan.SessionID)
		restore, skip = "would restore", "would skip"
	} else {
		fmt.Fprintf(out, "rolled back session %s (rollback session %s)\n", plan.SessionID, plan.RollbackSessionID)
	}
	printPlanGroup(out, restore, plan.Restored)
	printPlanGroup(out, skip, plan.Skipped)
	printPlanGroup(out, conflict, plan.Conflict)
	return nil
}

func printPlanGroup(out io.Writer, label string, items []agent.PlanItem) {
	fmt.Fprintf(out, "%s %d\n", label, len(items))
	for _, it := range items {
		line := "  " + it.Op + " " + it.Path
		if it.ToPath != "" {
			line += " -> " + it.ToPath
		}
		if it.Reason != "" {
			line += " (" + it.Reason + ")"
		}
		fmt.Fprintln(out, line)
	}
}

func printSessionDetail(out io.Writer, detail control.SessionDetail, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(detail)
	}
	s := detail.Session
	fmt.Fprintf(out, "session %s\n  client: %s %s\n  transport: %s\n  state: %s\n  scope: %s\n  started: %s\n  last seen: %s\n  writes: %d\n",
		s.ID, s.Client, s.ClientVersion, s.Transport, s.State, scopeSummary(s.Scope),
		s.StartedAt.Local().Format(time.RFC3339), s.LastSeenAt.Local().Format(time.RFC3339), s.Writes)
	if s.FinishedAt != nil {
		fmt.Fprintf(out, "  finished: %s\n", s.FinishedAt.Local().Format(time.RFC3339))
	}
	if s.RolledBackAt != nil {
		fmt.Fprintf(out, "  rolled back: %s\n", s.RolledBackAt.Local().Format(time.RFC3339))
	}
	if s.Summary != "" {
		fmt.Fprintf(out, "  summary: %s\n", s.Summary)
	}
	if s.Workspace != "" {
		fmt.Fprintf(out, "  workspace: %s (%d artifacts)\n", s.Workspace, s.ArtifactCount)
	}
	if len(detail.Ops) > 0 {
		fmt.Fprintf(out, "recorded writes (%d, rollback with `sessions rollback %s --dry-run`):\n", len(detail.Ops), s.ID)
		for _, op := range detail.Ops {
			line := fmt.Sprintf("  %d %s %s", op.Seq, op.Op, op.Path)
			if op.ToPath != "" {
				line += " -> " + op.ToPath
			}
			line += " [" + preimageLabel(op) + "]"
			if op.RollbackResult != "" {
				line += " " + op.RollbackResult
			}
			fmt.Fprintln(out, line)
		}
	}
	if len(detail.Audit) == 0 {
		return nil
	}
	fmt.Fprintln(out, "recent calls:")
	return printAudit(out, control.AuditResponse{Rows: detail.Audit}, false)
}

// preimageLabel is the one-word state of an op's preimage: what a
// rollback has to work with.
func preimageLabel(op agent.Op) string {
	switch {
	case op.PreState == "absent":
		return "new"
	case op.PreState == "dir":
		return "dir"
	case op.PreReason != "":
		return op.PreReason
	}
	return "restorable"
}

// scopeSummary is the one-line form of a scope for a table cell.
func scopeSummary(sc agent.Scope) string {
	parts := []string{}
	if len(sc.Read) == 0 {
		parts = append(parts, "read /")
	} else {
		parts = append(parts, "read "+strings.Join(sc.Read, ","))
	}
	switch {
	case sc.ReadOnly:
		parts = append(parts, "read-only")
	case sc.Sandbox != "":
		parts = append(parts, "write "+sc.Sandbox)
	case sc.Write != nil:
		if len(sc.Write) == 0 {
			parts = append(parts, "no writes")
		} else {
			parts = append(parts, "write "+strings.Join(sc.Write, ","))
		}
	}
	if !sc.ExpiresAt.IsZero() {
		parts = append(parts, "until "+sc.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, "; ")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// offlineAgent reads agent.db without a daemon, through the same view the
// control plane serves, so the two outputs cannot drift.
type offlineAgent struct {
	control.AgentView
	st *agent.Store
}

func openAgentOffline(cacheDir string) (*offlineAgent, error) {
	st, err := agent.OpenReadOnly(filepath.Join(cacheDir, "agent"))
	if err != nil {
		return nil, err
	}
	return &offlineAgent{AgentView: control.NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "", control.RollbackDeps{}), st: st}, nil
}

func (o *offlineAgent) close() { _ = o.st.Close() }

func (o *offlineAgent) auditViews(ctx context.Context, rows []agent.AuditRow) []control.AuditView {
	return control.AuditViews(ctx, o.AgentView, rows)
}

func (o *offlineAgent) list(ctx context.Context, q agent.ListQuery) (control.SessionsResponse, error) {
	sessions, next, err := o.Sessions(ctx, q)
	if err != nil {
		return control.SessionsResponse{}, err
	}
	out := control.SessionsResponse{Sessions: make([]control.SessionView, 0, len(sessions)), NextCursor: next}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, control.SessionViewOf(s))
	}
	if out.Summary, err = o.Summary(ctx); err != nil {
		return control.SessionsResponse{}, err
	}
	return out, nil
}

func (o *offlineAgent) detail(ctx context.Context, id string) (control.SessionDetail, error) {
	return control.SessionDetailOf(ctx, o.AgentView, id)
}
