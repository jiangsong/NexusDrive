package control

import (
	"os"
	"path/filepath"
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
	// The agent key may be owner/agent (layout v2): each segment is
	// encoded on its own, so the slash stays a path separator.
	if !strings.Contains(src, "export function factURL(") || !strings.Contains(src, "String(agent).split('/').map(encodeURIComponent).join('/') + '/' + encodeURIComponent(name)") {
		t.Error("memory_panel.js does not build the fact URL from the encoded agent segments and name")
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

// TestMemoryTabGroupsByOwnerAndMigrates (ui-plan G9): in layout v2 the
// agent list groups by owner through groupByOwner (a module with no DOM,
// under node), the v1 list carries the "move to v2" button whose typed
// confirmation is the word migrate and whose only write is POST
// /memory/migrate with confirm; the conflict overlay asks GET
// /memory/merge for a proposal, draws it through mergeRows, and adopts it
// with a put carrying both versions before deleting the copy.
func TestMemoryTabGroupsByOwnerAndMigrates(t *testing.T) {
	view := webSource(t, "web/memory_layout_view.js")
	if strings.Contains(view, "import ") {
		t.Error("memory_layout_view.js imports; it must run under node with no DOM")
	}
	for _, want := range []string{"export function groupByOwner(agents, layout, me)", "export function mergeRows(merged)", "export function adoptable(res)", "line.startsWith('<<<<<<<')", "line === '======='"} {
		if !strings.Contains(view, want) {
			t.Errorf("memory_layout_view.js lacks %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join("web", "_tests", "memory_layout_view.test.mjs")); err != nil {
		t.Fatalf("the node suite for memory_layout_view.js is missing: %v", err)
	}
	tab := webSource(t, "web/screens/agents_memory.js")
	for _, want := range []string{"import { groupByOwner } from '/ui/memory_layout_view.js'", "groupByOwner(agents, layout, owner)", "'data-owner-group': g.owner", "t('memory.owner.me', g.owner)", "t('memory.layout', layout)", "'data-migrate': ''", "confirmToken: 'migrate'", "api.post('/memory/migrate', { confirm: true })", "layout = r.layout === 'v2' ? 'v2' : 'v1'"} {
		if !strings.Contains(tab, want) {
			t.Errorf("agents_memory.js lacks %s", want)
		}
	}
	migrate := funcBody(t, tab, "migrate")
	if !strings.Contains(migrate, "confirmDelete({") || strings.Count(migrate, "api.post(") != 1 {
		t.Errorf("migrate must confirm once and post once:\n%s", migrate)
	}
	panel := webSource(t, "web/memory_panel.js")
	for _, want := range []string{"import { mergeRows, adoptable } from '/ui/memory_layout_view.js'", "api.get('/memory/merge?agent=' + encodeURIComponent(agent)", "mergeRows(res.merged).map(", "'data-conflict-block'", "adoptBtn.disabled = !adoptable(res)", "expected_remote_version: proposed.remote_version || ''", "expected_version: proposed.version || ''", "'data-adopt-merge'", "'data-propose'"} {
		if !strings.Contains(panel, want) {
			t.Errorf("memory_panel.js lacks %s", want)
		}
	}
	adopt := funcBody(t, panel, "adopt")
	if !strings.Contains(adopt, "if (!adoptable(proposed)) return") || !strings.Contains(adopt, "confirmDelete({") || strings.Index(adopt, "api.put(") > strings.Index(adopt, "api.post('/fs/delete'") {
		t.Errorf("adopt must refuse an unclean proposal, confirm, put, then delete the copy:\n%s", adopt)
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"memory.layout", "memory.migrate", "memory.migrate.title", "memory.migrate.body", "memory.migrate.done", "memory.owner.me", "memory.owner.shared", "memory.propose", "memory.adopt", "memory.adopt.title", "memory.adopt.body", "memory.proposal.clean", "memory.proposal.conflicts"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}
