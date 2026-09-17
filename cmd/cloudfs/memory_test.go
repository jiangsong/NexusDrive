package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/memory"
)

// cliMemory is MemoryControl over a map: the daemon side of the CLI tests,
// recording what reached it so a test can prove the command sent the
// right agent, name, body and version.
type cliMemory struct {
	layout, migratedTo string
	mu       sync.Mutex
	root     string
	facts    map[string]memory.Fact // agent/name
	deleted  []string
	searches []memory.SearchOptions
}

func newCLIMemory() *cliMemory {
	return &cliMemory{root: "/work/.agent", facts: map[string]memory.Fact{}}
}

func (m *cliMemory) Root() string { return m.root }
func (m *cliMemory) Config() config.Memory {
	return config.Memory{Root: m.root, MaxFactBytes: 64 << 10, MaxAgentBytes: 32 << 20}
}
func (m *cliMemory) HasIndex() bool { return true }
func (m *cliMemory) Agents(context.Context) ([]memory.AgentSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[string]*memory.AgentSummary{}
	for k, f := range m.facts {
		ag, _, _ := strings.Cut(k, "/")
		s := by[ag]
		if s == nil {
			s = &memory.AgentSummary{Name: ag, MaxBytes: 32 << 20}
			by[ag] = s
		}
		s.Facts++
		s.Bytes += f.Size
	}
	out := []memory.AgentSummary{}
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (m *cliMemory) List(_ context.Context, agent, cursor string, limit int) ([]memory.FactMeta, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []memory.FactMeta{}
	for k, f := range m.facts {
		if strings.HasPrefix(k, agent+"/") {
			out = append(out, f.FactMeta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, "", nil
}
func (m *cliMemory) Get(_ context.Context, agent, name string) (memory.Fact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.facts[agent+"/"+name]
	if !ok {
		return memory.Fact{}, fmt.Errorf("%w: %s/%s", memory.ErrNotFound, agent, name)
	}
	return f, nil
}
func (m *cliMemory) Put(_ context.Context, agent, name, content string, opt memory.PutOptions) (memory.Fact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !memory.ValidName(name) {
		return memory.Fact{}, fmt.Errorf("name %q: %w", name, memory.ErrBadName)
	}
	cur := m.facts[agent+"/"+name]
	if opt.ExpectedVersion != "" && cur.Version != opt.ExpectedVersion {
		return memory.Fact{}, fmt.Errorf("%w (current version %q)", memory.ErrVersionChanged, cur.Version)
	}
	if opt.Mode == "append" {
		content = cur.Content + content
	}
	f := memory.Fact{Content: content, Version: memory.Version([]byte(content))}
	f.Name, f.Agent, f.Size, f.Conflicts = name, agent, int64(len(content)), []string{}
	f.Path = "/work/.agent/memory/" + agent + "/facts/" + name + ".md"
	f.Meta.Name, f.Meta.Description, f.Meta.Type = name, opt.Description, opt.Type
	if f.Meta.Description == "" {
		f.Meta.Description = cur.Meta.Description
	}
	m.facts[agent+"/"+name] = f
	return f, nil
}
func (m *cliMemory) Delete(_ context.Context, agent, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.facts[agent+"/"+name]; !ok {
		return fmt.Errorf("%w: %s/%s", memory.ErrNotFound, agent, name)
	}
	delete(m.facts, agent+"/"+name)
	m.deleted = append(m.deleted, agent+"/"+name)
	return nil
}
func (m *cliMemory) Search(_ context.Context, opt memory.SearchOptions) (memory.SearchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searches = append(m.searches, opt)
	res := memory.SearchResult{Hits: []memory.SearchHit{}, ModeUsed: "keyword"}
	for k, f := range m.facts {
		if strings.HasPrefix(k, opt.Agent+"/") && strings.Contains(f.Content, opt.Query) {
			h := memory.SearchHit{Agent: f.Agent, Name: f.Name}
			h.Path, h.Snippet = f.Path, f.Content
			res.Hits = append(res.Hits, h)
		}
	}
	return res, nil
}

func memoryCLIDaemon(t *testing.T) (*cliMemory, string) {
	t.Helper()
	cfg, p := uploadCLIConfig(t)
	fake := newCLIMemory()
	srv, err := control.NewServer(&control.Collector{Memory: fake}).Start(context.Background(), cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return fake, p
}

func TestMemoryCLIPutsAndGets(t *testing.T) {
	ctx := context.Background()
	_, offline := uploadCLIConfig(t)
	var out bytes.Buffer
	// Every action needs the daemon: the VFS the facts live in is its.
	for _, args := range [][]string{{"agents"}, {"list"}, {"get", "style"}, {"delete", "style", "--confirm"}, {"search", "x"}} {
		err := runMemoryWith(ctx, append(args, "--config", offline), strings.NewReader(""), &out)
		if err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
			t.Fatalf("%v offline: %v", args, err)
		}
	}
	if err := runMemoryWith(ctx, []string{"put", "style", "--config", offline}, strings.NewReader("body\n"), &out); err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
		t.Fatalf("put offline: %v", err)
	}
	for _, bad := range [][]string{{}, {"dance"}, {"get"}, {"get", "Upper"}, {"put"}, {"put", "a/b"}, {"delete"}, {"search"}, {"list", "extra"}, {"get", "x", "--agent", "Bad"}, {"put", "x", "--mode", "prepend"}} {
		if err := runMemoryWith(ctx, append(bad, "--config", offline), strings.NewReader(""), &out); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}

	fake, p := memoryCLIDaemon(t)
	file := filepath.Join(t.TempDir(), "style.md")
	if err := os.WriteFile(file, []byte("tabs, not spaces\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runMemoryWith(ctx, []string{"put", "style", "--agent", "claude-code", "--file", file, "--description", "code style", "--type", "preference", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	got, ok := fake.facts["claude-code/style"]
	if !ok || got.Content != "tabs, not spaces\n" || got.Meta.Description != "code style" || got.Meta.Type != "preference" {
		t.Fatalf("put: %+v", fake.facts)
	}
	if !strings.Contains(out.String(), got.Path) || !strings.Contains(out.String(), got.Version) {
		t.Fatalf("put output: %s", out.String())
	}

	// Without --file the body comes from stdin; --agent defaults to shared.
	out.Reset()
	if err := runMemoryWith(ctx, []string{"put", "team", "--config", p}, strings.NewReader("we ship on fridays\n"), &out); err != nil {
		t.Fatal(err)
	}
	if f := fake.facts["shared/team"]; f.Content != "we ship on fridays\n" {
		t.Fatalf("stdin put: %+v", fake.facts)
	}

	// A stale --expected-version is refused and names the current one.
	err := runMemoryWith(ctx, []string{"put", "style", "--agent", "claude-code", "--expected-version", "stale", "--config", p}, strings.NewReader("x"), &out)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), got.Version) {
		t.Fatalf("stale put: %v", err)
	}
	if fake.facts["claude-code/style"].Content != "tabs, not spaces\n" {
		t.Fatal("stale put changed the fact")
	}
	if err := runMemoryWith(ctx, []string{"put", "style", "--agent", "claude-code", "--expected-version", got.Version, "--append", "--config", p}, strings.NewReader("and gofmt\n"), &out); err != nil {
		t.Fatal(err)
	}
	if fake.facts["claude-code/style"].Content != "tabs, not spaces\nand gofmt\n" {
		t.Fatalf("append: %+v", fake.facts["claude-code/style"])
	}

	// get prints the body verbatim, so it can be piped; --json the fact.
	out.Reset()
	if err := runMemoryWith(ctx, []string{"get", "style", "--agent", "claude-code", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "tabs, not spaces\nand gofmt\n" {
		t.Fatalf("get: %q", out.String())
	}
	out.Reset()
	if err := runMemoryWith(ctx, []string{"get", "style", "--agent", "claude-code", "--json", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var f memory.Fact
	if err := json.Unmarshal(out.Bytes(), &f); err != nil || f.Name != "style" || f.Version != fake.facts["claude-code/style"].Version {
		t.Fatalf("get --json: %s %v", out.String(), err)
	}
	if err := runMemoryWith(ctx, []string{"get", "missing", "--agent", "claude-code", "--config", p}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("get missing: %v", err)
	}

	// list and agents are tables; search names the fact of every hit.
	out.Reset()
	if err := runMemoryWith(ctx, []string{"list", "--agent", "claude-code", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "NAME") || !strings.Contains(s, "style") || !strings.Contains(s, "code style") {
		t.Fatalf("list: %s", s)
	}
	out.Reset()
	if err := runMemoryWith(ctx, []string{"agents", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "claude-code") || !strings.Contains(s, "shared") || !strings.Contains(s, "/work/.agent") {
		t.Fatalf("agents: %s", s)
	}
	out.Reset()
	if err := runMemoryWith(ctx, []string{"search", "fridays", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "shared/team") {
		t.Fatalf("search: %s", s)
	}
	if len(fake.searches) != 1 || fake.searches[0].Agent != "shared" || fake.searches[0].Query != "fridays" {
		t.Fatalf("search options: %+v", fake.searches)
	}
}

func TestMemoryCLIDeleteNeedsConfirm(t *testing.T) {
	ctx := context.Background()
	fake, p := memoryCLIDaemon(t)
	if _, err := fake.Put(ctx, "codex", "todo", "ship it\n", memory.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runMemoryWith(ctx, []string{"delete", "todo", "--agent", "codex", "--config", p}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "--confirm") || !strings.Contains(err.Error(), "codex/todo") {
		t.Fatalf("delete without confirm: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("the daemon was asked to delete: %v", fake.deleted)
	}
	if err := runMemoryWith(ctx, []string{"delete", "todo", "--agent", "codex", "--confirm", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "codex/todo" || !strings.Contains(out.String(), "deleted") {
		t.Fatalf("delete: %v %s", fake.deleted, out.String())
	}
	if err := runMemoryWith(ctx, []string{"delete", "todo", "--agent", "codex", "--confirm", "--config", p}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("delete twice: %v", err)
	}
}

func (m *cliMemory) Layout(context.Context) string {
	if m.layout == "" {
		return memory.LayoutV1
	}
	return m.layout
}

func (m *cliMemory) Migrate(_ context.Context, owner string) ([]string, error) {
	if m.layout == memory.LayoutV2 {
		return nil, nil
	}
	m.layout = memory.LayoutV2
	m.migratedTo = owner
	return []string{"codex"}, nil
}

func (m *cliMemory) Merge(context.Context, string, string, string, string) (memory.MergeResult, error) {
	return memory.MergeResult{}, memory.ErrNoConflict
}

// TestMemoryCLIMigrateNeedsConfirm: migrate refuses without --confirm
// and asks the daemon nothing; with it the daemon moves the tree once,
// names the owner, and a second run finds nothing to move.
func TestMemoryCLIMigrateNeedsConfirm(t *testing.T) {
	ctx := context.Background()
	fake, p := memoryCLIDaemon(t)
	var out bytes.Buffer
	if err := runMemoryWith(ctx, []string{"migrate", "--owner", "alice", "--config", p}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("migrate without confirm: %v", err)
	}
	if fake.layout != "" {
		t.Fatal("the daemon migrated without confirmation")
	}
	if err := runMemoryWith(ctx, []string{"migrate", "--owner", "alice", "--confirm", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if fake.migratedTo != "alice" || !strings.Contains(out.String(), "codex -> alice/codex") || !strings.Contains(out.String(), "v2") {
		t.Fatalf("migrate: %q %s", fake.migratedTo, out.String())
	}
	out.Reset()
	if err := runMemoryWith(ctx, []string{"migrate", "--confirm", "--config", p}, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "nothing to move") {
		t.Fatalf("second migrate: %v %s", err, out.String())
	}
	// An owner/agent key is accepted for --agent.
	if err := runMemoryWith(ctx, []string{"list", "--agent", "alice/codex", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("owner/agent key: %v", err)
	}
	if err := runMemoryWith(ctx, []string{"list", "--agent", "Alice/codex", "--config", p}, strings.NewReader(""), &out); err == nil {
		t.Fatal("a bad owner was accepted")
	}
}
