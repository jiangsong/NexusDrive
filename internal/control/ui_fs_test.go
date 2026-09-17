package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The render page (ui-plan G8): #/fs/<path> is routed from the hash with
// the path decoded per segment and is not a nav item; the screen reads
// GET /fs/render and sets HTML only from the daemon's rendered Markdown
// (renderMarkdown escapes every character of the source), shows text as
// text, images and PDFs from /fs/raw; creating a public link takes the
// typed file name and posts to /share with confirm, and a credential
// refusal is offered a second, typed confirmation before force; the
// inspector links to the page; the settings screen shows the share
// section.
func TestFsPageRendersSafelyAndConfirmsShares(t *testing.T) {
	router := webSource(t, "web/router.js")
	for _, want := range []string{"if (base === '#/fs' || base.startsWith('#/fs/')) return 'fs-view'", "export function fsPathFromHash(hash)", "decodeURIComponent(s)"} {
		if !strings.Contains(router, want) {
			t.Errorf("router.js lacks %s", want)
		}
	}
	if strings.Contains(router, "hash: '#/fs'") {
		t.Error("the render page is a nav item")
	}
	if app := webSource(t, "web/app.js"); !strings.Contains(app, "'fs-view': renderFs") {
		t.Error("the shell never mounts the render page")
	}
	screen := webSource(t, "web/screens/fs.js")
	for _, want := range []string{
		"api.get('/fs/render?path=' + encodeURIComponent(path))", "doc.innerHTML = r.html || ''", "'data-rendered-markdown'", "'data-rendered-text': r.kind",
		"el('img', { src: r.raw", "el('iframe', { src: r.raw", "sandbox: ''",
		"confirmToken: name", "api.post('/share', { path, confirm: true })", "api.post('/share', { path, confirm: true, force: true })", "shareRefusal(err)",
		"api.post('/share/render-link', { path })", "mountProvenance(prov, path,", "openHistoryPanel(path,", "copyBtn(r.console_url)", "disabled: !r.share",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("fs.js lacks %s", want)
		}
	}
	if n := strings.Count(screen, "innerHTML"); n != 1 {
		t.Errorf("fs.js sets innerHTML %d times; only the daemon's rendered Markdown may be set as HTML", n)
	}
	share := funcBody(t, screen, "share")
	if strings.Count(share, "confirmDelete({") != 2 || strings.Index(share, "confirmDelete({") > strings.Index(share, "api.post('/share'") {
		t.Errorf("share must confirm before posting, and again before forcing:\n%s", share)
	}
	view := webSource(t, "web/fs_view.js")
	if strings.Contains(view, "import ") || !strings.Contains(view, "export function shareRefusal(err)") {
		t.Error("fs_view.js must be a pure module exporting shareRefusal")
	}
	if _, err := os.Stat(filepath.Join("web", "_tests", "fs_view.test.mjs")); err != nil {
		t.Fatalf("the node suite for fs_view.js is missing: %v", err)
	}
	main := webSource(t, "web/screens/main.js")
	if !strings.Contains(main, "'data-open-page': e.path") || !strings.Contains(main, "href: '#/fs/' + e.path.split('/').filter(Boolean).map(encodeURIComponent).join('/')") {
		t.Error("the inspector does not link to the render page")
	}
	settings := webSource(t, "web/settings_view.js")
	if !strings.Contains(settings, "id: 'share'") || !strings.Contains(settings, "'settings.share.render.enabled'") {
		t.Error("the settings screen has no share section")
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"fs.eyebrow", "fs.none", "fs.link", "fs.share.create", "fs.share.title", "fs.share.body", "fs.share.done", "fs.share.unsupported", "fs.share.not_synced", "fs.share.not_cached", "fs.share.credentials", "fs.share.force", "fs.lan.create", "fs.lan.once", "fs.other", "action.openpage", "settings.section.share", "settings.share.console_links", "settings.share.render.enabled"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
	// The render token never enters the browser store.
	store := webSource(t, "web/store.js")
	if strings.Contains(store, "render_token") || strings.Contains(screen, "localStorage") {
		t.Error("the render token is kept in the browser")
	}
}
