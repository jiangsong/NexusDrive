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
	// By path and kind: a directory made on a writeback mount is created on
	// the backend afterwards, and that landing is a further, remote-origin
	// change on the same path — the way an upload landing is for a file.
	// The recorder is asynchronous, so wait for both rows, not for a count.
	deadline := time.Now().Add(5 * time.Second)
	var rows []agent.Change
	byPathKind := map[[2]string]agent.Change{}
	for time.Now().Before(deadline) {
		rows, _, _ = st.Changes(ctx, agent.ChangesQuery{Prefix: "/docs"})
		byPathKind = map[[2]string]agent.Change{}
		for _, r := range rows {
			byPathKind[[2]string{r.Path, r.Kind}] = r
		}
		_, madeOK := byPathKind[[2]string{"/docs/made", "mkdir"}]
		_, removedOK := byPathKind[[2]string{"/docs/b", "remove"}]
		if madeOK && removedOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c, ok := byPathKind[[2]string{"/docs/b", "remove"}]; !ok || c.Origin != "control" || !c.Reliable {
		t.Fatalf("the console's delete is not on record as control: %+v", rows)
	}
	if c, ok := byPathKind[[2]string{"/docs/made", "mkdir"}]; !ok || c.Origin != "control" {
		t.Fatalf("the console's mkdir is not on record as control: %+v", rows)
	}
	last, ok, err := st.LastWriter(ctx, "/docs/b")
	if err != nil || !ok || last.Origin != "control" || last.SessionID != "" {
		t.Fatalf("last_writer: %+v %v %v", last, ok, err)
	}
}
