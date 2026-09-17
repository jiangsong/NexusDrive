package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings screen (ui-plan G3, read-only by the 2026-09-17 decision):
// routed and in the nav, it reads GET /settings once and shows each
// section as rows with the YAML block to copy; it has no input, no form
// and no write, because configuration is defined in the file only. The
// sections and the YAML come from settings_view.js, which runs under
// node.
func TestSettingsScreenIsReadOnly(t *testing.T) {
	if _, err := webFS.ReadFile("web/screens/settings.js"); err != nil {
		t.Fatalf("there is no settings screen: %v", err)
	}
	router := webSource(t, "web/router.js")
	for _, want := range []string{"'#/settings': 'settings-view'", "hash: '#/settings', icon: 'wrench', key: 'nav.settings'"} {
		if !strings.Contains(router, want) {
			t.Errorf("router.js lacks %s", want)
		}
	}
	if app := webSource(t, "web/app.js"); !strings.Contains(app, "renderSettings") || !strings.Contains(app, "'settings-view': renderSettings") {
		t.Error("the shell never mounts the settings screen")
	}
	screen := webSource(t, "web/screens/settings.js")
	for _, want := range []string{"api.get('/settings')", "import { sections } from '/ui/settings_view.js'", "sections(v).map(section)", "copyBtn(s.yaml)", "'data-section': s.id", "'data-setting': r.key", "t('settings.readonly_note')", "t(r.hint.key, r.hint.arg)", "t('settings.builtin')"} {
		if !strings.Contains(screen, want) {
			t.Errorf("settings.js lacks %s", want)
		}
	}
	for _, bad := range []string{"api.post(", "api.put(", "api.patch(", "api.del(", "el('input'", "el('select'", "el('form'", "el('textarea'", "html:"} {
		if strings.Contains(screen, bad) {
			t.Errorf("settings.js has %s; the screen is read-only", bad)
		}
	}
	view := webSource(t, "web/settings_view.js")
	if strings.Contains(view, "import ") {
		t.Error("settings_view.js imports; it must run under node with no DOM")
	}
	for _, want := range []string{"export function sections(v)", "export function yamlBlock(root, obj)", "export function readTextTokens(maxTokens)", "'settings.mcp.max_tokens.hint'", "builtin: true"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings_view.js lacks %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join("web", "_tests", "settings_view.test.mjs")); err != nil {
		t.Fatalf("the node suite for settings_view.js is missing: %v", err)
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"nav.settings", "settings.title", "settings.readonly_note", "settings.builtin", "settings.mcp.max_tokens", "settings.mcp.max_tokens.hint", "settings.mcp.max_tokens.off", "settings.mcp.install_transport", "settings.session.retain_blobs", "settings.session.preimage_files", "settings.hooks.context", "settings.hooks.changed_max", "settings.hooks.memory_head_lines", "settings.heat.enabled", "settings.memory.root", "settings.index.enabled"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
		for _, id := range []string{"mcp", "session", "hooks", "heat", "memory", "index"} {
			if !keys["settings.section."+id] {
				t.Errorf("%s lacks settings.section.%s", lang, id)
			}
		}
	}
}
