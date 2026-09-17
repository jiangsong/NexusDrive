package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/meta"
)

// TestSuggestionsNeverWriteRules: the route drafts a pin for a hot file
// the cache does not hold, a stale notice for a hot file unchanged for
// ninety days, and an unpin for a pin nothing read; it costs no provider
// call once the tree is listed, and it changes neither pins nor index
// rules — adopting a draft is the console's job through the guarded
// routes. Without a heat store it answers enabled false.
func TestSuggestionsNeverWriteRules(t *testing.T) {
	f, fake := fsControl(t)
	if w := call(t, NewServer(f.coll), "GET", "/agent/suggestions", ""); w.Code != 200 || decode[SuggestionsResponse](t, w).Enabled {
		t.Fatalf("without a store: %d %s", w.Code, w.Body.String())
	}
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.coll.HeatStore = st
	ctx := context.Background()
	old := time.Now().Add(-100 * 24 * time.Hour)
	fake.SetMTime("/docs/b", old)
	for _, d := range []string{"/", "/docs", "/docs/sub"} {
		if _, err := f.coll.FS.ReadDirPath(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	// /docs/sub is pinned and nobody reads under it; /docs/b is hot, old
	// and not cached; /other is hot and fresh but not cached either.
	if err := f.coll.FS.Meta().AddPin(ctx, meta.Pin{Path: "/docs/sub", Recursive: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.BumpReadHeat(ctx, []agent.ReadSample{
		{Path: "/docs/b", ActorKind: agent.ReadByAgent, TS: now, Count: 9},
		{Path: "/other", ActorKind: agent.ReadByKernel, TS: now, Count: 2},
	}); err != nil {
		t.Fatal(err)
	}
	pinsBefore, _ := f.coll.FS.Meta().Pins(ctx)
	calls := fake.TotalCalls()
	s := NewServer(f.coll)
	resp := decode[SuggestionsResponse](t, call(t, s, "GET", "/agent/suggestions?days=30&path=/", ""))
	if !resp.Enabled || resp.Days != 30 {
		t.Fatalf("%+v", resp)
	}
	kinds := map[string]string{}
	for _, sg := range resp.Suggestions {
		kinds[sg.Kind+" "+sg.Path] = sg.Path
	}
	for _, want := range []string{"pin /docs/b", "stale /docs/b", "pin /other", "unpin /docs/sub"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("missing draft %q in %v", want, kinds)
		}
	}
	if _, ok := kinds["stale /other"]; ok {
		t.Errorf("a fresh file was called stale: %v", kinds)
	}
	if got := fake.TotalCalls(); got != calls {
		t.Fatalf("drafting suggestions cost %d provider calls", got-calls)
	}
	pinsAfter, _ := f.coll.FS.Meta().Pins(ctx)
	if len(pinsAfter) != len(pinsBefore) {
		t.Fatalf("pins changed: %v -> %v", pinsBefore, pinsAfter)
	}
	if w := call(t, s, "POST", "/agent/suggestions", "{}"); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	// A pin something reads under it stays.
	if err := st.BumpReadHeat(ctx, []agent.ReadSample{{Path: "/docs/sub/c", ActorKind: agent.ReadByAgent, TS: now, Count: 1}}); err != nil {
		t.Fatal(err)
	}
	resp = decode[SuggestionsResponse](t, call(t, s, "GET", "/agent/suggestions?days=30", ""))
	for _, sg := range resp.Suggestions {
		if sg.Kind == "unpin" {
			t.Fatalf("a read pin was offered for release: %+v", sg)
		}
	}
}

// TestHeatResponsesCarryNoIdentity: neither the heat nor the suggestions
// JSON carries a principal, session, token or user key at any depth — the
// heat tables aggregate by kind of reader, never by who.
func TestHeatResponsesCarryNoIdentity(t *testing.T) {
	f, _ := fsControl(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.coll.HeatStore = st
	ctx := context.Background()
	if _, err := f.coll.FS.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if err := st.BumpReadHeat(ctx, []agent.ReadSample{{Path: "/other", ActorKind: agent.ReadByAgent, TS: time.Now(), Count: 3}}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	for _, target := range []string{"/agent/heat?days=7", "/agent/suggestions?days=7"} {
		w := call(t, s, "GET", target, "")
		var doc any
		if err := json.Unmarshal(w.Body.Bytes(), &doc); w.Code != 200 || err != nil {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body.String())
		}
		if key := identityKey(doc); key != "" {
			t.Fatalf("%s carries %q: %s", target, key, w.Body.String())
		}
	}
}

// identityKey walks decoded JSON and returns the first key naming an
// identity, "" when there is none.
func identityKey(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			for _, bad := range []string{"principal", "session", "token", "user"} {
				if len(k) >= len(bad) && containsFold(k, bad) {
					return k
				}
			}
			if key := identityKey(child); key != "" {
				return key
			}
		}
	case []any:
		for _, child := range x {
			if key := identityKey(child); key != "" {
				return key
			}
		}
	}
	return ""
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c, d := s[i+j], sub[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
