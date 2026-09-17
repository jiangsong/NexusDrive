package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestControlDeleteIsAudited (docs/agent-first-design.md §6.1): a write
// the console makes reaches the change record with origin control — the
// route runs under controlOrigin, the VFS reports the change with that
// name, the recorder writes the row — so "who deleted this" has an
// answer for the browser as it does for the terminal and the agent.
func TestControlDeleteIsAudited(t *testing.T) {
	f, _ := fsControl(t)
	ctx := context.Background()
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go st.RunChangeRecorder(rctx, f.coll.FS)
	if _, err := f.coll.FS.ReadDirPath(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	if w := call(t, s, "POST", "/fs/mkdir", `{"path":"/docs/made"}`); w.Code != 200 {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", "/fs/delete", `{"path":"/docs/b","confirm":true}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	var rows []agent.Change
	for time.Now().Before(deadline) {
		rows, _, _ = st.Changes(ctx, agent.ChangesQuery{Prefix: "/docs"})
		if len(rows) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	byPath := map[string]agent.Change{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	if c, ok := byPath["/docs/b"]; !ok || c.Origin != "control" || c.Kind != "remove" || !c.Reliable {
		t.Fatalf("the console's delete is not on record as control: %+v", rows)
	}
	if c, ok := byPath["/docs/made"]; !ok || c.Origin != "control" || c.Kind != "mkdir" {
		t.Fatalf("the console's mkdir is not on record as control: %+v", rows)
	}
	last, ok, err := st.LastWriter(ctx, "/docs/b")
	if err != nil || !ok || last.Origin != "control" || last.SessionID != "" {
		t.Fatalf("last_writer: %+v %v %v", last, ok, err)
	}
}
