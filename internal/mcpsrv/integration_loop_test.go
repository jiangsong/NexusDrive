package mcpsrv

import (
	"context"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/memory"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the shared-owner workflow through independent MCP connections,
// including explicit candidate acceptance and a project-external delivery.
func TestTwoClientPersonalMemoryAndHandoffLoop(t *testing.T) {
	cfg := memoryConfig("/work/.agent")
	cfg.Layout = memory.LayoutV2
	idx := rulesOn("/work")
	scope := agent.Scope{Read: []string{"/work"}}
	e, _, x := newMemoryEnv(t, Options{Workspace: "/work/.agent"}, &scope, cfg, &idx)
	connect := func(name string) *env {
		ct, st := mcp.NewInMemoryTransports()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = e.server.Run(ctx, st) }()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "test"}, nil)
		session, err := client.Connect(ctx, ct, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close(); cancel() })
		return &env{server: e.server, fs: e.fs, fake: e.fake, up: e.up, j: e.j, session: session}
	}
	codex, claude := connect("codex"), connect("Claude Code")
	if r := codex.call(t, "begin_session", beginSessionInput{Name: "project-a"}, nil); r.IsError {
		t.Fatal(errText(r))
	}
	var candidate memory.Candidate
	if r := codex.call(t, "memory_propose", memoryProposeInput{Name: "project-a-decision", Agent: "personal", Scope: "/work/project-a", Content: "loopneedle confirmed project decision"}, &candidate); r.IsError {
		t.Fatal(errText(r))
	}
	if r := claude.call(t, "memory_get", memoryGetInput{Name: "project-a-decision", Agent: "personal"}, nil); !r.IsError {
		t.Fatal("pending candidate became a fact")
	}
	if r := codex.call(t, "memory_review", memoryReviewInput{ID: candidate.ID, Agent: "personal", Decision: "accept", ExpectedVersion: candidate.Version, Confirm: true}, nil); r.IsError {
		t.Fatal(errText(r))
	}
	if r := codex.call(t, "finish_session", finishSessionInput{Scope: "/work/project-a", Summary: "decision recorded", Handoff: "loopneedle evidence /work/project-a/source.md; next: verify source"}, nil); r.IsError {
		t.Fatal(errText(r))
	}
	reconcile(t, x)
	var out contextSearchOutput
	if r := claude.call(t, "context_search", contextSearchInput{Query: "loopneedle", Path: "/work/project-a", Scope: "/work/project-a"}, &out); r.IsError {
		t.Fatal(errText(r))
	}
	if len(out.Memories) != 1 || len(out.Handoffs) != 1 {
		t.Fatalf("shared retrieval: %+v", out)
	}
	if r := claude.call(t, "context_search", contextSearchInput{Query: "loopneedle", Path: "/work/project-b", Scope: "/work/project-b"}, &out); r.IsError {
		t.Fatal(errText(r))
	}
	if len(out.Memories) != 0 || len(out.Handoffs) != 0 {
		t.Fatalf("crossed project: %+v", out)
	}
}
