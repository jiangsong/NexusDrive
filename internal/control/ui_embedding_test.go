package control

import (
	"regexp"
	"strings"
	"testing"
)

// The embedding panel is the console's view of the endpoint that chunk
// text is sent to, and the semantic search mode is that endpoint seen from
// the search box. These tests read the shipped sources, the way the other
// ui_*_test.go files do: a remote banner with a close button, a key typed
// into the page, a "test endpoint" that spends a request on mount, or a
// degraded answer shown as a full semantic one would each ship as a page
// that looks right and is not. The pure decisions (is there a banner, what
// the estimate says) live in embedding_view.js and are exercised under node
// by web/_tests/embedding_view.test.mjs.

// funcBody cuts the body of `function name(` out of a module source, up to
// the next top-level `function ` or `export `, so a test can look at what
// one builder renders and nothing else.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "function "+name+"(")
	if i < 0 {
		t.Fatalf("no function %s", name)
	}
	rest := src[i+len("function "+name+"("):]
	end := len(rest)
	for _, stop := range []string{"\nfunction ", "\nexport ", "\n  function ", "\n  async function "} {
		if j := strings.Index(rest, stop); j >= 0 && j < end {
			end = j
		}
	}
	return rest[:end]
}

// TestRemoteBannerHasNoCloseButton: the banner is produced from remote ===
// true and from nothing else, it names the host, and the element that
// builds it holds no control that could hide it. The other half — that
// remote:false yields no banner and remote:true yields one — is the
// bannerFor() case in embedding_view.test.mjs, run by
// TestBrowserModuleBehaviour.
func TestRemoteBannerHasNoCloseButton(t *testing.T) {
	view := webSource(t, "web/embedding_view.js")
	if !strings.Contains(view, "export function bannerFor(") || !strings.Contains(view, "remote === true") {
		t.Fatal("embedding_view.js does not decide the banner from remote === true")
	}
	if strings.Contains(view, "import ") || strings.Contains(view, "document.") {
		t.Error("embedding_view.js must have no imports and touch no DOM, so node can run it")
	}
	panel := webSource(t, "web/embedding_panel.js")
	body := funcBody(t, panel, "remoteBanner")
	for _, want := range []string{"class: 'banner warn'", "role: 'alert'", "t('embedding.banner', "} {
		if !strings.Contains(body, want) {
			t.Errorf("remoteBanner lacks %s", want)
		}
	}
	for _, forbidden := range []string{"button", "close", "dismiss", "hidden", "remove()", "onclick"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("remoteBanner contains %q: the banner must have no control that hides it", forbidden)
		}
	}
	// The banner is rendered from bannerFor and nowhere else: no second
	// condition in the panel could show it for a local endpoint or hide it
	// for a remote one.
	if !strings.Contains(panel, "bannerFor(st)") || strings.Count(panel, "remoteBanner(") != 2 {
		t.Error("the panel must render the banner exactly where bannerFor(st) says so")
	}
	if strings.Contains(panel, ".remote") {
		t.Error("the panel reads remote itself instead of asking bannerFor")
	}
	if !strings.Contains(webSource(t, "web/app.css"), ".banner.warn") {
		t.Error("app.css has no warn banner style")
	}
}

// TestEmbeddingPanelHasNoKeyInput: the key is set with `cloudfs index auth`
// in a terminal. The panel shows that command with a copy button and holds
// no input element at all — not a text box, not a form — and never names
// the secret field or the header it travels in. The one occurrence of
// "api_key" allowed is the boolean api_key_configured the daemon serves.
func TestEmbeddingPanelHasNoKeyInput(t *testing.T) {
	for _, name := range []string{"web/embedding_panel.js", "web/embedding_view.js"} {
		src := webSource(t, name)
		if strings.Contains(src, "el('input'") || strings.Contains(src, "createElement('input')") || strings.Contains(src, "'textarea'") || strings.Contains(src, "openForm(") || strings.Contains(src, "promptText(") {
			t.Errorf("%s renders an input; the key is set in the terminal", name)
		}
		stripped := strings.ReplaceAll(src, "api_key_configured", "")
		for _, secret := range []string{"api_key", "apiKey", "Authorization", "Bearer", "secret", "password", "access_token", "api_token"} {
			if strings.Contains(stripped, secret) {
				t.Errorf("%s names a credential field: %q", name, secret)
			}
		}
	}
	panel := webSource(t, "web/embedding_panel.js")
	for _, want := range []string{"api_key_configured === false", "copyBtn(", "AUTH_COMMAND", "t('embedding.nokey')", "t('embedding.footer')"} {
		if !strings.Contains(panel, want) {
			t.Errorf("embedding_panel.js lacks %s", want)
		}
	}
	if !strings.Contains(webSource(t, "web/embedding_view.js"), "'cloudfs index auth'") {
		t.Error("the auth command shown is not cloudfs index auth")
	}
}

// TestEndpointCheckOnlyOnClick: mounting the panel asks GET /index/embedding
// and nothing else; the one POST /index/embedding/check is inside the click
// handler of the "test endpoint" button, after the line that says the
// click makes a request.
func TestEndpointCheckOnlyOnClick(t *testing.T) {
	panel := webSource(t, "web/embedding_panel.js")
	if strings.Count(panel, "api.get('/index/embedding')") != 1 {
		t.Fatal("the panel must load GET /index/embedding exactly once per render")
	}
	const check = "api.post('/index/embedding/check'"
	if strings.Count(panel, check) != 1 {
		t.Fatalf("expected exactly one %s, found %d", check, strings.Count(panel, check))
	}
	// The request lives in checkEndpoint, and checkEndpoint is reached
	// only from an onclick.
	body := funcBody(t, panel, "checkEndpoint")
	if !strings.Contains(body, check) {
		t.Error("the check request is not inside checkEndpoint")
	}
	calls := regexp.MustCompile(`checkEndpoint\(`).FindAllStringIndex(panel, -1)
	if len(calls) < 2 {
		t.Fatal("checkEndpoint is never called")
	}
	for _, c := range calls[1:] {
		before := panel[max(0, c[0]-80):c[0]]
		if !strings.Contains(before, "onclick") {
			t.Errorf("checkEndpoint is called outside a click handler: %q", panel[max(0, c[0]-80):c[1]])
		}
	}
	// The mount path does not reach it.
	mount := funcBody(t, panel, "mountEmbeddingPanel")
	load := strings.Index(mount, "api.get('/index/embedding')")
	if load < 0 {
		t.Fatal("mountEmbeddingPanel does not load the status")
	}
	// The explanation stands before the button, and the result lands next
	// to it rather than in a toast.
	explain, button := strings.Index(panel, "t('embedding.check.explain')"), strings.Index(panel, "t('embedding.check')")
	if explain < 0 || button < 0 || explain > button {
		t.Error("the explanation that the check spends a request must come before the button")
	}
	for _, want := range []string{"t('embedding.check.ok', ", "t('embedding.check.failed', "} {
		if !strings.Contains(body, want) {
			t.Errorf("checkEndpoint lacks %s", want)
		}
	}
	if strings.Contains(body, "toast(t('embedding.check") {
		t.Error("the check result must stay next to the button, not fade out of a toast")
	}
}

// TestSemanticModeShowsDegradedNote: the search box offers a "semantic"
// segment next to "keyword", the semantic one asks /index/search with
// mode=hybrid and the keyword one with mode=keyword, and an answer whose
// degraded field is set renders the reason above the rows, as text.
func TestSemanticModeShowsDegradedNote(t *testing.T) {
	main := webSource(t, "web/screens/main.js")
	for _, want := range []string{"t('search.mode.content')", "t('search.mode.semantic')", "'semantic'", "'content'"} {
		if !strings.Contains(main, want) {
			t.Errorf("main.js lacks %s", want)
		}
	}
	i := strings.Index(main, "t('search.mode.semantic')")
	if i < 0 || !strings.Contains(main[max(0, i-400):i+200], "indexEnabled()") {
		t.Error("the semantic segment is offered without checking index.enabled")
	}
	cs := webSource(t, "web/content_search.js")
	for _, want := range []string{
		"searchModeParam(mode)", "'&mode=' + encodeURIComponent(",
		"degradedReason(r)", "t('search.degraded', ",
	} {
		if !strings.Contains(cs, want) {
			t.Errorf("content_search.js lacks %s", want)
		}
	}
	// Above the rows: the note is appended before the hits.
	note, hits := strings.Index(cs, "t('search.degraded', "), strings.Index(cs, "hits.map((hit) => resultRow(")
	if note < 0 || hits < 0 || note > hits {
		t.Error("the degraded note must be rendered above the result rows")
	}
	if strings.Contains(cs, "toast(t('search.degraded'") {
		t.Error("the degraded note must stay with the rows, not fade out of a toast")
	}
	// The reason is daemon text and goes in as a text node.
	if strings.Contains(cs, "html:") {
		t.Error("content_search.js paints daemon text through innerHTML")
	}
	view := webSource(t, "web/embedding_view.js")
	for _, want := range []string{"export function searchModeParam(", "'hybrid'", "'keyword'", "export function degradedReason("} {
		if !strings.Contains(view, want) {
			t.Errorf("embedding_view.js lacks %s", want)
		}
	}
	// The stored mode survives an unknown value and a denied storage, for
	// both extra modes.
	if !strings.Contains(cs, "SEARCH_MODES.includes(") {
		t.Error("readSearchMode does not accept the semantic mode")
	}
}

// TestIndexOverviewShowsVectors: the index screen mounts the panel and
// its overview reads vectors and max_chunks from both status documents.
func TestIndexOverviewShowsVectors(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{
		"mountEmbeddingPanel(", "import { mountEmbeddingPanel } from '/ui/embedding_panel.js'",
		"vectors: st.vectors || 0", "maxChunks: st.max_chunks || 0",
		"t('index.card.vectors', f.vectors, f.maxChunks)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index screen lacks %s", want)
		}
	}
	// The panel is torn down with the screen.
	if !strings.Contains(src, "panel.dispose()") && !strings.Contains(src, "stops.push(") {
		t.Error("the panel is never disposed")
	}
	if n := strings.Count(src, "\n"); n >= 800 {
		t.Errorf("screens/index.js has %d lines; keep it under 800 by putting the panel in its own module", n)
	}
	if n := strings.Count(webSource(t, "web/embedding_panel.js"), "\n"); n >= 800 {
		t.Errorf("embedding_panel.js has %d lines", n)
	}
	// Everything the panel and the search box call is a registered route.
	s := &Server{}
	registered := map[string]bool{}
	for _, r := range s.routes() {
		registered[r.pattern] = true
	}
	for _, want := range []string{"/index/embedding", "/index/embedding/check", "/index/search"} {
		if !registered[want] {
			t.Errorf("the page calls %s, which routes() does not register", want)
		}
	}
}

// TestEmbeddingCatalogCoversThePanel: every embedding.* and search key the
// panel and the search box use is in both tables (the generic key test
// covers literal t() calls; this pins the ones the plan names).
func TestEmbeddingCatalogCoversThePanel(t *testing.T) {
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, want := range []string{
			"embedding.title", "embedding.disabled", "embedding.banner", "embedding.estimate.note",
			"embedding.check", "embedding.check.explain", "embedding.nokey", "embedding.footer",
			"search.mode.semantic", "search.degraded", "index.card.vectors",
		} {
			if !keys[want] {
				t.Errorf("%s table lacks %s", lang, want)
			}
		}
	}
}
