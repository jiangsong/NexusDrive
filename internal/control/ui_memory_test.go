package control

import (
	"regexp"
	"strings"
	"testing"

	"cloudfs/internal/memory"
)

// The memory tab is the console's view of what an agent remembers, and
// the editor behind it is the one place a person rewrites a fact by hand.
// These tests read the shipped sources the way the other ui_*_test.go
// files do: a save without the version it read, a delete without the typed
// confirmation, a conflict action wired to the wrong route, or a fact body
// painted in as HTML would each ship as a page that looks right and is
// not. The pure decisions (which sibling is a copy of which fact, how many
// bytes a body is, what a merge draft looks like) live in
// memory_conflicts.js and run under node in web/_tests.

// TestMemorySaveCarriesExpectedVersion: the editor puts back what it read
// with the version it read, and a 409 becomes the "changed elsewhere" line
// with a reload — never a silent overwrite and never a retry without the
// version.
func TestMemorySaveCarriesExpectedVersion(t *testing.T) {
	src := webSource(t, "web/memory_panel.js")
	body := funcBody(t, src, "save")
	for _, want := range []string{"api.put(", "factURL(", "expected_version: ", "err.status === 409", "t('memory.changed_elsewhere')", "t('memory.reload')"} {
		if !strings.Contains(body, want) {
			t.Errorf("the editor's save lacks %s", want)
		}
	}
	if !strings.Contains(src, "export function factURL(") || !strings.Contains(src, "'/memory/' + encodeURIComponent(agent) + '/' + encodeURIComponent(name)") {
		t.Error("memory_panel.js does not build the fact URL from the encoded agent and name")
	}
	// A 409 is answered with a reload of the fact, not a second put.
	reload := funcBody(t, src, "reload")
	if !strings.Contains(reload, "api.get(") || strings.Contains(reload, "api.put(") {
		t.Error("reload must re-read the fact and never write")
	}
	if !strings.Contains(src, "t('memory.reload.discards')") {
		t.Error("the reload offer must say that the draft is discarded")
	}
}

// TestMemoryDeleteConfirms: deleting a fact goes through the typed
// confirmation with the fact's name, and the request carries confirm:true
// in a JSON body, which is what the route decodes.
func TestMemoryDeleteConfirms(t *testing.T) {
	src := webSource(t, "web/memory_panel.js")
	body := funcBody(t, src, "remove")
	for _, want := range []string{"confirmDelete({", "confirmToken: name", "api.del(", "factURL(", "{ confirm: true }"} {
		if !strings.Contains(body, want) {
			t.Errorf("the editor's delete lacks %s", want)
		}
	}
	api := webSource(t, "web/api.js")
	if !strings.Contains(api, "del: (p, b) => request('DELETE', p, b)") {
		t.Error("api.del must carry a JSON body, or the confirmation never reaches the route")
	}
}

// TestConflictActionsHitTheirRoutes: the three actions of the conflict
// overlay each reach their own route. Keeping the fact deletes the copy
// through /fs/delete after a typed confirmation; using the copy puts its
// body with the version read and only then deletes the copy; merging by
// hand opens the editor with both bodies and writes nothing.
func TestConflictActionsHitTheirRoutes(t *testing.T) {
	src := webSource(t, "web/memory_panel.js")

	keep := funcBody(t, src, "keepMine")
	for _, want := range []string{"confirmDelete({", "api.post('/fs/delete', { path: copy", "confirm: true"} {
		if !strings.Contains(keep, want) {
			t.Errorf("keepMine lacks %s", want)
		}
	}
	if strings.Contains(keep, "api.put(") {
		t.Error("keepMine must not write the fact")
	}

	use := funcBody(t, src, "useCopy")
	put := strings.Index(use, "api.put(")
	del := strings.Index(use, "api.post('/fs/delete'")
	if put < 0 || del < 0 {
		t.Fatal("useCopy must put the fact and delete the copy")
	}
	if put > del {
		t.Error("useCopy must write the fact before it deletes the copy")
	}
	if !strings.Contains(use[:del], "expected_version: ") || !strings.Contains(use[del:], "confirm: true") {
		t.Error("useCopy must carry the read version on the put and confirm:true on the delete")
	}

	merge := funcBody(t, src, "mergeByHand")
	if !strings.Contains(merge, "openMemoryEditor(") || !strings.Contains(merge, "draft: mergeDraft(") {
		t.Error("mergeByHand must open the editor prefilled with the merge draft")
	}
	for _, forbidden := range []string{"api.put(", "api.post(", "api.del("} {
		if strings.Contains(merge, forbidden) {
			t.Errorf("mergeByHand writes through %s; it must only open the editor", forbidden)
		}
	}
	// The copy is read through the file preview, which is the only route
	// that can read a name outside the fact grammar.
	if !strings.Contains(src, "api.get('/fs/preview?path=' + encodeURIComponent(copy)") {
		t.Error("the conflict overlay must read the copy through /fs/preview")
	}
}

// TestMemoryBodyIsInsertedAsText: a fact body, a description and a copy's
// bytes come from files the agent wrote; they go into the page as text,
// never as HTML.
func TestMemoryBodyIsInsertedAsText(t *testing.T) {
	for _, name := range []string{"web/memory_panel.js", "web/screens/agents_memory.js", "web/memory_conflicts.js"} {
		src := webSource(t, name)
		if strings.Contains(src, "html:") || strings.Contains(src, "innerHTML") {
			t.Errorf("%s inserts HTML; every string from a fact must be a text node", name)
		}
	}
	panel := webSource(t, "web/memory_panel.js")
	if !strings.Contains(panel, "el('textarea'") || !strings.Contains(panel, "body.value = ") {
		t.Error("the editor must fill its textarea through .value")
	}
	if !strings.Contains(panel, "el('pre'") {
		t.Error("the conflict overlay must show both bodies in <pre> text nodes")
	}
}

// TestMemoryTabExplainsMissingRoot: enabled:false renders the explanation
// for the reason the daemon gave and the configuration example as text,
// and asks for no agent listing.
func TestMemoryTabExplainsMissingRoot(t *testing.T) {
	src := webSource(t, "web/screens/agents_memory.js")
	body := funcBody(t, src, "renderDisabled")
	for _, want := range []string{"reason === 'no_root'", "t('memory.disabled.no_root')", "t('memory.disabled.unavailable')", "el('pre'", "r.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderDisabled lacks %s", want)
		}
	}
	if strings.Contains(body, "api.") {
		t.Error("the disabled state must make no request")
	}
	if !strings.Contains(src, "if (!r.enabled)") || !strings.Contains(src, "api.get('/memory/agents')") {
		t.Error("the tab must decide on GET /memory/agents enabled before it lists anything")
	}
	agents := webSource(t, "web/screens/agents.js")
	if !strings.Contains(agents, "memory: renderMemoryTab,") || strings.Contains(agents, "renderPlaceholder") {
		t.Error("agents.js must mount the memory tab in place of the placeholder")
	}
}

// TestMemoryNameValidatedInTheForm: the new-memory form checks the agent
// and the name against the same grammar the store enforces, so the mistake
// lands next to the field rather than in a toast.
func TestMemoryNameValidatedInTheForm(t *testing.T) {
	tab := webSource(t, "web/screens/agents_memory.js")
	form := funcBody(t, tab, "createMemory")
	for _, want := range []string{"openForm({", "validate: ", "isValidName(name.value", "isValidName(agent.value", "t('memory.err.name')", "api.put("} {
		if !strings.Contains(form, want) {
			t.Errorf("createMemory lacks %s", want)
		}
	}
	// The grammar in the browser module is the store's, character for
	// character.
	view := webSource(t, "web/memory_conflicts.js")
	if strings.Contains(view, "import ") || strings.Contains(view, "document.") {
		t.Error("memory_conflicts.js must have no imports and touch no DOM, so node can run it")
	}
	m := regexp.MustCompile(`NAME_RE = /(\^[^/]+\$)/`).FindStringSubmatch(view)
	if m == nil {
		t.Fatal("memory_conflicts.js has no NAME_RE literal")
	}
	re := regexp.MustCompile(m[1])
	for _, s := range []string{"style", "a", "a-b-1", "-a", "A", "a b", "", strings.Repeat("a", 64), strings.Repeat("a", 65), "a..b", "a/b"} {
		if re.MatchString(s) != memory.ValidName(s) {
			t.Errorf("NAME_RE and memory.ValidName disagree on %q", s)
		}
	}
}

func TestMemoryCatalogCoversTheTab(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{
		"agents.tab.memory", "memory.conflict", "memory.changed_elsewhere", "memory.reload", "memory.reload.discards",
		"memory.disabled.no_root", "memory.disabled.unavailable", "memory.err.name", "memory.keep_mine", "memory.use_copy", "memory.merge",
	} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

func TestMemoryModulesStayShort(t *testing.T) {
	for _, name := range []string{"web/memory_conflicts.js", "web/memory_panel.js", "web/screens/agents_memory.js", "web/screens/agents.js"} {
		if n := strings.Count(webSource(t, name), "\n"); n >= 800 {
			t.Errorf("%s is %d lines; split it", name, n)
		}
	}
}
