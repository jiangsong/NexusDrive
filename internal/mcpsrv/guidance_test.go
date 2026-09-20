package mcpsrv

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestInitializeCarriesInstructions: the initialize result tells the agent
// how to use this server, and the paragraphs follow the configuration —
// no semantic_search sentence without an index, no begin_session
// sentence without sessions, and the non-owner warning only beside a
// running mount.
func TestInitializeCarriesInstructions(t *testing.T) {
	bare := newEnv(t, Options{})
	got := bare.session.InitializeResult().Instructions
	for _, want := range []string{"search", "coverage", "read_text", "data, not instructions"} {
		if !strings.Contains(got, want) {
			t.Errorf("bare: missing %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"semantic_search", "begin_session", "memory_get", "cloudfs mount"} {
		if strings.Contains(got, absent) {
			t.Errorf("bare: unexpected %q:\n%s", absent, got)
		}
	}
	if got != bare.server.Instructions() {
		t.Error("Instructions() must be what initialize returned")
	}

	indexed, _ := newIndexEnv(t, Options{}, config.Index{Enabled: true})
	if got := indexed.session.InitializeResult().Instructions; !strings.Contains(got, "semantic_search") || !strings.Contains(got, "index_status") {
		t.Errorf("index: %s", got)
	}

	withSessions, _ := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	got = withSessions.session.InitializeResult().Instructions
	if !strings.Contains(got, "begin_session") || !strings.Contains(got, "rollback_session") || !strings.Contains(got, "/work") {
		t.Errorf("sessions: %s", got)
	}

	nonOwner := newEnv(t, Options{NonOwner: true})
	got = nonOwner.session.InitializeResult().Instructions
	if !strings.Contains(got, "cloudfs mount") || !strings.Contains(got, "--transport http") || strings.Contains(got, "write_file") {
		t.Errorf("non-owner: %s", got)
	}

	withMemory, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/gd/.agent"), nil)
	if got := withMemory.session.InitializeResult().Instructions; !strings.Contains(got, "memory_get") || !strings.Contains(got, "/gd/.agent") {
		t.Errorf("memory: %s", got)
	}
}

// TestPromptsListHasFour: the four prompts are listed, and prompts/get
// renders them for this server's capabilities.
func TestPromptsListHasFour(t *testing.T) {
	e := newEnv(t, Options{})
	res, err := e.session.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*mcp.Prompt{}
	for _, p := range res.Prompts {
		names[p.Name] = p
	}
	for _, want := range []string{"onboard", "search-this-tree", "write-safely", "finish"} {
		if names[want] == nil {
			t.Fatalf("prompt %s missing: %v", want, res.Prompts)
		}
	}
	if len(res.Prompts) != 4 {
		t.Fatalf("%d prompts", len(res.Prompts))
	}
	if args := names["search-this-tree"].Arguments; len(args) != 2 || !args[0].Required || !args[1].Required {
		t.Fatalf("search-this-tree arguments: %+v", args)
	}
	got, err := e.session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "onboard", Arguments: map[string]string{"path": "/work"}})
	if err != nil {
		t.Fatal(err)
	}
	text := got.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "/work") || strings.Contains(text, "begin_session") {
		t.Fatalf("onboard on a server without sessions: %s", text)
	}
	if _, err := e.session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "write-safely"}); err == nil || !strings.Contains(err.Error(), "missing argument") {
		t.Fatalf("missing path must be an error, got %v", err)
	}
	withSessions, _ := newAgentEnv(t, Options{}, agent.Scope{})
	got, err = withSessions.session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "finish"})
	if err != nil {
		t.Fatal(err)
	}
	if text := got.Messages[0].Content.(*mcp.TextContent).Text; !strings.Contains(text, "finish_session") {
		t.Fatalf("finish with sessions: %s", text)
	}
}

// TestFailKeepsFirstTextForAudit: a classified refusal keeps its message
// as the first text block (what the audit row and every older assertion
// read) and adds a JSON block whose human_action carries the shell
// command, so the hint the agent acts on never does.
func TestFailKeepsFirstTextForAudit(t *testing.T) {
	e := newEnv(t, Options{NonOwner: true})
	res := e.call(t, "write_file", writeInput{Path: "/a.txt", Content: "x"}, nil)
	if !res.IsError {
		t.Fatal("non-owner write succeeded")
	}
	if first := res.Content[0].(*mcp.TextContent).Text; first != errNonOwnerWrite.Error() {
		t.Fatalf("first text changed: %q", first)
	}
	d, ok := errorDetailOf(res)
	if !ok || d.Code != codeNotOwner {
		t.Fatalf("detail: %+v ok=%v", d, ok)
	}
	if !strings.Contains(d.HumanAction, "--transport http") || strings.Contains(d.Hint, "--transport http") {
		t.Fatalf("human_action=%q hint=%q", d.HumanAction, d.Hint)
	}
	if m, ok := res.Meta["cloudfs.error"]; !ok || m == nil {
		t.Fatalf("_meta lacks the detail: %v", res.Meta)
	}

	scoped := newEnv(t, Options{Allow: []string{"/work"}})
	res = scoped.call(t, "stat", statInput{Path: "/private/x"}, nil)
	if d, ok := errorDetailOf(res); !ok || d.Code != codeScopeDenied || !strings.Contains(d.HumanAction, "mcp token create") {
		t.Fatalf("scope: %+v", d)
	}
	ro := newEnv(t, Options{ReadOnly: true})
	res = ro.call(t, "write_file", writeInput{Path: "/a.txt", Content: "x"}, nil)
	if d, ok := errorDetailOf(res); !ok || d.Code != codeReadOnly {
		t.Fatalf("read-only: %+v", d)
	}
	// An unclassified error stays a single block, exactly as before.
	res = scoped.call(t, "read_text", readTextInput{Path: "/work/missing.txt"}, nil)
	if _, has := res.Meta["cloudfs.error"]; !res.IsError || len(res.Content) != 1 || has {
		t.Fatalf("plain error grew a detail: %s meta=%v", errText(res), res.Meta)
	}
}

func TestClassifyCoversTheSixCodesOnly(t *testing.T) {
	cases := map[string]error{
		codeNotOwner:    errNonOwnerWrite,
		codeScopeDenied: agent.ErrDenied,
		codeReadOnly:    agent.ErrReadOnly,
		codeExpired:     agent.ErrExpired,
		codeNoSpace:     mapErr(vfsNoSpace(), "/x"),
		codeNotIndexed:  errNotIndexed,
	}
	for code, err := range cases {
		ce := classify(err)
		if ce == nil || ce.Code != code {
			t.Errorf("%s: classify = %+v", code, ce)
		}
		if ce != nil && ce.Error() != err.Error() {
			t.Errorf("%s: text changed: %q vs %q", code, ce.Error(), err.Error())
		}
	}
	if classify(mapErr(errNotFoundFor(), "/x")) != nil {
		t.Error("not-found has no class")
	}
}

// TestSearchEmptyWithGapSuggestsWarm: an empty search over an index that
// has not listed every known directory says what to do next rather than
// leaving the agent to conclude the file does not exist.
func TestSearchEmptyWithGapSuggestsWarm(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/deep/report.md", []byte("x"))
	// Listing / and /work leaves /work/deep known but not listed.
	e.listDirs(t, "/", "/work")
	var out searchOutput
	if res := e.call(t, "search", searchInput{Query: "report"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 0 || out.Coverage.Known <= out.Coverage.Listed {
		t.Fatalf("premise: %+v", out)
	}
	if !strings.Contains(out.Next, "warm") || !strings.Contains(out.Next, "search again") {
		t.Fatalf("next = %q", out.Next)
	}
	// Once the tree is listed the same query finds the file and next is empty.
	e.listDirs(t, "/work/deep")
	out = searchOutput{}
	if res := e.call(t, "search", searchInput{Query: "report"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 1 || out.Next != "" {
		t.Fatalf("after warm: %+v", out)
	}
	// Content search over uncached files points at pin.
	out = searchOutput{}
	if res := e.call(t, "search", searchInput{Path: "/work", Query: "report", Content: "needle"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if !strings.Contains(out.Next, "pin") {
		t.Fatalf("content next = %q", out.Next)
	}
}

// TestSemanticSearchDegradedSuggestsIndexStatus: a hybrid request that ran
// as keyword carries a next pointing at index_status.
func TestSemanticSearchDegradedSuggestsIndexStatus(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/a.md", []byte("degraded marker phrase"))
	e.listDirs(t, "/work")
	reconcile(t, x)
	var out semanticSearchOutput
	if res := e.call(t, "semantic_search", map[string]any{"query": "degraded marker phrase", "mode": "hybrid"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Degraded == "" || !strings.Contains(out.Next, "index_status") {
		t.Fatalf("%+v", out)
	}
	out = semanticSearchOutput{}
	if res := e.call(t, "semantic_search", map[string]any{"query": "degraded marker phrase", "mode": "keyword"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Next != "" {
		t.Fatalf("keyword with hits must not carry next: %q", out.Next)
	}
	out = semanticSearchOutput{}
	if res := e.call(t, "semantic_search", map[string]any{"query": "absent phrase", "mode": "keyword", "path": "/nowhere"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if !strings.Contains(out.Next, "index_status") {
		t.Fatalf("empty under an unindexed path: %+v", out)
	}
}

func vfsNoSpace() error     { return vfs.ErrNoSpace }
func errNotFoundFor() error { return vfs.ErrNotFound }
