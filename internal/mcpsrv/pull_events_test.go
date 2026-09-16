package mcpsrv

import (
	"context"
	"strconv"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestPullEventsReturnsOthersChangesWithoutTriggerRules: with no trigger
// rule configured at all, a write from outside the session (the kernel
// path; here the VFS directly) comes back from pull_events, while the
// session's own writes do not unless asked for; the cursor continues and
// is remembered for the session.
func TestPullEventsReturnsOthersChangesWithoutTriggerRules(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	recordChanges(t, e, st)
	e.fake.Seed("work/.keep", []byte(""))
	e.fake.Seed("private/.keep", []byte(""))
	// Establish the session before the changes so "since the session
	// started" has something to be after.
	if res := e.call(t, "list_roots", struct{}{}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "write_file", writeInput{Path: "/work/mine.txt", Content: "me"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if _, err := e.fs.WriteFile(context.Background(), "/work/theirs.txt", []byte("k"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(context.Background(), "/private/secret.txt", []byte("k"), false); err != nil {
		t.Fatal(err)
	}
	waitLastWriter(t, st, "/private/secret.txt")

	var out pullEventsOutput
	if res := e.call(t, "pull_events", pullEventsInput{}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	paths := map[string]bool{}
	for _, ev := range out.Events {
		paths[ev.Path] = true
		if ev.SessionID != "" {
			t.Fatalf("own or foreign session leaked by default: %+v", ev)
		}
	}
	if !paths["/work/theirs.txt"] || paths["/work/mine.txt"] || paths["/private/secret.txt"] {
		t.Fatalf("events: %+v", out.Events)
	}
	if out.Cursor == "" || out.Cursor == "0" || out.Rescan {
		t.Fatalf("%+v", out)
	}
	// Nothing new: the stored cursor makes the next call empty.
	next := pullEventsOutput{}
	if res := e.call(t, "pull_events", pullEventsInput{}, &next); res.IsError {
		t.Fatal(errText(res))
	}
	if len(next.Events) != 0 || next.Cursor != out.Cursor {
		t.Fatalf("second pull: %+v", next)
	}
	// include_own from an explicit cursor replays the session's write.
	own := pullEventsOutput{}
	if res := e.call(t, "pull_events", pullEventsInput{Cursor: "0", IncludeOwn: true}, &own); res.IsError {
		t.Fatal(errText(res))
	}
	sess := currentSession(t, st)
	found := false
	for _, ev := range own.Events {
		found = found || (ev.Path == "/work/mine.txt" && ev.SessionID == sess.ID)
	}
	if !found {
		t.Fatalf("include_own: %+v", own.Events)
	}
	// A new change after the pull is what the next default call returns.
	if _, err := e.fs.WriteFile(context.Background(), "/work/later.txt", []byte("l"), false); err != nil {
		t.Fatal(err)
	}
	waitLastWriter(t, st, "/work/later.txt")
	later := pullEventsOutput{}
	if res := e.call(t, "pull_events", pullEventsInput{Kinds: []string{"create", "write"}}, &later); res.IsError {
		t.Fatal(errText(res))
	}
	if len(later.Events) == 0 || later.Events[len(later.Events)-1].Path != "/work/later.txt" {
		t.Fatalf("later: %+v", later.Events)
	}
	if id, _ := st.LastChangeSeen(context.Background(), sess.ID); strconv.FormatInt(id, 10) != later.Cursor {
		t.Fatalf("stored cursor %d, returned %s", id, later.Cursor)
	}
	if res := e.call(t, "pull_events", pullEventsInput{Cursor: "abc"}, nil); !res.IsError {
		t.Fatal("bad cursor accepted")
	}
	if res := e.call(t, "pull_events", pullEventsInput{Path: "/private"}, nil); !res.IsError {
		t.Fatal("path outside the scope accepted")
	}
}

// TestPullEventsFlagsARescan: a rescan row in the range sets rescan and
// the note, and pages continue past it.
func TestPullEventsFlagsARescan(t *testing.T) {
	e, st := newAgentEnv(t, Options{Limits: Limits{MaxResults: 2}}, agent.Scope{})
	if res := e.call(t, "list_roots", struct{}{}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	now := time.Now()
	rows := []agent.Change{
		{TS: now, Path: "/a", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: now, Path: "/", Kind: "rescan", Origin: "remote"},
		{TS: now, Path: "/b", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: now, Path: "/c", Kind: "write", Origin: "kernel", Reliable: true},
	}
	if _, err := st.RecordChanges(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	var out pullEventsOutput
	if res := e.call(t, "pull_events", pullEventsInput{Cursor: "0"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Events) != 2 || !out.Rescan || out.Note == "" || !out.More {
		t.Fatalf("%+v", out)
	}
	rest := pullEventsOutput{}
	if res := e.call(t, "pull_events", pullEventsInput{Cursor: out.Cursor}, &rest); res.IsError {
		t.Fatal(errText(res))
	}
	if len(rest.Events) != 1 || rest.Events[0].Path != "/c" || rest.Rescan || rest.More {
		t.Fatalf("rest: %+v", rest)
	}
	if toolNames(t, newEnv(t, Options{}))["pull_events"] {
		t.Fatal("pull_events registered without a store")
	}
}

// TestPullEventsAndHistoryHideARenameSourceOutsideTheScope: a file
// renamed into the caller's scope from outside it is reported as
// arriving, without the source path the caller may not read; a rename
// within the scope keeps its source.
func TestPullEventsAndHistoryHideARenameSourceOutsideTheScope(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/.keep", []byte(""))
	e.fake.Seed("private/.keep", []byte(""))
	if res := e.call(t, "list_roots", struct{}{}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	now := time.Now()
	if _, err := st.RecordChanges(context.Background(), []agent.Change{
		{TS: now, Path: "/work/arrived.txt", Kind: "rename", From: "/private/secret-name.txt", Origin: "kernel", Reliable: true},
		{TS: now, Path: "/work/moved.txt", Kind: "rename", From: "/work/old.txt", Origin: "kernel", Reliable: true},
	}); err != nil {
		t.Fatal(err)
	}
	var out pullEventsOutput
	if res := e.call(t, "pull_events", pullEventsInput{Cursor: "0"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	from := map[string]string{}
	for _, ev := range out.Events {
		from[ev.Path] = ev.From
	}
	if from["/work/arrived.txt"] != "" || from["/work/moved.txt"] != "/work/old.txt" {
		t.Fatalf("pull_events from: %v", from)
	}
	var hist historyOutput
	if res := e.call(t, "history", historyInput{Path: "/work/arrived.txt"}, &hist); res.IsError {
		t.Fatal(errText(res))
	}
	for _, en := range hist.Entries {
		if en.From != "" {
			t.Fatalf("history leaked the out-of-scope source: %+v", en)
		}
	}
	if len(hist.Entries) == 0 {
		t.Fatal("history has no entry for the arrival")
	}
}
