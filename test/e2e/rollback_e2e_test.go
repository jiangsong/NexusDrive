package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/mcpsrv"
)

// Session rollback end to end (TODO.md T-38): what an agent writes through
// MCP is what the shell sees at the mount, and what a rollback restores
// through the control plane, or through the console in a real browser, is
// again what the shell sees.

// withSessions hands the daemon's sessions and preimage store to the MCP
// server the way cmd/cloudfs does, with a workspace so begin_session has
// somewhere to deliver.
func withSessions(d *daemon.Daemon, o *mcpsrv.Options) {
	o.Sessions, o.Preimages, o.Workspace = d.Sessions, d.Preimages, "/.agent"
}

// waitFileContent polls the kernel's view of path until it holds want or
// the deadline passes; it returns what was read last. The mount's attribute
// timeout is one second, so a write made through the VFS behind the
// kernel's back can take up to that long to show at the mount point.
func waitFileContent(t *testing.T, path string, want []byte, wait time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(wait)
	var got []byte
	for {
		got, _ = os.ReadFile(path)
		if string(got) == string(want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// treeOf lists everything under dir as the shell would see it: "d <rel>"
// for directories and "f <rel> <content>" for files, sorted, so two trees
// compare item by item.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	out := []string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			out = append(out, "d "+rel)
			return nil
		}
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			// Listed by a directory entry the kernel still had, gone by
			// the time it was opened: not part of the tree.
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, "f "+rel+" "+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// waitTree polls treeOf until it equals want or the deadline passes, and
// returns the last listing. The mount's entry timeout is one second, so a
// change made through the VFS can take that long to show in a listing.
func waitTree(t *testing.T, dir string, want []string, wait time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		got := treeOf(t, dir)
		if strings.Join(got, "\n") == strings.Join(want, "\n") || time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// journalRows is the number of rows in every state, which a dry run must
// leave unchanged.
func journalRows(t *testing.T, s *stack) int {
	t.Helper()
	st, err := s.d.Journal.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st.Pending + st.Uploading + st.Dead + st.Done + st.Cancelling + st.Cancelled + st.Purging
}

// rollbackVia posts one rollback request through the control API and
// decodes the plan.
func rollbackVia(t *testing.T, h http.Handler, id, body string) agent.Plan {
	t.Helper()
	w := uiCall(t, h, "POST", "/sessions/"+id+"/rollback", body)
	if w.Code != 200 {
		t.Fatalf("POST /sessions/%s/rollback %s: %d %s", id, body, w.Code, w.Body.String())
	}
	var plan agent.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("rollback plan: %v\n%s", err, w.Body.String())
	}
	return plan
}

// overwriteThroughMCP is the shared first half of the rollback tests: the
// shell writes a 1 MiB file and reads it (so it is cached), a session
// overwrites it through write_file, the shell sees the new content, and
// the session is finished. It returns the session id and the two contents.
func overwriteThroughMCP(t *testing.T, s *stack) (id string, old, updated []byte) {
	t.Helper()
	old = make([]byte, 1<<20)
	for i := range old {
		old[i] = byte('a' + i%26)
	}
	updated = []byte("replaced by the agent\n")
	shellPath := filepath.Join(s.dir, "report.txt")
	if err := os.WriteFile(shellPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)
	if got, err := os.ReadFile(shellPath); err != nil || string(got) != string(old) {
		t.Fatalf("cat after the shell write: %d bytes, %v", len(got), err)
	}
	var b beginSessionOutput
	if res := s.callTool(t, "begin_session", map[string]any{}, &b); res.IsError {
		t.Fatalf("begin_session: %s", toolText(res))
	}
	if res := s.callTool(t, "write_file", map[string]any{"path": "/report.txt", "content": string(updated)}, nil); res.IsError {
		t.Fatalf("write_file: %s", toolText(res))
	}
	if got := waitFileContent(t, shellPath, updated, 5*time.Second); string(got) != string(updated) {
		t.Fatalf("cat after the agent's write: %q", got)
	}
	if res := s.callTool(t, "finish_session", map[string]any{"summary": "overwrote report.txt"}, nil); res.IsError {
		t.Fatalf("finish_session: %s", toolText(res))
	}
	return b.SessionID, old, updated
}

// TestRollbackRestoresWhatTheShellSees needs a kernel mount. An agent's
// overwrite of a cached 1 MiB file is visible at the mount point at once;
// the control plane's dry run reports the plan and writes nothing; the
// confirmed rollback puts the old content back, again visible to the
// shell, without a single download from the remote; and the restored
// content reaches the remote through the ordinary upload queue. A second
// session's create → move → delete is then undone to the tree the shell
// had before it, item by item.
func TestRollbackRestoresWhatTheShellSees(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{mcp: withSessions})
	ctx := context.Background()
	h := control.NewServer(s.d.Collector()).Handler()

	reads, links := s.fake.Calls("ReadRange"), s.fake.Calls("DownloadURL")
	id, old, updated := overwriteThroughMCP(t, s)
	shellPath := filepath.Join(s.dir, "report.txt")

	// GET /sessions/{id} lists the one op with its preimage state.
	var detail control.SessionDetail
	if w := uiCall(t, h, "GET", "/sessions/"+id, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
		t.Fatalf("GET /sessions/%s: %d %s", id, w.Code, w.Body.String())
	}
	if len(detail.Ops) != 1 || detail.Ops[0].Op != "overwrite" || detail.Ops[0].Path != "/report.txt" || detail.Ops[0].PreState != "file" || detail.Ops[0].PreReason != "" {
		t.Fatalf("session ops: %+v", detail.Ops)
	}

	// The dry run: the plan says what a rollback would restore and the
	// journal has not moved.
	s.settle(t)
	rowsBefore, uploads := journalRows(t, s), s.fake.Calls("BeginUpload")
	plan := rollbackVia(t, h, id, `{"dry_run":true}`)
	if !plan.DryRun || len(plan.Restored) != 1 || plan.Restored[0].Path != "/report.txt" || len(plan.Skipped)+len(plan.Conflict) != 0 {
		t.Fatalf("dry-run plan: %+v", plan)
	}
	if got := journalRows(t, s); got != rowsBefore {
		t.Fatalf("the dry run changed the journal: %d rows, was %d", got, rowsBefore)
	}
	if got := waitFileContent(t, shellPath, updated, time.Second); string(got) != string(updated) {
		t.Fatalf("the dry run changed the file: %q", got)
	}
	if got := s.fake.Calls("BeginUpload"); got != uploads {
		t.Fatalf("the dry run started %d uploads", got-uploads)
	}
	// Executing needs confirm; the control plane refuses without it.
	if w := uiCall(t, h, "POST", "/sessions/"+id+"/rollback", `{}`); w.Code == 200 {
		t.Fatalf("a rollback without confirm or dry_run was executed: %s", w.Body.String())
	}

	// The confirmed rollback: the shell reads the old content back.
	plan = rollbackVia(t, h, id, `{"confirm":true}`)
	if plan.DryRun || len(plan.Restored) != 1 || plan.RollbackSessionID == "" {
		t.Fatalf("rollback plan: %+v", plan)
	}
	if got := waitFileContent(t, shellPath, old, 5*time.Second); string(got) != string(old) {
		t.Fatalf("cat after the rollback: %d bytes, first %q", len(got), got[:min(len(got), 32)])
	}
	if d := s.fake.Calls("ReadRange") - reads; d != 0 {
		t.Errorf("the write and its rollback downloaded from the remote: %d ReadRange calls", d)
	}
	if d := s.fake.Calls("DownloadURL") - links; d != 0 {
		t.Errorf("the write and its rollback asked for %d download links", d)
	}
	s.settle(t)
	if got, ok := s.fake.Content("report.txt"); !ok || string(got) != string(old) {
		t.Fatalf("remote after the rollback: %d bytes (%v)", len(got), ok)
	}
	sess, err := s.d.Sessions.Get(ctx, id)
	if err != nil || sess.State != "rolled_back" {
		t.Fatalf("session after the rollback: %+v %v", sess, err)
	}
	// The rollback's own session recorded its one write, so it can be
	// rolled back in turn; a dry run of that says so.
	if again := rollbackVia(t, h, plan.RollbackSessionID, `{"dry_run":true}`); len(again.Restored) != 1 {
		t.Fatalf("the rollback session's own plan: %+v", again)
	}

	// create → move → delete, then the tree is what the shell had before.
	treeDir := filepath.Join(s.dir, "tree")
	if err := os.Mkdir(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"keep.txt": "keep me\n", "old.txt": "rename me\n"} {
		if err := os.WriteFile(filepath.Join(treeDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.settle(t)
	before := treeOf(t, treeDir)
	var b beginSessionOutput
	if res := s.callTool(t, "begin_session", map[string]any{}, &b); res.IsError {
		t.Fatalf("begin_session: %s", toolText(res))
	}
	if res := s.callTool(t, "write_file", map[string]any{"path": "/tree/new.txt", "content": "created by the agent\n"}, nil); res.IsError {
		t.Fatalf("write_file: %s", toolText(res))
	}
	if res := s.callTool(t, "move", map[string]any{"from": "/tree/old.txt", "to": "/tree/renamed.txt"}, nil); res.IsError {
		t.Fatalf("move: %s", toolText(res))
	}
	if res := s.callTool(t, "delete", map[string]any{"path": "/tree/keep.txt", "confirm": true}, nil); res.IsError {
		t.Fatalf("delete: %s", toolText(res))
	}
	if res := s.callTool(t, "finish_session", map[string]any{}, nil); res.IsError {
		t.Fatalf("finish_session: %s", toolText(res))
	}
	want := []string{"f new.txt created by the agent\n", "f renamed.txt rename me\n"}
	if during := waitTree(t, treeDir, want, 5*time.Second); strings.Join(during, "\n") != strings.Join(want, "\n") {
		entries, err := os.ReadDir(treeDir)
		vfsEntries, verr := s.d.FS.ReadDirPath(ctx, "/tree")
		t.Fatalf("the shell does not see the session's changes: %v\nreaddir %v %v\nvfs %+v %v", during, entries, err, vfsEntries, verr)
	}
	plan = rollbackVia(t, h, b.SessionID, `{"confirm":true}`)
	if len(plan.Restored) != 3 || len(plan.Skipped)+len(plan.Conflict) != 0 {
		t.Fatalf("rollback of create → move → delete: %+v", plan)
	}
	if after := waitTree(t, treeDir, before, 5*time.Second); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("tree after the rollback differs from before the session:\n got %v\nwant %v", after, before)
	}
	s.settle(t)
	for name, body := range map[string]string{"tree/keep.txt": "keep me\n", "tree/old.txt": "rename me\n"} {
		if got, ok := s.fake.Content(name); !ok || string(got) != body {
			t.Fatalf("remote %s after the rollback: %q (%v)", name, got, ok)
		}
	}
	for _, name := range []string{"tree/new.txt", "tree/renamed.txt"} {
		if _, ok := s.fake.Content(name); ok {
			t.Fatalf("remote still has %s after the rollback", name)
		}
	}

	// A file the shell edits after the session is a conflict and is left
	// alone; the session's other file is restored.
	for _, name := range []string{"c1.txt", "c2.txt"} {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(name+" by the shell\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.settle(t)
	if res := s.callTool(t, "begin_session", map[string]any{}, &b); res.IsError {
		t.Fatalf("begin_session: %s", toolText(res))
	}
	for _, name := range []string{"c1.txt", "c2.txt"} {
		if res := s.callTool(t, "write_file", map[string]any{"path": "/" + name, "content": name + " by the agent\n"}, nil); res.IsError {
			t.Fatalf("write_file %s: %s", name, toolText(res))
		}
	}
	if res := s.callTool(t, "finish_session", map[string]any{}, nil); res.IsError {
		t.Fatalf("finish_session: %s", toolText(res))
	}
	if got := waitFileContent(t, filepath.Join(s.dir, "c1.txt"), []byte("c1.txt by the agent\n"), 5*time.Second); string(got) != "c1.txt by the agent\n" {
		t.Fatalf("cat c1.txt after the agent's write: %q", got)
	}
	// The shell edits c1.txt through the kernel.
	if err := os.WriteFile(filepath.Join(s.dir, "c1.txt"), []byte("c1.txt edited by the shell afterwards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)
	plan = rollbackVia(t, h, b.SessionID, `{"dry_run":true}`)
	if len(plan.Conflict) != 1 || plan.Conflict[0].Path != "/c1.txt" || plan.Conflict[0].Reason != "modified" || len(plan.Restored) != 1 || plan.Restored[0].Path != "/c2.txt" {
		t.Fatalf("dry-run plan after a kernel edit: %+v", plan)
	}
	plan = rollbackVia(t, h, b.SessionID, `{"confirm":true}`)
	if len(plan.Conflict) != 1 || plan.Conflict[0].Path != "/c1.txt" || len(plan.Restored) != 1 || plan.Restored[0].Path != "/c2.txt" {
		t.Fatalf("rollback plan after a kernel edit: %+v", plan)
	}
	if got := waitFileContent(t, filepath.Join(s.dir, "c2.txt"), []byte("c2.txt by the shell\n"), 5*time.Second); string(got) != "c2.txt by the shell\n" {
		t.Fatalf("cat c2.txt after the rollback: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(s.dir, "c1.txt")); err != nil || string(got) != "c1.txt edited by the shell afterwards\n" {
		t.Fatalf("the conflicting file was overwritten: %q %v", got, err)
	}
	if w := uiCall(t, h, "GET", "/sessions/"+b.SessionID, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
		t.Fatalf("GET /sessions/%s: %d %s", b.SessionID, w.Code, w.Body.String())
	}
	results := map[string]string{}
	for _, op := range detail.Ops {
		results[op.Path] = op.RollbackResult
	}
	if results["/c1.txt"] != "conflict: modified" || results["/c2.txt"] != "restored" {
		t.Fatalf("rollback results on the rows: %v", results)
	}
}

// TestRollbackInTheBrowser is the acceptance line of T-38 in full: MCP
// writes, the shell reads the new content, a person rolls the session
// back in the console (rendered in headless Chromium: the session panel's
// rollback button, the preview, the typed confirmation, the result), and
// the shell reads the old content. The browser half runs only with
// CLOUDFS_BROWSER=1; TestRollbackRestoresWhatTheShellSees covers the same
// path through the control API without one.
func TestRollbackInTheBrowser(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{mcp: withSessions})
	chrome := requireBrowser(t)
	id, old, updated := overwriteThroughMCP(t, s)
	shellPath := filepath.Join(s.dir, "report.txt")
	short := id[:8]

	base := startControlUI(t, s.d.Collector())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := startBrowser(ctx, chrome)
	if err != nil {
		t.Fatalf("headless chrome: %v", err)
	}
	defer b.close()
	page, err := b.open(ctx, base+"/?lang=en#/agents?session="+id)
	if err != nil {
		t.Fatal(err)
	}
	// The session panel, with its operation row and the rollback button.
	page.waitFor(t, `document.querySelector('[data-action=rollback]') && document.querySelector('[data-op]')`)
	if dom := page.dom(t); !strings.Contains(dom, "/report.txt") {
		t.Fatalf("the session panel does not list the overwritten path:\n%s", dom)
	}
	// Rollback → preview (a dry run) with the path in the restore group.
	page.click(t, "[data-action=rollback]")
	page.waitFor(t, `document.querySelector('[data-rollback=preview]')`)
	if dom := page.dom(t); !strings.Contains(dom, "/report.txt") || !strings.Contains(dom, `data-action="execute"`) {
		t.Fatalf("the preview lacks the path or the execute button:\n%s", dom)
	}
	if got := waitFileContent(t, shellPath, updated, 500*time.Millisecond); string(got) != string(updated) {
		t.Fatalf("the preview changed the file: %q", got)
	}
	// Execute → the typed confirmation of the short id.
	page.click(t, "[data-action=execute]")
	page.waitFor(t, `document.querySelector('.sheet.danger input[type=text]')`)
	page.eval(t, fmt.Sprintf(`(() => { const i = document.querySelector('.sheet.danger input[type=text]'); i.value = %q; i.dispatchEvent(new Event('input', { bubbles: true })); return true; })()`, short))
	page.waitFor(t, `[...document.querySelectorAll('.sheet.danger button.danger')].some((b) => !b.disabled)`)
	page.click(t, ".sheet.danger button.danger:not([disabled])")
	// The result overlay, with the offer to roll the rollback back.
	page.waitFor(t, `document.querySelector('[data-rollback=result]')`)
	if dom := page.dom(t); !strings.Contains(dom, `data-action="rollback-again"`) || !strings.Contains(dom, "/report.txt") {
		t.Fatalf("the result overlay lacks the rolled-back path or the rollback-again button:\n%s", dom)
	}
	// The shell reads the old content.
	if got := waitFileContent(t, shellPath, old, 5*time.Second); string(got) != string(old) {
		t.Fatalf("cat after the browser rollback: %d bytes, first %q", len(got), got[:min(len(got), 32)])
	}
	if sess, err := s.d.Sessions.Get(context.Background(), id); err != nil || sess.State != "rolled_back" {
		t.Fatalf("session after the browser rollback: %+v %v", sess, err)
	}
	s.settle(t)
	if got, ok := s.fake.Content("report.txt"); !ok || string(got) != string(old) {
		t.Fatalf("remote after the browser rollback: %d bytes (%v)", len(got), ok)
	}
}

// page is one tab of the headless browser driven through its DevTools
// session: JavaScript in, values out. It is what a flow with clicks and
// typing needs beyond renderedDOM's load-and-read.
type page struct {
	b   *browser
	ctx context.Context
	sid string
}

// open creates a tab, navigates it to url and waits for the load event.
func (b *browser) open(ctx context.Context, url string) (*page, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	res, err := b.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(res, &created); err != nil {
		return nil, err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	res, err = b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(res, &attached); err != nil {
		return nil, err
	}
	p := &page{b: b, ctx: ctx, sid: attached.SessionID}
	if _, err := b.call(ctx, p.sid, "Page.enable", nil, nil); err != nil {
		return nil, err
	}
	events := make(chan cdpMessage, 64)
	if _, err := b.call(ctx, p.sid, "Page.navigate", map[string]any{"url": url}, events); err != nil {
		return nil, err
	}
	loadCtx, cancelLoad := context.WithTimeout(ctx, 30*time.Second)
	defer cancelLoad()
	if err := b.waitEvent(loadCtx, events, p.sid, "Page.loadEventFired"); err != nil {
		return nil, fmt.Errorf("waiting for the load event: %w", err)
	}
	return p, nil
}

// eval runs expr in the page and returns its value as JSON.
func (p *page) eval(t *testing.T, expr string) json.RawMessage {
	t.Helper()
	res, err := p.b.call(p.ctx, p.sid, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, nil)
	if err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	if out.ExceptionDetails != nil {
		desc := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil {
			desc = out.ExceptionDetails.Exception.Description
		}
		t.Fatalf("evaluate %s threw: %s", expr, desc)
	}
	return out.Result.Value
}

// waitFor polls until expr is truthy, failing the test with the DOM
// after thirty seconds.
func (p *page) waitFor(t *testing.T, expr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if v := p.eval(t, "!!("+expr+")"); string(v) == "true" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page never satisfied %s:\n%s", expr, p.dom(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// click clicks the first element matching selector, which must exist.
func (p *page) click(t *testing.T, selector string) {
	t.Helper()
	if v := p.eval(t, fmt.Sprintf(`(() => { const n = document.querySelector(%q); if (!n) return false; n.click(); return true; })()`, selector)); string(v) != "true" {
		t.Fatalf("nothing matches %s:\n%s", selector, p.dom(t))
	}
}

func (p *page) dom(t *testing.T) string {
	t.Helper()
	dom, err := p.b.outerHTML(p.ctx, p.sid)
	if err != nil {
		t.Fatal(err)
	}
	return dom
}
