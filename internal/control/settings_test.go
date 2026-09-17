package control

import (
	"encoding/json"
	"testing"

	"cloudfs/internal/config"
)

// TestSettingsViewIsReadOnlyAndCarriesNoSecret: the route projects the
// configuration onto its whitelist with durations the YAML accepts back,
// defaults filled where the file is silent, refuses every mutating method,
// and no key at any depth is one config.IsSecretField names.
func TestSettingsViewIsReadOnlyAndCarriesNoSecret(t *testing.T) {
	f := newFixture(t)
	cfg := config.Default()
	cfg.MCP.Allow = []string{"/work"}
	cfg.MCP.Limits.MaxTokens = 12000
	cfg.MCP.Session = config.DefaultMCPSession()
	cfg.MCP.Audit = config.DefaultMCPAudit()
	cfg.Hooks.Context = "full"
	cfg.Index.Enabled = true
	cfg.Remotes = map[string]config.Remote{"gd": {Type: "gdrive", Extra: map[string]any{"refresh_token": "SECRET-VALUE"}}}
	f.coll.PublishConfigView(&cfg)
	s := NewServer(f.coll)
	w := call(t, s, "GET", "/settings", "")
	var v SettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); w.Code != 200 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if v.MCP.MaxTokens != 12000 || v.MCP.Transport != "auto" || len(v.MCP.Allow) != 1 || v.MCP.Allow[0] != "/work" {
		t.Fatalf("mcp: %+v", v.MCP)
	}
	if v.MCP.Session.Idle != "30m" || v.MCP.Session.Retain != "720h" || v.MCP.Session.RetainBlobs != "168h" || v.MCP.Session.PreimageFiles != 500 || v.MCP.AuditRetain != "2160h" {
		t.Fatalf("session: %+v audit=%s", v.MCP.Session, v.MCP.AuditRetain)
	}
	if v.Hooks.Context != "full" || v.Hooks.ChangedMax != config.DefaultHookChangedMax || v.Hooks.MemoryHeadLines != config.DefaultHookMemoryHeadLines || !v.Index.Enabled || v.Heat.Enabled || v.Heat.RetentionDays != config.DefaultHeatRetentionDays {
		t.Fatalf("hooks/index/heat: %+v %+v %+v", v.Hooks, v.Index, v.Heat)
	}
	body := w.Body.String()
	if containsFold(body, "SECRET-VALUE") || containsFold(body, "gdrive") {
		t.Fatalf("the view leaks the remotes: %s", body)
	}
	var doc any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if key := secretKey(doc); key != "" {
		t.Fatalf("the view carries %q", key)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if w := call(t, s, method, "/settings", `{"mcp":{"max_tokens":1}}`); w.Code != 405 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	f.coll.PublishConfigView(nil)
	if w := call(t, s, "GET", "/settings", ""); w.Code != 503 {
		t.Fatalf("without a config: %d", w.Code)
	}
}

// secretKey walks decoded JSON for a key config.IsSecretField names.
func secretKey(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if config.IsSecretField(k) {
				return k
			}
			if key := secretKey(child); key != "" {
				return key
			}
		}
	case []any:
		for _, child := range x {
			if key := secretKey(child); key != "" {
				return key
			}
		}
	}
	return ""
}
