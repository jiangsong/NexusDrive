package control

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/hooks"
)

// turnStartContext is the text one turn of a hook session is injected with.
func turnStartContext(t *testing.T) string {
	t.Helper()
	f, _, _, mount := hooksFixture(t)
	s := NewServer(f.coll)
	req := hooks.ContextRequest{Client: "claude", SessionID: "sess-guidance", CWD: filepath.Join(mount, "docs")}
	var resp hooks.ContextResponse
	if err := json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Context == "" {
		t.Fatal("the turn-start hook injected nothing")
	}
	return resp.Context
}

// The turn-start hook is the one place an agent is told what reads cost
// here, and it used to tell it the opposite of the truth: "read what you
// need rather than whole trees". Reading a directory in listing order is
// the cheap path — internal/vfs/read_dir_ahead.go arms after three
// in-order reads and pulls the small siblings ahead, which is why
// test/perf/dirahead_test.go can assert those sibling reads cost no
// ReadRange call at all — and it is scattered reads across directories
// that pay a request each. An agent that follows the old sentence walks
// away from the only fast path it has.
func TestTheTurnStartHookExplainsDirectoryOrderedReads(t *testing.T) {
	ctx := turnStartContext(t)
	for _, gone := range []string{"read what you need rather than whole trees", "rather than whole trees"} {
		if strings.Contains(ctx, gone) {
			t.Errorf("the turn-start hook still steers away from whole directories (%q):\n%s", gone, ctx)
		}
	}
	for _, want := range []string{"in name order", "scattered"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("the turn-start hook never says %q:\n%s", want, ctx)
		}
	}
}

// pin is the first step before a shell burst over a repository — it makes
// the subtree resident and exempt from eviction (internal/vfs/pin.go,
// internal/cache/blockcache.go), and TODO.md T-14 assumes a pinned
// directory when it talks about git status — but nothing has ever told an
// agent to reach for it.
func TestTheTurnStartHookNamesPinForRepositories(t *testing.T) {
	ctx := turnStartContext(t)
	for _, want := range []string{"pin", "cloudfs pin"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("the turn-start hook never names %q:\n%s", want, ctx)
		}
	}
}
