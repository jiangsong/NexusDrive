package mcpsrv

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// recordChanges runs the changes recorder over the env's VFS for the
// test's lifetime, as the owner daemon does.
func recordChanges(t *testing.T, e *env, st *agent.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); st.RunChangeRecorder(ctx, e.fs) }()
	t.Cleanup(func() { cancel(); <-done })
}

// waitLastWriter polls until the recorder has flushed a row for p.
func waitLastWriter(t *testing.T, st *agent.Store, p string) agent.Change {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, ok, err := st.LastWriter(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("no change recorded for %s", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStatCarriesLastWriter: a write through an MCP session shows up on
// stat as last_writer with that session's id; a write from outside the
// server (the kernel path, here the VFS directly) shows origin kernel-or-
// remote and no session; a file nobody changed since recording began
// has no last_writer at all.
func TestStatCarriesLastWriter(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	recordChanges(t, e, st)
	e.fake.Seed("work/untouched.txt", []byte("x"))
	if res := e.call(t, "write_file", writeInput{Path: "/work/by-agent.txt", Content: "hi"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	sess := currentSession(t, st)
	c := waitLastWriter(t, st, "/work/by-agent.txt")
	if c.Origin != "mcp" || c.SessionID != sess.ID || c.Principal != sess.PrincipalID {
		t.Fatalf("recorded %+v, session %s", c, sess.ID)
	}
	var out statOutput
	res := e.call(t, "stat", statInput{Path: "/work/by-agent.txt"}, &out)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if out.LastWriter == nil || out.LastWriter.Origin != "mcp" || out.LastWriter.SessionID != sess.ID || out.LastWriter.Kind == "" || out.LastWriter.At == "" {
		t.Fatalf("stat last_writer: %+v", out.LastWriter)
	}
	if !strings.Contains(errText(res), "last changed by mcp") {
		t.Fatalf("summary: %s", errText(res))
	}
	out = statOutput{}
	if res := e.call(t, "stat", statInput{Path: "/work/untouched.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	// The listing that found the seeded file is itself a remote change;
	// what matters is that no session is blamed for it.
	if out.LastWriter != nil && (out.LastWriter.Origin != "remote" || out.LastWriter.SessionID != "") {
		t.Fatalf("untouched file has a last_writer: %+v", out.LastWriter)
	}
	// A write that bypasses the server (the kernel would; here the VFS).
	if _, err := e.fs.WriteFile(context.Background(), "/work/outside.txt", []byte("k"), false); err != nil {
		t.Fatal(err)
	}
	c = waitLastWriter(t, st, "/work/outside.txt")
	if c.Origin == "mcp" || c.SessionID != "" {
		t.Fatalf("outside write attributed to the agent: %+v", c)
	}
	// list_directory carries it in full mode and drops it in minimal.
	var list listOutput
	if res := e.call(t, "list_directory", listInput{Path: "/work"}, &list); res.IsError {
		t.Fatal(errText(res))
	}
	found := false
	for _, en := range list.Entries {
		if en.Name == "by-agent.txt" {
			found = en.LastWriter != nil && en.LastWriter.SessionID == sess.ID
		}
	}
	if !found {
		t.Fatalf("listing lacks last_writer: %+v", list.Entries)
	}
	list = listOutput{}
	if res := e.call(t, "list_directory", listInput{Path: "/work", Fields: "minimal"}, &list); res.IsError {
		t.Fatal(errText(res))
	}
	for _, en := range list.Entries {
		if en.LastWriter != nil {
			t.Fatalf("minimal listing carries last_writer: %+v", en)
		}
	}
	var many statManyOutput
	if res := e.call(t, "stat_many", statManyInput{Paths: []string{"/work/by-agent.txt", "/work/untouched.txt"}}, &many); res.IsError {
		t.Fatal(errText(res))
	}
	if many.Results[0].LastWriter == nil || many.Results[0].LastWriter.SessionID != sess.ID || (many.Results[1].LastWriter != nil && many.Results[1].LastWriter.SessionID != "") {
		t.Fatalf("stat_many: %+v", many.Results)
	}
}

// TestHistoryListsChangesNewestFirst: history shows the recorded changes
// of a path (and, for a directory, under it), newest first, marks a
// rescan as unreliable, and hides paths outside the caller's scope.
func TestHistoryListsChangesNewestFirst(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	recordChanges(t, e, st)
	e.fake.Seed("work/.keep", []byte(""))
	if res := e.call(t, "write_file", writeInput{Path: "/work/h.txt", Content: "one"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "write_file", writeInput{Path: "/work/h.txt", Content: "two", Mode: "append"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "move", moveInput{From: "/work/h.txt", To: "/work/moved.txt"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	waitLastWriter(t, st, "/work/moved.txt")
	// Rows outside the scope exist in the table but never reach the agent.
	if _, err := st.RecordChanges(context.Background(), []agent.Change{{Path: "/private/x", Kind: "write", Origin: "kernel", Reliable: true}, {Path: "/", Kind: "rescan", Origin: "remote"}}); err != nil {
		t.Fatal(err)
	}
	var out historyOutput
	if res := e.call(t, "history", historyInput{Path: "/work/moved.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Entries) < 1 || out.Entries[0].Kind != "rename" || out.Entries[0].From != "/work/h.txt" {
		t.Fatalf("file history: %+v", out.Entries)
	}
	out = historyOutput{}
	if res := e.call(t, "history", historyInput{Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	kinds := []string{}
	for _, en := range out.Entries {
		if strings.HasPrefix(en.Path, "/private") {
			t.Fatalf("history leaked %+v", en)
		}
		kinds = append(kinds, en.Kind)
	}
	if kinds[0] != "rename" || !strings.Contains(strings.Join(kinds, ","), "write") {
		t.Fatalf("dir history kinds: %v", kinds)
	}
	for i := 1; i < len(out.Entries); i++ {
		if out.Entries[i].At > out.Entries[i-1].At {
			t.Fatalf("not newest first: %+v", out.Entries)
		}
	}
	out = historyOutput{}
	if res := e.call(t, "history", historyInput{Path: "/work/never.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Entries) != 0 || !strings.Contains(out.Note, "record starts") {
		t.Fatalf("empty history: %+v", out)
	}
	if res := e.call(t, "history", historyInput{Path: "/private/x"}, nil); !res.IsError {
		t.Fatal("history outside the scope answered")
	}
	// Without sessions there is no history tool and no last_writer.
	bare := newEnv(t, Options{})
	if toolNames(t, bare)["history"] {
		t.Fatal("history registered without a store")
	}
}

// TestChangeRowsCarryTheAdapterName: the change feed names the adapter
// (control, webdav) so the table can say who, not just "api".
func TestChangeRowsCarryTheAdapterName(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	recordChanges(t, e, st)
	e.fake.Seed("work/.keep", []byte(""))
	ctx := vfs.WithOrigin(context.Background(), "webdav")
	if _, err := e.fs.WriteFile(ctx, "/work/dav.txt", []byte("d"), false); err != nil {
		t.Fatal(err)
	}
	if c := waitLastWriter(t, st, "/work/dav.txt"); c.Origin != "webdav" {
		t.Fatalf("%+v", c)
	}
}
