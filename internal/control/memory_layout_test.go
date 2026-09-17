package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/memory"
)

// TestMemoryRoutesMigrateAndReadLayoutV2 (T-56, ui-plan G9): the agents
// listing says which layout the drive is in and who this machine's
// owner is; POST /memory/migrate needs the typed confirmation and moves
// every v1 directory under the owner, after which the same routes read
// owner/agent keys — three-segment fact paths — and the v1 paths are
// gone; GET /memory/merge proposes a merge without writing.
func TestMemoryRoutesMigrateAndReadLayoutV2(t *testing.T) {
	_, s, store := memoryControl(t, memoryConfig(), nil)
	first := decode[memory.Fact](t, call(t, s, "PUT", "/memory/codex/style", `{"content":"one\n"}`))
	if first.Path != "/work/.agent/memory/codex/facts/style.md" {
		t.Fatalf("v1 put: %+v", first)
	}
	agents := decode[MemoryAgentsResponse](t, call(t, s, "GET", "/memory/agents", ""))
	if agents.Layout != memory.LayoutV1 || agents.Owner != agent.DefaultOwner() || len(agents.Agents) != 1 || agents.Agents[0].Owner != "" {
		t.Fatalf("v1 agents: %+v", agents)
	}
	if w := call(t, s, "POST", "/memory/migrate", `{"owner":"alice"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirm=true") {
		t.Fatalf("migrate without confirm: %d %s", w.Code, w.Body.String())
	}
	if store.Layout(context.Background()) != memory.LayoutV1 {
		t.Fatal("an unconfirmed migrate changed the layout")
	}
	done := decode[MemoryMigrateResponse](t, call(t, s, "POST", "/memory/migrate", `{"owner":"alice","confirm":true}`))
	if done.Owner != "alice" || len(done.Moved) != 1 || done.Moved[0] != "codex" || done.Layout != memory.LayoutV2 {
		t.Fatalf("migrate: %+v", done)
	}
	again := decode[MemoryMigrateResponse](t, call(t, s, "POST", "/memory/migrate", `{"owner":"alice","confirm":true}`))
	if len(again.Moved) != 0 {
		t.Fatalf("a second migrate moved something: %+v", again)
	}
	agents = decode[MemoryAgentsResponse](t, call(t, s, "GET", "/memory/agents", ""))
	if agents.Layout != memory.LayoutV2 || len(agents.Agents) != 1 || agents.Agents[0].Name != "alice/codex" || agents.Agents[0].Owner != "alice" {
		t.Fatalf("v2 agents: %+v", agents)
	}
	list := decode[MemoryListResponse](t, call(t, s, "GET", "/memory/alice/codex", ""))
	if list.Agent != "alice/codex" || len(list.Facts) != 1 {
		t.Fatalf("v2 list: %+v", list)
	}
	got := decode[memory.Fact](t, call(t, s, "GET", "/memory/alice/codex/style", ""))
	if got.Content != "one\n" || got.Agent != "alice/codex" {
		t.Fatalf("v2 get: %+v", got)
	}
	// The v1 fact path now reads as an owner/agent listing, and finds nothing.
	if l := decode[MemoryListResponse](t, call(t, s, "GET", "/memory/codex/style", "")); len(l.Facts) != 0 {
		t.Fatalf("the v1 path still answers after migration: %+v", l)
	}
	put := decode[memory.Fact](t, call(t, s, "PUT", "/memory/bob/codex/style", `{"content":"bob's\n"}`))
	if put.Path != "/work/.agent/memory/bob/codex/facts/style.md" {
		t.Fatalf("v2 put for another owner: %+v", put)
	}
	if w := call(t, s, "GET", "/memory/merge?agent=alice/codex&name=style", ""); w.Code != 409 {
		t.Fatalf("merge without a conflict copy: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if w := call(t, s, method, "/memory/migrate", `{"confirm":true}`); w.Code != 405 {
			t.Fatalf("%s /memory/migrate: %d", method, w.Code)
		}
	}
	var raw map[string]any
	_ = json.Unmarshal(call(t, s, "GET", "/memory/agents", "").Body.Bytes(), &raw)
	if _, ok := raw["layout"]; !ok {
		t.Fatal("the agents response carries no layout")
	}
	_ = config.Memory{}
}
