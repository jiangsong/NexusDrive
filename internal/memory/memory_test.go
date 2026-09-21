package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"

	"cloudfs/internal/net/retry"
)

// newStack builds a VFS over a fake provider, the way the mcpsrv tests do,
// and a Store rooted at /work/.agent over it.
func newStack(t *testing.T, cfg config.Memory) (*Store, *vfs.FS, *fakeprovider.Fake, *upload.Uploader) {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	up, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:  fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	if cfg.Root == "" {
		cfg.Root = "/work/.agent"
	}
	if cfg.MaxFactBytes == 0 {
		cfg.MaxFactBytes = config.DefaultMemoryMaxFactBytes
	}
	if cfg.MaxAgentBytes == 0 {
		cfg.MaxAgentBytes = config.DefaultMemoryMaxAgentBytes
	}
	fake.Seed("work/.keep", []byte(""))
	return New(Options{FS: fsys, Config: cfg}), fsys, fake, up
}

func readAll(t *testing.T, fsys *vfs.FS, p string) string {
	t.Helper()
	b, err := fsys.ReadFileRange(context.Background(), p, 0, 0)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestPutCreatesTheFactAndOneIndexLine(t *testing.T) {
	s, fsys, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	f, err := s.Put(ctx, "claude-code", "style", "Prefer tabs.\n", PutOptions{Description: "coding style", Type: "preference"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "/work/.agent/memory/claude-code/facts/style.md" || f.Version == "" {
		t.Fatalf("%+v", f)
	}
	body := readAll(t, fsys, f.Path)
	if !strings.HasPrefix(body, "---\nname: style\ndescription: coding style\ntype: preference\nupdated_at: ") || !strings.HasSuffix(body, "\n---\nPrefer tabs.\n") {
		t.Fatalf("fact file:\n%s", body)
	}
	index := readAll(t, fsys, "/work/.agent/memory/claude-code/MEMORY.md")
	if index != "- [style](facts/style.md) — coding style\n" {
		t.Fatalf("MEMORY.md:\n%q", index)
	}
	// A second put replaces the content and the line; the description is
	// kept when the caller does not repeat it.
	f2, err := s.Put(ctx, "claude-code", "style", "Prefer spaces.\n", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f2.Version == f.Version {
		t.Fatal("a changed fact must change its version")
	}
	index = readAll(t, fsys, "/work/.agent/memory/claude-code/MEMORY.md")
	if index != "- [style](facts/style.md) — coding style\n" {
		t.Fatalf("repeated put must not add a line:\n%q", index)
	}
	got, err := s.Get(ctx, "claude-code", "style")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "Prefer spaces.\n" || got.Meta.Description != "coding style" || got.Meta.Type != "preference" || got.Version != f2.Version {
		t.Fatalf("%+v", got)
	}
	// Append keeps what was there and updates the line's description.
	if _, err := s.Put(ctx, "claude-code", "style", "Also: gofmt.\n", PutOptions{Mode: "append", Description: "style rules"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "claude-code", "style")
	if got.Content != "Prefer spaces.\nAlso: gofmt.\n" {
		t.Fatalf("append: %q", got.Content)
	}
	index = readAll(t, fsys, "/work/.agent/memory/claude-code/MEMORY.md")
	if index != "- [style](facts/style.md) — style rules\n" {
		t.Fatalf("MEMORY.md after append:\n%q", index)
	}
	// A second fact gets its own line after the first.
	if _, err := s.Put(ctx, "claude-code", "repo-layout", "cmd/ and internal/\n", PutOptions{Description: "where code lives"}); err != nil {
		t.Fatal(err)
	}
	index = readAll(t, fsys, "/work/.agent/memory/claude-code/MEMORY.md")
	if index != "- [style](facts/style.md) — style rules\n- [repo-layout](facts/repo-layout.md) — where code lives\n" {
		t.Fatalf("MEMORY.md with two facts:\n%q", index)
	}
	// Delete removes the file and its line, and nothing else.
	if err := s.Delete(ctx, "claude-code", "style"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "claude-code", "style"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted fact: %v", err)
	}
	index = readAll(t, fsys, "/work/.agent/memory/claude-code/MEMORY.md")
	if index != "- [repo-layout](facts/repo-layout.md) — where code lives\n" {
		t.Fatalf("MEMORY.md after delete:\n%q", index)
	}
}

func TestPutRejectsFrontmatterBeyondTheReadableHeader(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	paths := make([]string, 100)
	for i := range paths {
		paths[i] = "/work/" + strings.Repeat("x", 50) + fmt.Sprint(i)
	}
	_, err := s.Put(context.Background(), "codex", "oversized-meta", "body\n", PutOptions{SourcePaths: paths})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("oversized frontmatter: %v", err)
	}
}

func TestStaleExpectedVersionIsRefused(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	f, err := s.Put(ctx, "codex", "style", "one\n", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "codex", "style", "two\n", PutOptions{ExpectedVersion: f.Version}); err != nil {
		t.Fatalf("matching version refused: %v", err)
	}
	_, err = s.Put(ctx, "codex", "style", "three\n", PutOptions{ExpectedVersion: f.Version})
	if !errors.Is(err, ErrVersionChanged) || !strings.Contains(err.Error(), "memory changed elsewhere; re-read") {
		t.Fatalf("stale version: %v", err)
	}
	got, _ := s.Get(ctx, "codex", "style")
	if got.Content != "two\n" {
		t.Fatalf("content changed by a refused put: %q", got.Content)
	}
	// A version on a fact that does not exist yet is stale by definition.
	if _, err := s.Put(ctx, "codex", "new", "x\n", PutOptions{ExpectedVersion: "anything"}); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("version on a missing fact: %v", err)
	}
}

func TestGetListsConflictCopies(t *testing.T) {
	s, fsys, fake, up := newStack(t, config.Memory{})
	ctx := context.Background()
	f, err := s.Put(ctx, "codex", "style", "mine\n", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	// Another device changes the fact on the drive before this device's
	// next write uploads: the upload lands as a conflict copy beside it.
	fake.Seed(strings.TrimPrefix(f.Path, "/"), []byte("theirs\n"))
	if _, err := s.Put(ctx, "codex", "style", "mine again\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDirPath(ctx, "/work/.agent/memory/codex/facts"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "codex", "style")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conflicts) != 1 || !strings.HasPrefix(got.Conflicts[0], "/work/.agent/memory/codex/facts/style") || got.Conflicts[0] == got.Path {
		t.Fatalf("conflicts: %+v (content %q)", got.Conflicts, got.Content)
	}
	agents, err := s.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Name != "codex" || agents[0].Facts != 1 || agents[0].Conflicts != 1 {
		t.Fatalf("%+v", agents)
	}
	facts, _, err := s.List(ctx, "codex", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || len(facts[0].Conflicts) != 1 {
		t.Fatalf("%+v", facts)
	}
}

func TestBudgetsAreEnforcedWithUsage(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{MaxFactBytes: 256, MaxAgentBytes: 600})
	ctx := context.Background()
	_, err := s.Put(ctx, "codex", "big", strings.Repeat("x", 300), PutOptions{})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "max_fact_bytes") {
		t.Fatalf("over max_fact_bytes: %v", err)
	}
	// Frontmatter counts: a body of 200 bytes lands around 260 on disk.
	if _, err := s.Put(ctx, "codex", "a", strings.Repeat("a", 150), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "codex", "b", strings.Repeat("b", 150), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "codex", "c", strings.Repeat("c", 150), PutOptions{})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "max_agent_bytes") || !strings.Contains(err.Error(), "2 facts") {
		t.Fatalf("over max_agent_bytes must report the usage: %v", err)
	}
	// Replacing an existing fact counts the new size in place of the old.
	if _, err := s.Put(ctx, "codex", "a", strings.Repeat("a", 160), PutOptions{}); err != nil {
		t.Fatalf("replacing within the budget: %v", err)
	}
	agents, err := s.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Facts != 2 || agents[0].Bytes <= 300 || agents[0].MaxBytes != 600 {
		t.Fatalf("%+v", agents)
	}
}

func TestAgentNameIsNormalised(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code": "claude-code", "codex": "codex", "OpenClaw_v2": "openclaw-v2",
		"  ": "agent", "": "agent", "--x--": "x", "a!!b": "a-b", "中文": "agent",
	} {
		if got := NormalizeAgent(in); got != want {
			t.Errorf("NormalizeAgent(%q) = %q, want %q", in, got, want)
		}
	}
	for _, ok := range []string{"style", "a", "repo-layout", "x1", strings.Repeat("a", 64)} {
		if !ValidName(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "Style", "a b", "a/b", "..", "a..b", strings.Repeat("a", 65), "a.md"} {
		if ValidName(bad) {
			t.Errorf("%q must be invalid", bad)
		}
	}
	s, _, _, _ := newStack(t, config.Memory{})
	if _, err := s.Put(context.Background(), "Codex", "x", "y", PutOptions{}); !errors.Is(err, ErrBadName) {
		t.Fatalf("an agent name is validated like a fact name: %v", err)
	}
	if _, err := s.Get(context.Background(), "codex", "../x"); !errors.Is(err, ErrBadName) {
		t.Fatalf("traversal in a fact name: %v", err)
	}
}

func TestSearchWithoutAnIndexSaysSo(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	if _, err := s.Search(context.Background(), SearchOptions{Query: "x", Agent: "codex"}); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("%v", err)
	}
}

func TestIndexRuleCoversTheMemoryTree(t *testing.T) {
	r := IndexRule("/work/.agent")
	if r.Path != "/work/.agent/memory" || r.Source != "builtin" || len(r.Include) != 1 || r.Include[0] != "**/*.md" {
		t.Fatalf("%+v", r)
	}
}
