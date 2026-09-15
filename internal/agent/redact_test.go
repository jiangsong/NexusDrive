package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRedactArgsKeepsLargeContentOutAndBounded(t *testing.T) {
	big := strings.Repeat("a", 204800)
	tail := " see https://cdn.example/x?sig=1"
	raw, _ := json.Marshal(map[string]any{"path": "/work/a.md", "content": big + tail})
	args, n := RedactArgs(raw)
	if n != int64(len(big)+len(tail)) {
		t.Fatalf("bytes_in = %d", n)
	}
	if len(args) > MaxAuditArgs {
		t.Fatalf("args %d bytes", len(args))
	}
	if bytes.Contains(args, []byte("aaaa")) || bytes.Contains(args, []byte("http")) {
		t.Fatalf("leaked: %s", args)
	}
	if !bytes.Contains(args, []byte(`"/work/a.md"`)) {
		t.Fatalf("path dropped: %s", args)
	}
	if !bytes.Contains(args, []byte(fmt.Sprintf(`"content":{"bytes":%d}`, len(big)+len(tail)))) {
		t.Fatalf("content not summarised by size: %s", args)
	}
}

func TestRedactArgsRedactsEditsTokensAndURLs(t *testing.T) {
	raw := []byte(`{"path":"/w","edits":[{"old_text":"secret-old","new_text":"secret-new"}],"token":"t","cookie":"c","Authorization":"Bearer x","note":"http://x","nested":{"api_secret":"s","link":"ftp://h/p","ok":"plain"}}`)
	args, n := RedactArgs(raw)
	for _, leak := range []string{"secret-old", "secret-new", `"t"`, `"c"`, "http", "Bearer", `"s"`, "ftp://", "token", "cookie", "Authorization", "api_secret"} {
		if bytes.Contains(args, []byte(leak)) {
			t.Fatalf("leaked %q: %s", leak, args)
		}
	}
	for _, keep := range []string{`"path":"/w"`, `"old_text":{"bytes":10}`, `"new_text":{"bytes":10}`, `"note":"[url]"`, `"link":"[url]"`, `"ok":"plain"`} {
		if !bytes.Contains(args, []byte(keep)) {
			t.Fatalf("missing %q: %s", keep, args)
		}
	}
	if n != 20 {
		t.Fatalf("edits count as bytes in: %d", n)
	}
}

func TestRedactArgsTruncatesAWideObject(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 500; i++ {
		m[fmt.Sprintf("k%03d", i)] = strings.Repeat("v", 30)
	}
	raw, _ := json.Marshal(m)
	args, _ := RedactArgs(raw)
	if len(args) > MaxAuditArgs || !bytes.Contains(args, []byte(`"truncated":true`)) {
		t.Fatalf("%d %s", len(args), args[:80])
	}
	var out struct {
		Truncated bool     `json:"truncated"`
		Keys      []string `json:"keys"`
	}
	if err := json.Unmarshal(args, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) == 0 || out.Keys[0] != "k000" || bytes.Contains(args, []byte("vvv")) {
		t.Fatalf("keys must be listed in order and values dropped: %s", args)
	}
}

func TestRedactArgsToleratesOddInput(t *testing.T) {
	for _, raw := range []string{"", "null", "not json", `[1,"https://x",{"content":"ab"}]`, `"https://x"`, `42`} {
		args, n := RedactArgs(json.RawMessage(raw))
		if !json.Valid(args) || len(args) > MaxAuditArgs {
			t.Fatalf("%q -> %s", raw, args)
		}
		if bytes.Contains(args, []byte("http")) {
			t.Fatalf("%q leaked a URL: %s", raw, args)
		}
		if raw == `[1,"https://x",{"content":"ab"}]` && n != 2 {
			t.Fatalf("%q: bytes in = %d", raw, n)
		}
	}
}
