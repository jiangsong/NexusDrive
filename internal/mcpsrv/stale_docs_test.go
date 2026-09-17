package mcpsrv

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestStaleDocsCostsNoRemoteCalls (T-58): a cached document that links
// to a file changed after it is listed with that link; a document whose
// links are all older is not; an uncached document is counted as skipped
// and never downloaded; nothing here touches the provider once the tree
// is listed.
func TestStaleDocsCostsNoRemoteCalls(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Allow: []string{"/work"}}, agent.Scope{Read: []string{"/work"}})
	old := time.Now().Add(-48 * time.Hour)
	e.fake.Seed("work/plan.md", []byte("# Plan\n\nSee [the script](scripts/run.sh) and [notes](notes.md) and [web](https://x.example).\n"))
	e.fake.SetMTime("work/plan.md", old)
	e.fake.Seed("work/scripts/run.sh", []byte("echo new\n"))
	e.fake.Seed("work/notes.md", []byte("older notes\n"))
	e.fake.SetMTime("work/notes.md", old.Add(-time.Hour))
	e.fake.Seed("work/fresh.md", []byte("[run](scripts/run.sh)\n"))
	e.fake.Seed("work/uncached.md", []byte("[run](scripts/run.sh)\n"))
	e.fake.SetMTime("work/uncached.md", old)
	e.listDirs(t, "/work", "/work/scripts")
	for _, p := range []string{"/work/plan.md", "/work/fresh.md", "/work/notes.md"} {
		if err := e.fs.Prefetch(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	calls := e.fake.TotalCalls()
	var out staleDocsOutput
	if res := e.call(t, "stale_docs", staleDocsInput{Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("stale_docs reached the provider: %d calls", e.fake.TotalCalls()-calls)
	}
	if len(out.Docs) != 1 || out.Docs[0].Path != "/work/plan.md" || len(out.Docs[0].Newer) != 1 || out.Docs[0].Newer[0].Target != "/work/scripts/run.sh" {
		t.Fatalf("docs: %+v", out.Docs)
	}
	if out.Skipped != 1 || out.Note == "" {
		t.Fatalf("skipped: %+v", out)
	}
	if res := e.call(t, "stale_docs", staleDocsInput{Path: "/private"}, nil); !res.IsError {
		t.Fatal("a path outside the scope was accepted")
	}
}
