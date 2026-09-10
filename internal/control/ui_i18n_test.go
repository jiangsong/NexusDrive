package control

import (
	"regexp"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
)

// The page's own catalog is a pair of hand-edited tables in one file. A key
// added to one and forgotten in the other renders as the key itself on the
// other language's screen, which is exactly the kind of gap nobody notices
// until a user in that language reports a screen full of dotted identifiers.
// These tests read the shipped asset, so they fail on the edit that caused it.

var jsKeyRe = regexp.MustCompile(`'([a-z][a-zA-Z0-9._]*)':`)

// tableKeys returns the keys of one `const <name> = { ... };` table in the
// embedded i18n.js.
func tableKeys(t *testing.T, source, name string) map[string]bool {
	t.Helper()
	start := strings.Index(source, "const "+name+" = {")
	if start < 0 {
		t.Fatalf("no %s table in i18n.js", name)
	}
	end := strings.Index(source[start:], "\n};")
	if end < 0 {
		t.Fatalf("%s table is not terminated", name)
	}
	keys := map[string]bool{}
	for _, m := range jsKeyRe.FindAllStringSubmatch(source[start:start+end], -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatalf("%s table has no keys", name)
	}
	return keys
}

// webSource reads one embedded asset. Every UI test needs this and the
// four-line read-and-fatal was being copied into each new one.
func webSource(t *testing.T, name string) string {
	t.Helper()
	b, err := webFS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func webI18nSource(t *testing.T) string { return webSource(t, "web/i18n.js") }

// webScripts lists every embedded .js asset, which is what the tests that
// hold a rule for the whole app walk.
func webScripts(t *testing.T) []string {
	t.Helper()
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			sub, err := webFS.ReadDir("web/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range sub {
				files = append(files, "web/"+e.Name()+"/"+x.Name())
			}
			continue
		}
		files = append(files, "web/"+e.Name())
	}
	var scripts []string
	for _, f := range files {
		if strings.HasSuffix(f, ".js") {
			scripts = append(scripts, f)
		}
	}
	return scripts
}

// TestWebCatalogsHaveTheSameKeys: the English table is a copy-and-translate of
// the Chinese one, and it must stay a complete copy.
func TestWebCatalogsHaveTheSameKeys(t *testing.T) {
	src := webI18nSource(t)
	zhKeys := tableKeys(t, src, "zh")
	enKeys := tableKeys(t, src, "en")
	for k := range zhKeys {
		if !enKeys[k] {
			t.Errorf("key %q is in the zh table but not en", k)
		}
	}
	for k := range enKeys {
		if !zhKeys[k] {
			t.Errorf("key %q is in the en table but not zh", k)
		}
	}
}

// A browser can deny localStorage (privacy settings, embedded contexts). The
// selected language must therefore cross the reload in the URL as well, and
// detection must honor that explicit value before consulting storage.
func TestWebLanguageSwitchSurvivesUnavailableStorage(t *testing.T) {
	src := webI18nSource(t)
	queryRead := "new URLSearchParams(location.search).get('lang')"
	queryWrite := "next.searchParams.set('lang', code)"
	if !strings.Contains(src, queryRead) || !strings.Contains(src, queryWrite) || !strings.Contains(src, "location.replace(next.toString())") {
		t.Fatal("language switch does not preserve the selection in the page URL")
	}
	if strings.Index(src, queryRead) > strings.Index(src, "localStorage.getItem(STORAGE_KEY)") {
		t.Fatal("stored language overrides the explicit URL language")
	}
}

// TestWebScreensHoldNoUntranslatedText: a screen with a Chinese literal in it
// is a string that never reaches the English table. i18n.js is the one file
// allowed to contain them.
func TestWebScreensHoldNoUntranslatedText(t *testing.T) {
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			sub, err := webFS.ReadDir("web/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range sub {
				files = append(files, "web/"+e.Name()+"/"+s.Name())
			}
			continue
		}
		files = append(files, "web/"+e.Name())
	}
	for _, name := range files {
		if name == "web/i18n.js" || !strings.HasSuffix(name, ".js") {
			continue
		}
		b, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range string(b) {
			if r >= 0x4E00 && r <= 0x9FFF {
				t.Errorf("%s contains the untranslated literal %q; route it through t()", name, string(r))
				break
			}
		}
		if strings.ContainsAny(string(b), "，。：；（）【】") {
			t.Errorf("%s contains locale-specific punctuation outside i18n.js", name)
		}
		if strings.Contains(string(b), "'zh-CN'") || strings.Contains(string(b), `"zh-CN"`) {
			t.Errorf("%s hard-codes zh-CN formatting instead of using the selected locale", name)
		}
	}
}

// Upload states come from the daemon, so their keys are assembled at runtime
// and cannot be found by the literal t('...') scan below. Keep every journal
// state visible in both languages instead of leaking the enum to the table.
func TestWebCatalogCoversUploadStates(t *testing.T) {
	known := tableKeys(t, webI18nSource(t), "zh")
	for _, state := range []string{"pending", "uploading", "done", "dead", "cancelling", "cancelled", "purging"} {
		key := "upload.state." + state
		if !known[key] {
			t.Errorf("i18n.js has no translation for upload state %q", state)
		}
	}
}

// Copy states and mount modes are assembled at the call site the same way
// upload states are — t('copy.state.' + c.state) — so the literal scan below
// cannot see them. Each enum is closed and lives in Go, so the catalog is
// checked against the Go constants rather than against a list kept by hand
// in two places.
func TestWebCatalogCoversCopyStatesAndMountModes(t *testing.T) {
	known := tableKeys(t, webI18nSource(t), "zh")
	for _, state := range []journal.CopyState{
		journal.CopyPreparing, journal.CopyReady, journal.CopySubmitted,
		journal.CopyFailed, journal.CopyCancelled, journal.CopyPurging,
	} {
		if key := "copy.state." + string(state); !known[key] {
			t.Errorf("i18n.js has no translation for copy state %q", state)
		}
	}
	for _, mode := range []config.Mode{config.ModeWriteback, config.ModeStrict, config.ModeReadonly} {
		if key := "mode." + string(mode); !known[key] {
			t.Errorf("i18n.js has no translation for mount mode %q", mode)
		}
	}
	for _, tier := range []provider.Tier{provider.TierOfficial, provider.TierUnofficial} {
		if key := "tier." + string(tier); !known[key] {
			t.Errorf("i18n.js has no translation for API tier %q", tier)
		}
	}
}

// TestEveryTranslationKeyUsedByTheAppExists: a t('...') whose key is in
// neither table renders as the key on every screen, in every language.
func TestEveryTranslationKeyUsedByTheAppExists(t *testing.T) {
	src := webI18nSource(t)
	known := tableKeys(t, src, "zh")
	// A key built by concatenation — t('avail.' + x) — is not a literal and
	// cannot be checked here, so the pattern demands the argument end at the
	// quote.
	used := regexp.MustCompile(`\bt\('([a-z][a-zA-Z0-9._]*)'\s*[,)]`)
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			sub, _ := webFS.ReadDir("web/" + e.Name())
			for _, s := range sub {
				files = append(files, "web/"+e.Name()+"/"+s.Name())
			}
			continue
		}
		files = append(files, "web/"+e.Name())
	}
	for _, name := range files {
		if name == "web/i18n.js" || !strings.HasSuffix(name, ".js") {
			continue
		}
		b, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range used.FindAllStringSubmatch(string(b), -1) {
			// Keys assembled at the call site — t('avail.' + x) — cannot be
			// checked here; the regex only matches complete literals.
			if !known[m[1]] {
				t.Errorf("%s uses t(%q), which is in no table", name, m[1])
			}
		}
	}
}
