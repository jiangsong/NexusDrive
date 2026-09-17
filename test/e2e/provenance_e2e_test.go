package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The provenance chain of docs/agent-first-design.md §10.3 (P1 acceptance,
// link a), on a real mount: a terminal write reaches the change record as
// a kernel change within the recorder's flush, history and pull_events
// return it, and stat names the terminal as the last writer; an agent's
// write names the agent's session.

type e2eLastWriter struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	At        string `json:"at"`
}

type e2eStat struct {
	Path       string         `json:"path"`
	LastWriter *e2eLastWriter `json:"last_writer"`
}

type e2eHistory struct {
	Entries []struct {
		Path      string `json:"path"`
		Kind      string `json:"kind"`
		Origin    string `json:"origin"`
		SessionID string `json:"session_id"`
		Reliable  bool   `json:"reliable"`
	} `json:"entries"`
}

type e2eEvents struct {
	Events []struct {
		Path   string `json:"path"`
		Origin string `json:"origin"`
		Kind   string `json:"kind"`
	} `json:"events"`
	Cursor string `json:"cursor"`
}

// TestKernelWriteLandsInChanges: echo > /mnt/x.md → history says origin
// kernel within 500 ms of the write settling → pull_events returns the
// event → stat carries last_writer.origin kernel with no session.
func TestKernelWriteLandsInChanges(t *testing.T) {
	s := newStack(t, "writeback")
	// Establish the session first, so pull_events has a start to be after.
	s.callTool(t, "list_roots", map[string]any{}, nil)
	if err := os.WriteFile(filepath.Join(s.dir, "x.md"), []byte("# from the terminal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)
	var hist e2eHistory
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res := s.callTool(t, "history", map[string]any{"path": "/x.md"}, &hist); res.IsError {
			t.Fatal(toolText(res))
		}
		if len(hist.Entries) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(hist.Entries) == 0 || hist.Entries[0].Origin != "kernel" || hist.Entries[0].SessionID != "" || !hist.Entries[0].Reliable {
		t.Fatalf("history after a terminal write: %+v", hist.Entries)
	}
	var ev e2eEvents
	if res := s.callTool(t, "pull_events", map[string]any{}, &ev); res.IsError {
		t.Fatal(toolText(res))
	}
	found := false
	for _, e := range ev.Events {
		found = found || (e.Path == "/x.md" && e.Origin == "kernel")
	}
	if !found {
		t.Fatalf("pull_events did not return the terminal write: %+v", ev.Events)
	}
	var st e2eStat
	if res := s.callTool(t, "stat", map[string]any{"path": "/x.md"}, &st); res.IsError {
		t.Fatal(toolText(res))
	}
	if st.LastWriter == nil || st.LastWriter.Origin != "kernel" || st.LastWriter.SessionID != "" {
		t.Fatalf("stat.last_writer after a terminal write: %+v", st.LastWriter)
	}
}

// TestStatCarriesLastWriterAfterMCPWrite: an agent's write_file, then stat
// names origin mcp and the agent's own session; the terminal sees the same
// bytes, and history lists the agent's write first.
func TestStatCarriesLastWriterAfterMCPWrite(t *testing.T) {
	s := newStack(t, "writeback")
	if res := s.callTool(t, "write_file", map[string]any{"path": "/agent.md", "content": "# from the agent\n"}, nil); res.IsError {
		t.Fatal(toolText(res))
	}
	var sessions struct {
		Sessions []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"sessions"`
	}
	if res := s.callTool(t, "list_sessions", map[string]any{}, &sessions); res.IsError {
		t.Fatal(toolText(res))
	}
	var mine string
	for _, sess := range sessions.Sessions {
		if sess.State == "active" {
			mine = sess.ID
		}
	}
	if mine == "" {
		t.Fatalf("no active session: %+v", sessions)
	}
	var st e2eStat
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res := s.callTool(t, "stat", map[string]any{"path": "/agent.md"}, &st); res.IsError {
			t.Fatal(toolText(res))
		}
		if st.LastWriter != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.LastWriter == nil || st.LastWriter.Origin != "mcp" || st.LastWriter.SessionID != mine {
		t.Fatalf("stat.last_writer after an agent write: %+v (session %s)", st.LastWriter, mine)
	}
	if data, err := os.ReadFile(filepath.Join(s.dir, "agent.md")); err != nil || string(data) != "# from the agent\n" {
		t.Fatalf("terminal read: %q %v", data, err)
	}
	var hist e2eHistory
	if res := s.callTool(t, "history", map[string]any{"path": "/agent.md"}, &hist); res.IsError {
		t.Fatal(toolText(res))
	}
	if len(hist.Entries) == 0 || hist.Entries[0].Origin != "mcp" || hist.Entries[0].SessionID != mine {
		t.Fatalf("history after an agent write: %+v", hist.Entries)
	}
}
