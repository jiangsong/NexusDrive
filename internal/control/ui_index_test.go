package control

import (
	"strings"
	"testing"
)

// The index screen is the console's view of the content index. These tests
// read the embedded sources: a screen the router does not know, a disabled
// index that still asks for rules the daemon refuses, or a destructive
// action sent without confirm: true would each ship as a blank or broken
// page with nothing else to fail.
func TestIndexScreenIsRoutedAndInTheNav(t *testing.T) {
	if _, err := webFS.ReadFile("web/screens/index.js"); err != nil {
		t.Fatalf("there is no index screen: %v", err)
	}
	router := webSource(t, "web/router.js")
	for _, want := range []string{"'#/index': 'index-view'", "hash: '#/index', icon: 'layers', key: 'nav.index'"} {
		if !strings.Contains(router, want) {
			t.Errorf("router.js lacks %s", want)
		}
	}
	// The nav item sits right after the agents item.
	agents := strings.Index(router, "hash: '#/agents'")
	index := strings.Index(router, "hash: '#/index'")
	storage := strings.Index(router, "hash: '#/storage'")
	if agents < 0 || index < 0 || storage < 0 || agents > index || index > storage {
		t.Errorf("the index nav item must come after agents and before storage (agents=%d index=%d storage=%d)", agents, index, storage)
	}
	app := webSource(t, "web/app.js")
	if !strings.Contains(app, "renderIndex") || !strings.Contains(app, "'index-view': renderIndex") {
		t.Error("the shell never mounts the index screen")
	}
	if !strings.Contains(app, "export function onIndexChange(") || !strings.Contains(app, "onIndex:") {
		t.Error("the shell does not relay index progress events to the screen")
	}
	if !strings.Contains(webSource(t, "web/api.js"), "es.addEventListener('index'") {
		t.Error("api.js never subscribes to the index SSE event")
	}
}

// A disabled index renders an explanation, and must not go on to ask for
// rules the daemon will refuse.
func TestIndexScreenSkipsRulesWhenDisabled(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	gate := strings.Index(src, "if (!st.enabled)")
	rules := strings.Index(src, "api.get('/index/rules'")
	if gate < 0 || rules < 0 || gate > rules {
		t.Fatalf("the enabled check (%d) must come before the rules request (%d)", gate, rules)
	}
	if !strings.Contains(src[gate:rules], "return") {
		t.Error("the disabled branch does not return before loading rules")
	}
	for _, want := range []string{"t('index.disabled.body')", "DISABLED_EXAMPLE", "api.get('/index/status')"} {
		if !strings.Contains(src, want) {
			t.Errorf("index screen lacks %s", want)
		}
	}
}

func TestIndexMutationsCarryConfirm(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{
		"api.post('/index/remove', { path: rule.path, confirm: true })",
		"api.post('/index/rebuild', { confirm: true })",
		"confirmToken: rule.path",
		"confirmToken: 'rebuild'",
		"api.post('/index/add'",
		"api.post('/index/retry'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index screen lacks %s", want)
		}
	}
	// Both destructive actions go through the typed confirmation sheet, and
	// only after it resolved true.
	for _, action := range []string{"'/index/remove'", "'/index/rebuild'"} {
		post := strings.Index(src, "api.post("+action)
		confirm := strings.LastIndex(src[:post], "confirmDelete(")
		if post < 0 || confirm < 0 {
			t.Fatalf("%s is not preceded by confirmDelete", action)
		}
		if !strings.Contains(src[confirm:post], "if (!") {
			t.Errorf("%s does not stop when the confirmation sheet is dismissed", action)
		}
	}
}

func TestConfigRulesHaveNoRemoveButton(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	if !strings.Contains(src, "rule.source === 'config'") || !strings.Contains(src, "t('index.rule.inconfig')") {
		t.Error("configuration rules must render the in-config note instead of a remove action")
	}
	if !strings.Contains(src, "t('index.rule.remove')") {
		t.Error("console rules have no remove action")
	}
}

func TestIndexScreenPagesFailedDocsAndFollowsProgress(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{
		"api.get('/index/failed?", "next_cursor", "moreRow(", "pageFailureMode(", "pageCursor(",
		"onIndexChange(", "t('index.paused.' + ", "resume_at",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index screen lacks %s", want)
		}
	}
}

// The add-rule form offers the presets by name, parses the size, and keeps
// the download warning in view: a rule is the one console action that
// makes the daemon fetch files it was not asked to read.
func TestIndexAddRuleFormUsesPresetsAndWarns(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{"includeFor(", "parseSize(", "t('index.rule.note')", "t('index.rule.badsize')", "openForm("} {
		if !strings.Contains(src, want) {
			t.Errorf("index screen lacks %s", want)
		}
	}
	presets := webSource(t, "web/index_presets.js")
	if strings.Contains(presets, "import ") {
		t.Error("index_presets.js must stay import-free so node can test it directly")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, p := range []string{"docs", "code", "text"} {
		if !zh["index.preset."+p] {
			t.Errorf("missing index.preset.%s", p)
		}
	}
}

// Pause reasons and rule sources come from the daemon, so their keys are
// assembled at runtime and the literal t('...') scan cannot see them.
func TestWebCatalogCoversIndexPauseReasons(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, r := range []string{"busy", "risk_control", "budget", "text_budget"} {
		if !zh["index.paused."+r] {
			t.Errorf("missing index.paused.%s", r)
		}
	}
	for _, s := range []string{"config", "ui", "tool"} {
		if !zh["index.rule.source."+s] {
			t.Errorf("missing index.rule.source.%s", s)
		}
	}
}
