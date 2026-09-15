package control

import (
	"regexp"
	"strings"
	"testing"
)

// Rolling a session back is the one destructive thing the console can do
// to what an agent wrote, and the TODO's acceptance for it is a shape, not
// a screenshot: the button previews first, the preview asks for the short
// id, and only the branch behind that answer sends confirm. These tests
// read the embedded sources so that a shortcut — a confirm without a
// preview, a plan grouped by hand, paths rendered as markup — fails here.

// rollbackFlow is the source of the one function that talks to
// /sessions/{id}/rollback, cut from its export to the next one.
func rollbackFlow(t *testing.T) string {
	t.Helper()
	src := webSource(t, "web/session_panel.js")
	start := strings.Index(src, "export async function openRollback(")
	if start < 0 {
		t.Fatal("session_panel.js exports no openRollback flow")
	}
	rest := src[start+1:]
	end := strings.Index(rest, "\nexport ")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+1+end]
}

// The dry run comes first, the typed confirmation second, and confirm: true
// is posted exactly once, after both. No other module posts to the route
// at all, so the flow cannot be bypassed from a row's quick entry.
func TestRollbackButtonPreviewsBeforeConfirming(t *testing.T) {
	flow := rollbackFlow(t)
	dry := strings.Index(flow, "{ dry_run: true }")
	ask := strings.Index(flow, "confirmDelete({")
	confirm := strings.Index(flow, "{ confirm: true }")
	if dry < 0 || ask < 0 || confirm < 0 {
		t.Fatalf("the flow lacks a dry run (%d), a typed confirmation (%d) or the confirm post (%d)", dry, ask, confirm)
	}
	if !(dry < ask && ask < confirm) {
		t.Errorf("the order must be dry run (%d) → confirmDelete (%d) → confirm (%d)", dry, ask, confirm)
	}
	// The confirm post sits inside the branch the confirmation opened: the
	// text between the question and the post checks its answer.
	between := flow[ask:confirm]
	if !strings.Contains(flow[:ask], "const ok = await ") || !strings.Contains(between, "if (!ok) return;") {
		t.Error("the confirm post is not guarded by the answer to confirmDelete")
	}
	if !strings.Contains(flow, "'/rollback'") {
		t.Error("the flow does not post to the rollback route")
	}
	for _, name := range webScripts(t) {
		src := webSource(t, name)
		if n := strings.Count(src, "confirm: true }"); name == "web/session_panel.js" {
			if n != 1 {
				t.Errorf("session_panel.js posts confirm: true %d times; the flow allows one", n)
			}
		} else if strings.Contains(src, "/rollback'") {
			t.Errorf("%s talks to the rollback route; only the session panel's flow may", name)
		}
	}
	// The dry run is the first thing the flow does: nothing is posted
	// before it, and the preview is what its answer opens.
	if i := strings.Index(flow, "api.post("); i < 0 || !strings.Contains(flow[i:i+120], "{ dry_run: true }") {
		t.Error("the first request of the flow is not the dry run")
	}
}

// The preview and the result overlay share the three groups of the plan,
// and the preview carries the three promises of docs/agent-roadmap.md §4.8.
func TestRollbackPlanGroupsAndPromise(t *testing.T) {
	panel := webSource(t, "web/session_panel.js")
	for _, want := range []string{
		"import { groupPlan, shortID", "from '/ui/rollback_plan.js'",
		"t('rollback.group.restore', ", "t('rollback.group.skip', ", "t('rollback.group.conflict', ",
		"t('rollback.promise.1')", "t('rollback.promise.2')", "t('rollback.promise.3')",
		"t('rollback.conflict.modified')", "t('rollback.again')", "rollback_session_id",
		"iconEl('undo')", "t('rollback.button')",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("session panel lacks %s", want)
		}
	}
	// The button is not offered for a session already rolled back or one
	// that recorded nothing to roll back.
	if !strings.Contains(panel, "'rolled_back'") {
		t.Error("the panel does not hide the button for a rolled back session")
	}
	plan := webSource(t, "web/rollback_plan.js")
	if strings.Contains(plan, "import ") {
		t.Error("rollback_plan.js imports; it must run under node with no DOM")
	}
	for _, want := range []string{"export function groupPlan(", "export function shortID(", "export function planCounts("} {
		if !strings.Contains(plan, want) {
			t.Errorf("rollback_plan.js lacks %s", want)
		}
	}
	src := webI18nSource(t)
	zh, en := tableKeys(t, src, "zh"), tableKeys(t, src, "en")
	for _, k := range []string{
		"rollback.button", "rollback.preview.title", "rollback.result.title", "rollback.execute", "rollback.again",
		"rollback.group.restore", "rollback.group.skip", "rollback.group.conflict", "rollback.conflict.modified",
		"rollback.promise.1", "rollback.promise.2", "rollback.promise.3", "rollback.confirm.title", "rollback.confirm.body",
		"rollback.nothing", "rollback.done",
		"rollback.pre.ok", "rollback.pre.too_large", "rollback.pre.not_cached", "rollback.pre.dir", "rollback.pre.absent",
		"session.op.create", "session.op.overwrite", "session.op.append", "session.op.edit",
		"session.op.mkdir", "session.op.rename", "session.op.delete",
		"session.ops", "session.ops.empty", "session.col.seq", "session.col.op", "session.col.pre", "session.col.result",
		"session.state.rolled_back", "session.transport.console", "session.rollback.quick",
		"inspector.agenttouch",
	} {
		if !zh[k] {
			t.Errorf("zh table lacks %s", k)
		}
		if !en[k] {
			t.Errorf("en table lacks %s", k)
		}
	}
	// The sessions table names the new state and offers the quick entry
	// through the same flow.
	sessions := webSource(t, "web/screens/agents_sessions.js")
	for _, want := range []string{"openRollback(", "t('session.rollback.quick')", "'rolled_back'", "t('session.transport.' + "} {
		if !strings.Contains(sessions, want) {
			t.Errorf("sessions tab lacks %s", want)
		}
	}
}

// The inspector asks the daemon which session touched the selected file in
// the retention window; the request and the rendering live in
// agent_touch.js, and main.js only mounts it.
func TestInspectorShowsAgentTouch(t *testing.T) {
	touch := webSource(t, "web/agent_touch.js")
	for _, want := range []string{
		"export async function mountAgentTouch(", "'/sessions?path=' + encodeURIComponent(", "&since=",
		"t('inspector.agenttouch')", "openSession(",
	} {
		if !strings.Contains(touch, want) {
			t.Errorf("agent_touch.js lacks %s", want)
		}
	}
	if strings.Contains(touch, "html:") {
		t.Error("agent_touch.js uses the html attribute; the client name is agent text")
	}
	// Seven days is the default retention of preimages (mcp.session.retain);
	// the marker looks back that far and no further.
	if !strings.Contains(touch, "7 * 24 * 60 * 60 * 1000") {
		t.Error("agent_touch.js does not bound since= to the retention window")
	}
	main := webSource(t, "web/screens/main.js")
	if !strings.Contains(main, "import { mountAgentTouch } from '/ui/agent_touch.js'") {
		t.Error("main.js does not import agent_touch.js")
	}
	if strings.Count(main, "mountAgentTouch(") != 1 {
		t.Error("main.js should mount the marker exactly once, in the inspector")
	}
}

// Paths, reasons and results come from the agent and the daemon; they are
// text nodes in every row, never markup.
func TestSessionOpsRenderAsText(t *testing.T) {
	panel := webSource(t, "web/session_panel.js")
	if strings.Contains(panel, "html:") {
		t.Error("session_panel.js uses the html attribute")
	}
	for _, want := range []string{
		"t('session.op.' + ", "t('rollback.pre.' + ", "'data-op'", "to_path", "rollback_result", "pre_reason",
		"t('session.ops')", "t('session.ops.empty')",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("session panel lacks %s", want)
		}
	}
	// A rename shows both paths as text with an arrow between them.
	if !regexp.MustCompile(`o\.to_path \? \[[^\]]*' → '`).MatchString(panel) && !strings.Contains(panel, "' → ', o.to_path") {
		t.Error("a rename row does not show old → new")
	}
	// A plan item's reason is passed to a text node, not interpolated into a
	// translation that could be markup.
	if !strings.Contains(panel, "item.reason") {
		t.Error("plan items do not show their reason")
	}
}

// The typed confirmation is the session's short id, so that a rollback of
// the wrong session cannot be confirmed by muscle memory on a fixed word.
func TestRollbackConfirmTypesTheShortID(t *testing.T) {
	flow := rollbackFlow(t)
	i := strings.Index(flow, "confirmDelete({")
	if i < 0 {
		t.Fatal("no confirmDelete in the flow")
	}
	call := flow[i:]
	if j := strings.Index(call, "})"); j > 0 {
		call = call[:j]
	}
	if !strings.Contains(call, "confirmToken: shortID(id)") {
		t.Errorf("confirmDelete does not ask for shortID(id):\n%s", call)
	}
	if !strings.Contains(call, "confirmLabel: t('rollback.execute')") {
		t.Error("the confirm button is not labelled as executing a rollback")
	}
	plan := webSource(t, "web/rollback_plan.js")
	if !strings.Contains(plan, "slice(0, 8)") {
		t.Error("shortID is not the first eight characters")
	}
}

// Every module this task adds stays small enough to read in one sitting.
func TestRollbackModulesStayShort(t *testing.T) {
	for _, name := range []string{"web/session_panel.js", "web/rollback_plan.js", "web/agent_touch.js", "web/screens/agents_sessions.js", "web/screens/main.js"} {
		if n := strings.Count(webSource(t, name), "\n"); n >= 800 {
			t.Errorf("%s is %d lines; split it", name, n)
		}
	}
}
