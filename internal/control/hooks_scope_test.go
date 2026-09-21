package control

import (
	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeCanonicalMountGitAndPlain(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	nested := filepath.Join(mount, "nested")
	repo := filepath.Join(nested, "repo")
	cwd := filepath.Join(repo, "src")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere"), 0600)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Skip(err)
	}
	cfg := &config.Config{Mounts: []config.Mount{{Path: mount}, {Path: nested}}}
	m, v, ok := mountFor(cfg, filepath.Join(alias, "repo", "src"))
	if !ok || m.Path != nested || v != "/repo/src" {
		t.Fatalf("%+v %s %t", m, v, ok)
	}
	if got := projectScope(m, filepath.Join(alias, "repo", "src"), v); got != "/repo" {
		t.Fatal(got)
	}
	if got := projectScope(m, nested, "/"); got != "/" {
		t.Fatal(got)
	}
	if _, _, ok := mountFor(cfg, mount+"-outside"); ok {
		t.Fatal("crossed mount boundary")
	}
}
func TestHookScopeSharedAndLaterTurnBrief(t *testing.T) {
	f, _, _, mount := hooksFixture(t)
	s := NewServer(f.coll)
	var first hooks.ContextResponse
	q := hooks.ContextRequest{Client: "codex", SessionID: "one", CWD: filepath.Join(mount, "docs")}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", q), &first)
	if first.Scope != "/docs" || first.VirtualPath != "/docs" || !strings.Contains(first.Index, "disabled") {
		t.Fatalf("%+v", first)
	}
	var later hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", q), &later)
	if len(later.Context) >= len(first.Context) {
		t.Fatal("full guidance repeated")
	}
	q.Client = "claude"
	q.SessionID = "two"
	var other hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", q), &other)
	if other.Scope != first.Scope {
		t.Fatal("client-dependent scope")
	}
}

func TestDirectoryInspectionWorksWithInjectionDisabled(t *testing.T) {
	f, _, _, mount := hooksFixture(t)
	f.coll.Config.Hooks.Context = "off"
	s := NewServer(f.coll)
	var out hooks.ContextResponse
	q := hooks.ContextRequest{CWD: filepath.Join(mount, "docs"), Inspect: true}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", q), &out)
	if out.Scope != "/docs" {
		t.Fatalf("explicit skill cannot resolve directory: %+v", out)
	}
	q.Inspect = false
	out = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", q), &out)
	if out.Context != "" {
		t.Fatal("disabled hook injected context")
	}
}
