package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// readHeatFor installs the daemon's read-heat hook on the env's VFS, the
// way internal/daemon does, and returns the observer.
func readHeatFor(t *testing.T, e *env, st *agent.Store) *agent.ReadObserver {
	t.Helper()
	obs := agent.NewReadObserver(st)
	e.fs.SetReadObserver(func(ctx context.Context, ino uint64) {
		kind := ""
		switch {
		case vfs.IsFromKernel(ctx):
			kind = agent.ReadByKernel
		case vfs.OriginName(ctx) == "mcp":
			kind = agent.ReadByAgent
		}
		if kind == "" {
			return
		}
		if p, err := e.fs.Meta().Path(ctx, ino); err == nil {
			obs.Observe(p, kind)
		}
	})
	return obs
}

// TestReadHeatCountsAgentAndKernelReadsWithoutRemoteCalls: an agent's
// read_text and a kernel read of the same file count once each under
// their kind; the daemon's own background read counts nothing; hot_paths
// reports them, and none of it costs a provider call beyond the reads
// themselves.
func TestReadHeatCountsAgentAndKernelReadsWithoutRemoteCalls(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	obs := readHeatFor(t, e, st)
	e.fake.Seed("work/hot.md", []byte("hot content"))
	e.fake.Seed("work/cold.md", []byte("cold"))
	for i := 0; i < 3; i++ {
		if res := e.call(t, "read_text", readTextInput{Path: "/work/hot.md"}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	kctx := vfs.FromKernel(context.Background())
	if _, err := e.fs.ReadFileRange(kctx, "/work/hot.md", 0, 0); err != nil {
		t.Fatal(err)
	}
	// Background work: no origin, not the kernel.
	if _, err := e.fs.ReadFileRange(context.Background(), "/work/cold.md", 0, 0); err != nil {
		t.Fatal(err)
	}
	if obs.Pending() != 2 {
		t.Fatalf("pending samples = %d, want agent+kernel for hot.md only", obs.Pending())
	}
	if err := obs.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := e.fake.TotalCalls()
	var out hotPathsOutput
	if res := e.call(t, "hot_paths", hotPathsInput{Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("hot_paths reached the provider: %d→%d", calls, e.fake.TotalCalls())
	}
	if len(out.Paths) != 1 || out.Paths[0].Path != "/work/hot.md" || out.Paths[0].Reads != 2 || out.Paths[0].ByKind[agent.ReadByAgent] != 1 || out.Paths[0].ByKind[agent.ReadByKernel] != 1 {
		t.Fatalf("%+v", out.Paths)
	}
	if out.Paths[0].MTime == "" || out.Paths[0].Stale || out.Paths[0].LastRead == "" {
		t.Fatalf("mtime/stale: %+v", out.Paths[0])
	}
	if toolNames(t, newEnv(t, Options{}))["hot_paths"] {
		t.Fatal("hot_paths registered without a store")
	}
}

// TestHotPathsMarksStaleAndSuggestsPins: a file changed before the
// window but read inside it is stale; hot files that are not cached
// earn a pin suggestion for their directory, and nothing is pinned.
func TestHotPathsMarksStaleAndSuggestsPins(t *testing.T) {
	e, st := newAgentEnv(t, Options{Allow: []string{"/work"}}, agent.Scope{Read: []string{"/work"}})
	old := time.Now().Add(-30 * 24 * time.Hour)
	e.fake.Seed("work/old.md", []byte("old but hot"))
	e.fake.SetMTime("work/old.md", old)
	e.fake.Seed("work/uncached.bin", []byte(strings.Repeat("x", 100)))
	e.fake.Seed("private/p.md", []byte("p"))
	e.listDirs(t, "/work", "/private")
	now := time.Now()
	if err := st.BumpReadHeat(context.Background(), []agent.ReadSample{
		{Path: "/work/old.md", ActorKind: agent.ReadByAgent, TS: now, Count: 5},
		{Path: "/work/uncached.bin", ActorKind: agent.ReadByKernel, TS: now, Count: 2},
		{Path: "/private/p.md", ActorKind: agent.ReadByAgent, TS: now, Count: 9},
	}); err != nil {
		t.Fatal(err)
	}
	var out hotPathsOutput
	if res := e.call(t, "hot_paths", hotPathsInput{Days: 7}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	byPath := map[string]hotPath{}
	for _, hp := range out.Paths {
		byPath[hp.Path] = hp
	}
	if _, leaked := byPath["/private/p.md"]; leaked {
		t.Fatal("a path outside the scope was reported")
	}
	if hp := byPath["/work/old.md"]; !hp.Stale || hp.Reads != 5 {
		t.Fatalf("old.md: %+v", hp)
	}
	if hp := byPath["/work/uncached.bin"]; hp.Stale {
		t.Fatalf("uncached.bin: %+v", hp)
	}
	found := false
	for _, s := range out.Suggestions {
		found = found || strings.HasPrefix(s, "pin /work")
	}
	if !found {
		t.Fatalf("no pin suggestion: %v", out.Suggestions)
	}
	if a, err := e.fs.StatPath(context.Background(), "/work/uncached.bin"); err != nil || a.Pinned || a.Cached > 0 {
		t.Fatalf("hot_paths pinned or downloaded: %+v %v", a, err)
	}
	if res := e.call(t, "hot_paths", hotPathsInput{Path: "/private"}, nil); !res.IsError {
		t.Fatal("hot_paths outside the scope answered")
	}
}

// TestHotPathsNeverCarryPrincipal: the hot_paths result — its structured
// content and its text alike — names no principal, session, token or
// user at any depth. The heat tables aggregate by kind of reader; who
// read a file is the audit trail's business, behind its own filters.
func TestHotPathsNeverCarryPrincipal(t *testing.T) {
	e, st := newAgentEnv(t, Options{Allow: []string{"/work"}}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/a.md", []byte("a"))
	e.listDirs(t, "/work")
	now := time.Now()
	if err := st.BumpReadHeat(context.Background(), []agent.ReadSample{
		{Path: "/work/a.md", ActorKind: agent.ReadByAgent, TS: now, Count: 3},
		{Path: "/work/a.md", ActorKind: agent.ReadByKernel, TS: now, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	res := e.call(t, "hot_paths", hotPathsInput{Days: 7}, nil)
	if res.IsError {
		t.Fatal(errText(res))
	}
	var doc any
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &doc); err != nil {
		t.Fatal(err)
	}
	if key := identityKeyIn(doc); key != "" {
		t.Fatalf("hot_paths carries %q: %s", key, mustJSON(t, res.StructuredContent))
	}
	for _, bad := range []string{"principal", "session", "token", "user"} {
		if strings.Contains(strings.ToLower(firstText(res)), bad) {
			t.Fatalf("hot_paths text names %q: %s", bad, firstText(res))
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// identityKeyIn walks decoded JSON for a key naming an identity.
func identityKeyIn(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			lk := strings.ToLower(k)
			for _, bad := range []string{"principal", "session", "token", "user"} {
				if strings.Contains(lk, bad) {
					return k
				}
			}
			if key := identityKeyIn(child); key != "" {
				return key
			}
		}
	case []any:
		for _, child := range x {
			if key := identityKeyIn(child); key != "" {
				return key
			}
		}
	}
	return ""
}
