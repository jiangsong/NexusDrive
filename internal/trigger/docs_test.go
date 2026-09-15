package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// The user-facing copies of the webhook contract — README.md, the roadmap
// and the console's empty state — must agree with each other and with
// Verify, or a receiver written from the docs rejects every hook.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// verifySnippet extracts the "func verify(" Go block from a document.
var verifySnippet = regexp.MustCompile(`(?s)func verify\(secret \[\]byte, r \*http\.Request, body \[\]byte, now time\.Time\) bool \{.*?\n\}\n`)

// code strips trailing // comments from a snippet.
func code(snippet string) string {
	return regexp.MustCompile(`\s*//[^\n]*`).ReplaceAllString(snippet, "")
}

func TestWebhookVerifySnippetIsTheSameEverywhere(t *testing.T) {
	readme := verifySnippet.FindString(repoFile(t, "README.md"))
	roadmap := verifySnippet.FindString(repoFile(t, "docs/agent-roadmap.md"))
	ui := verifySnippet.FindString(repoFile(t, "internal/control/web/trigger_view.js"))
	if readme == "" || roadmap == "" || ui == "" {
		t.Fatalf("a verify snippet is missing: readme=%d roadmap=%d ui=%d bytes", len(readme), len(roadmap), len(ui))
	}
	// The code must be the same; a trailing comment may be in the
	// document's language (the console has no Chinese outside i18n_zh.js).
	if code(readme) != code(roadmap) || code(readme) != code(ui) {
		t.Fatalf("the verify snippets differ:\nREADME:\n%s\nroadmap:\n%s\nconsole:\n%s", readme, roadmap, ui)
	}
	// What the snippet hard-codes is what Verify uses.
	for _, want := range []string{`"` + HeaderTimestamp + `"`, `"` + HeaderSignature + `"`, "5*time.Minute", `ts + "."`, `"sha256=" + hex.EncodeToString`, "hmac.Equal"} {
		if !strings.Contains(readme, want) {
			t.Errorf("the snippet lacks %s", want)
		}
	}
	if MaxSignatureSkew != 5*time.Minute {
		t.Errorf("MaxSignatureSkew = %v, the documented skew is 5 minutes", MaxSignatureSkew)
	}
	// And the snippet's arithmetic, transcribed, accepts what Verify signs.
	secret := []byte("s3cret")
	body := []byte(`{"rule":"notify"}`)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r, _ := http.NewRequest(http.MethodPost, "https://hooks.example/", nil)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderSignature, Sign(secret, ts, body))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(r.Header.Get(HeaderSignature))) || !Verify(secret, r, body, now) {
		t.Fatal("the documented computation and Verify disagree on a signed request")
	}
}

// TestReadmeTriggerExampleParses: the triggers:/agents: block in README's
// configuration example is a valid, warning-free configuration.
func TestReadmeTriggerExampleParses(t *testing.T) {
	readme := repoFile(t, "README.md")
	start := strings.Index(readme, "\ntriggers:")
	end := strings.Index(readme, "\nwebdav:")
	if start < 0 || end < 0 || end < start {
		t.Fatal("README's configuration example has no triggers: block before webdav:")
	}
	c, err := config.Parse([]byte(readme[start+1 : end+1]))
	if err != nil {
		t.Fatalf("README triggers example: %v", err)
	}
	if len(c.Triggers) != 2 || len(c.Agents) != 1 {
		t.Fatalf("triggers = %d, agents = %d", len(c.Triggers), len(c.Agents))
	}
	if c.Triggers[0].Action.Exec == nil || c.Triggers[1].Action.Webhook == nil {
		t.Fatalf("the example should show one exec and one webhook rule: %+v", c.Triggers)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("the README example must be warning-free, got %q", c.Warnings)
	}
}
