package control

import (
	"strings"
	"testing"
)

// The folder glyph at the start of the breadcrumb represents the virtual
// filesystem root. It used to be a decorative SVG, so only named breadcrumb
// segments were navigable and there was no way back to / in one click.
func TestBreadcrumbRootIconNavigatesToRoot(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{
		"title: t('nav.root'), 'aria-label': t('nav.root')",
		"onclick: () => { cwd = '/'; load(); }",
		"}, iconEl('folder'))",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("breadcrumb root is missing %q", want)
		}
	}
	for _, file := range []string{"web/i18n_en.js", "web/i18n_zh.js"} {
		if src := webSource(t, file); !strings.Contains(src, "'nav.root':") {
			t.Fatalf("%s has no root-folder label", file)
		}
	}
}
