package control

import (
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

func mustAsset(t *testing.T, path string) []byte {
	t.Helper()
	b, err := webFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
