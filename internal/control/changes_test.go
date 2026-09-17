package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestChangesRouteFollowsCursorAndFiltersByPath: the route pages the
// change record newest first by row id, a directory path covers what is
// under it, a rename's source path is kept so the inspector can say where
// a file came from, a bad cursor is 400, and a daemon without agent.db
// answers enabled false rather than 503 — the inspector shows the line
// only when there is something to show.
func TestChangesRouteFollowsCursorAndFiltersByPath(t *testing.T) {
	f, st, _ := agentFixture(t)
	f.coll.Changes = st
	h := NewServer(f.coll).Handler()
	base := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	if _, err := st.RecordChanges(context.Background(), []agent.Change{
		{TS: base, Path: "/docs/a.md", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: base.Add(time.Second), Path: "/docs/a.md", Kind: "write", Origin: "mcp", SessionID: "s1", Principal: "p1", Reliable: true},
		{TS: base.Add(2 * time.Second), Path: "/other.txt", Kind: "create", Origin: "webdav", Reliable: true},
		{TS: base.Add(3 * time.Second), Path: "/docs/b.md", From: "/docs/a.md", Kind: "rename", Origin: "control"},
	}); err != nil {
		t.Fatal(err)
	}
	get := func(target string) ChangesResponse {
		t.Helper()
		w := uiCallControl(t, h, "GET", target, "")
		var resp ChangesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); w.Code != 200 || err != nil {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
		return resp
	}
	all := get("/changes")
	if !all.Enabled || len(all.Changes) != 4 || all.Changes[0].Kind != "rename" || all.Changes[0].From != "/docs/a.md" || all.NextCursor != "" {
		t.Fatalf("all: %+v", all)
	}
	if all.Changes[0].Reliable || !all.Changes[1].Reliable {
		t.Fatalf("reliable flags: %+v", all.Changes[:2])
	}
	page := get("/changes?path=/docs&limit=2")
	if len(page.Changes) != 2 || page.Changes[0].ID != 4 || page.Changes[1].ID != 2 || page.NextCursor != "2" {
		t.Fatalf("page 1: %+v", page)
	}
	page = get("/changes?path=/docs&limit=2&cursor=" + page.NextCursor)
	if len(page.Changes) != 1 || page.Changes[0].ID != 1 || page.NextCursor != "" {
		t.Fatalf("page 2: %+v", page)
	}
	one := get("/changes?path=/docs/a.md&limit=1")
	if len(one.Changes) != 1 || one.Changes[0].Origin != "control" || one.Changes[0].Path != "/docs/b.md" {
		t.Fatalf("a file's newest row follows its rename: %+v", one)
	}
	if w := uiCallControl(t, h, "GET", "/changes?cursor=abc", ""); w.Code != 400 {
		t.Fatalf("bad cursor: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/changes?path=..%2F..", ""); w.Code == 200 {
		t.Fatalf("path escape: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/changes", "{}"); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	f.coll.Changes = nil
	if off := get("/changes"); off.Enabled || len(off.Changes) != 0 {
		t.Fatalf("without agent.db: %+v", off)
	}
}
