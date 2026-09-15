package control

import (
	"bytes"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// iconRule extracts the body of a CSS rule for one selector.
func iconRule(t *testing.T, css, selector string) string {
	t.Helper()
	i := strings.Index(css, selector+" {")
	if i < 0 {
		t.Fatalf("app.css has no rule for %q; an icon with no size draws nothing", selector)
	}
	body := css[i+len(selector)+2:]
	j := strings.Index(body, "}")
	if j < 0 {
		t.Fatalf("unterminated rule for %q", selector)
	}
	return body[:j]
}

// TestIconsAreSizedAndDefined guards two ways an icon disappears without any
// error to show for it. iconEl renders <span class="ico"> around an inline SVG
// that carries a viewBox but no width or height, so unless the stylesheet gives
// it a size the SVG resolves to 100% of a zero-wide box and paints nothing —
// a button holding only an icon then renders as an empty square. And an icon
// named by a screen but missing from icons.js renders as the empty string.
func TestIconsAreSizedAndDefined(t *testing.T) {
	css := string(mustAsset(t, "web/app.css"))
	for _, sel := range []string{".ico", ".ico > svg"} {
		rule := iconRule(t, css, sel)
		if !strings.Contains(rule, "width:") || !strings.Contains(rule, "height:") {
			t.Errorf("%s must set width and height, got %q", sel, rule)
		}
	}

	icons := string(mustAsset(t, "web/icons.js"))
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-z]+):\s*svg\(`).FindAllStringSubmatch(icons, -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("no icons parsed out of icons.js")
	}
	used := regexp.MustCompile(`iconEl\('([a-z]+)'\)`)
	err := fs.WalkDir(webFS, "web", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		for _, m := range used.FindAllStringSubmatch(string(mustAsset(t, path)), -1) {
			if !defined[m[1]] {
				t.Errorf("%s asks for icon %q, which icons.js does not define", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPhaseTwoSharedHooksExist: the rollback and trigger screens land on
// separate branches, and both need an icon and an SSE hook that live in
// files every branch touches. Adding them once here is what keeps those
// branches from colliding on the same lines of icons.js and api.js.
func TestPhaseTwoSharedHooksExist(t *testing.T) {
	icons := string(mustAsset(t, "web/icons.js"))
	for _, name := range []string{"undo", "bolt"} {
		if !regexp.MustCompile(`(?m)^\s*` + name + `:\s*svg\(`).MatchString(icons) {
			t.Errorf("icons.js does not define %q", name)
		}
	}
	api := string(mustAsset(t, "web/api.js"))
	for _, want := range []string{"onTrigger", "es.addEventListener('trigger'"} {
		if !strings.Contains(api, want) {
			t.Errorf("api.js does not wire %q", want)
		}
	}
}

// TestScreensDoNotStringifyASkippedChild: the screens build rows with
// conditional children (`cond ? row : null`). node.replaceChildren stringifies
// a null child into the text "null" instead of skipping it, so a row that was
// deliberately not rendered shows up in the panel as the word null — twice over
// where two rows are skipped. ui.js owns the one call that is allowed, wrapped
// in fill(); every screen goes through that.
func TestScreensDoNotStringifyASkippedChild(t *testing.T) {
	err := fs.WalkDir(webFS, "web", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") || path == "web/ui.js" {
			return err
		}
		if bytes.Contains(mustAsset(t, path), []byte(".replaceChildren(")) {
			t.Errorf("%s calls replaceChildren directly; use fill() so a null child is skipped, not printed", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper itself skips null and false, the way el() does.
	ui := string(mustAsset(t, "web/ui.js"))
	if !strings.Contains(ui, "export function fill(") || !strings.Contains(ui, "if (c == null || c === false) continue;") {
		t.Error("ui.js must export fill() and skip null/false children")
	}
	// el() builds a fresh node and must only append to it. Clearing there would
	// also throw away what the `html` attribute set a few lines earlier, which
	// is every icon in the app.
	body := ui[strings.Index(ui, "export function el("):]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "replaceChildren") || strings.Contains(body, "fill(") {
		t.Error("el() must append its children, not clear the node it just filled from the html attribute")
	}
}

func mustAsset(t *testing.T, path string) []byte {
	t.Helper()
	b, err := webFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
