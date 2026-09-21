package mcpsrv

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/memory"
)

func TestContextSearchCombinesNamesAndContentWithVersions(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/project-plan.md", []byte("milestone alpha belongs here\n"))
	e.fake.Seed("work/notes.md", []byte("the project milestone is beta\n"))
	e.listDirs(t, "/work")
	reconcile(t, x)

	var out contextSearchOutput
	res := e.call(t, "context_search", contextSearchInput{Query: "project", Path: "/work", Sources: []string{"knowledge"}}, &out)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Knowledge) != 2 {
		t.Fatalf("knowledge = %+v", out.Knowledge)
	}
	seen := map[string]bool{}
	for _, hit := range out.Knowledge {
		if hit.Version == "" {
			t.Fatalf("missing version: %+v", hit)
		}
		seen[hit.Path] = true
	}
	if !seen["/work/project-plan.md"] || !seen["/work/notes.md"] {
		t.Fatalf("missing name or content result: %+v", out.Knowledge)
	}
	if out.Coverage.Indexed != 2 || out.ModeUsed == "" {
		t.Fatalf("coverage = %+v mode=%q", out.Coverage, out.ModeUsed)
	}
}

func TestContextSearchUsesPersonalMemoryScopeAndExpiry(t *testing.T) {
	cfg := memoryConfig("/work/.agent")
	cfg.Layout = memory.LayoutV2
	idx := rulesOn("/work")
	e, _, x := newMemoryEnv(t, Options{}, nil, cfg, &idx)
	puts := []memoryPutInput{
		{Name: "global-rule", Content: "scope-token global", SourcePaths: []string{"/work/reference.md"}},
		{Name: "alpha-rule", Content: "scope-token alpha", Scope: "alpha"},
		{Name: "beta-rule", Content: "scope-token beta", Scope: "beta"},
		{Name: "expired-rule", Content: "scope-token expired", Scope: "alpha", ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		{Name: "personal-rule", Content: "personal-token visible", Agent: "personal"},
	}
	for _, put := range puts {
		if res := e.call(t, "memory_put", put, nil); res.IsError {
			t.Fatalf("put %s: %s", put.Name, errText(res))
		}
	}
	reconcile(t, x)

	var scoped contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "scope-token", Scope: "alpha", Sources: []string{"memory"}}, &scoped); res.IsError {
		t.Fatal(errText(res))
	}
	seen := map[string]bool{}
	for _, hit := range scoped.Memories {
		seen[hit.Name] = true
	}
	if !seen["global-rule"] || !seen["alpha-rule"] || seen["beta-rule"] || seen["expired-rule"] {
		t.Fatalf("scoped memories: %+v", scoped.Memories)
	}
	var global contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "scope-token", Sources: []string{"memory"}}, &global); res.IsError {
		t.Fatal(errText(res))
	}
	if len(global.Memories) != 1 || global.Memories[0].Name != "global-rule" {
		t.Fatalf("unscoped retrieval crossed project boundaries: %+v", global.Memories)
	}
	var personal contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "personal-token", Sources: []string{"memory"}}, &personal); res.IsError {
		t.Fatal(errText(res))
	}
	if len(personal.Memories) != 1 || !strings.HasSuffix(personal.Memories[0].Agent, "/shared") {
		t.Fatalf("personal memory: %+v", personal.Memories)
	}
}

func TestContextSearchRefillsAfterMemoryPolicyFilters(t *testing.T) {
	idx := rulesOn("/work")
	e, _, x := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), &idx)
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, name := range []string{"a-expired", "b-expired", "c-expired"} {
		if res := e.call(t, "memory_put", memoryPutInput{Name: name, Content: strings.Repeat("refill-token ", 20), ExpiresAt: expired}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	if res := e.call(t, "memory_put", memoryPutInput{Name: "z-current", Content: "refill-token current"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	reconcile(t, x)

	var out contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "refill-token", Sources: []string{"memory"}, TopK: 1}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Memories) != 1 || out.Memories[0].Name != "z-current" {
		t.Fatalf("filtered results were not refilled: %+v", out.Memories)
	}
}

func TestContextSearchSuppressesReplacedMemory(t *testing.T) {
	idx := rulesOn("/work")
	e, _, x := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), &idx)
	for _, put := range []memoryPutInput{
		{Name: "old-endpoint", Content: "replacement-token uses v1"},
		{Name: "new-endpoint", Content: "replacement-token uses v2", Replaces: []string{"old-endpoint"}},
	} {
		if res := e.call(t, "memory_put", put, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	reconcile(t, x)
	var out contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "replacement-token", Sources: []string{"memory"}}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Memories) != 1 || out.Memories[0].Name != "new-endpoint" {
		t.Fatalf("replacement result: %+v", out.Memories)
	}
}

func TestContextSearchReturnsSessionHandoffSeparately(t *testing.T) {
	e, _, x := newIndexAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}}, rulesOn("/work"))
	begin(t, e, map[string]any{"name": "handoff retrieval"})
	if res := e.call(t, "finish_session", finishSessionInput{Handoff: "Continue the handoff-token parser work."}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	reconcile(t, x)
	var out contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "handoff-token", Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Handoffs) != 1 || !strings.HasSuffix(out.Handoffs[0].Path, "/handoff.md") || len(out.Knowledge) != 0 {
		t.Fatalf("handoff grouping: %+v", out)
	}
}

func TestContextSearchKeepsMemorySeparateFromKnowledge(t *testing.T) {
	idx := rulesOn("/work")
	e, _, x := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), &idx)
	res := e.call(t, "memory_put", memoryPutInput{Name: "theme", Content: "preferred accent is violet"}, nil)
	if res.IsError {
		t.Fatal(errText(res))
	}
	reconcile(t, x)

	var out contextSearchOutput
	res = e.call(t, "context_search", contextSearchInput{Query: "violet", Path: "/work"}, &out)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Memories) == 0 || out.Memories[0].Name != "theme" {
		t.Fatalf("memory = %+v", out.Memories)
	}
	if len(out.Knowledge) != 0 {
		t.Fatalf("memory leaked into knowledge: %+v", out.Knowledge)
	}
}

func TestContextSearchHidesStaleChunksByDefault(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/note.md", []byte("obsolete-marker\n"))
	e.listDirs(t, "/work")
	reconcile(t, x)
	if res := e.call(t, "write_file", writeInput{Path: "/work/note.md", Content: "new material\n"}, nil); res.IsError {
		t.Fatal(errText(res))
	}

	var hidden contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "obsolete-marker", Path: "/work", Sources: []string{"knowledge"}}, &hidden); res.IsError {
		t.Fatal(errText(res))
	}
	if len(hidden.Knowledge) != 0 || hidden.Coverage.StaleHidden != 1 {
		t.Fatalf("stale result was not hidden: %+v", hidden)
	}
	var shown contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "obsolete-marker", Path: "/work", Sources: []string{"knowledge"}, IncludeStale: true}, &shown); res.IsError {
		t.Fatal(errText(res))
	}
	if len(shown.Knowledge) != 1 || !shown.Knowledge[0].Stale {
		t.Fatalf("stale result was not returned explicitly: %+v", shown.Knowledge)
	}
}

func TestVersionCheckedReadsRejectChangedFiles(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/note.md", []byte("stable marker\n"))
	e.listDirs(t, "/work")
	reconcile(t, x)
	var out contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "stable", Path: "/work", Sources: []string{"knowledge"}}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Knowledge) == 0 || out.Knowledge[0].Version == "" {
		t.Fatalf("search result = %+v", out.Knowledge)
	}
	version := out.Knowledge[0].Version
	if res := e.call(t, "write_file", writeInput{Path: "/work/note.md", Content: "changed\n"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	for _, tc := range []struct {
		tool string
		in   any
	}{
		{"read_text", readTextInput{Path: "/work/note.md", ExpectedVersion: version}},
		{"read_extracted_text", readExtractedTextInput{Path: "/work/note.md", ExpectedVersion: version}},
	} {
		res := e.call(t, tc.tool, tc.in, nil)
		if !res.IsError || !strings.Contains(errText(res), "changed since search") {
			t.Fatalf("%s: IsError=%v %s", tc.tool, res.IsError, errText(res))
		}
	}
}

func TestReadExtractedTextRejectsAnIndexOlderThanTheCurrentFile(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/current-version.md", []byte("old extracted body\n"))
	e.listDirs(t, "/work")
	reconcile(t, x)
	if res := e.call(t, "write_file", writeInput{Path: "/work/current-version.md", Content: "new body awaiting extraction\n"}, nil); res.IsError {
		t.Fatal(errText(res))
	}

	var found contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "current-version", Path: "/work", Sources: []string{"knowledge"}}, &found); res.IsError {
		t.Fatal(errText(res))
	}
	if len(found.Knowledge) != 1 || found.Knowledge[0].Version == "" {
		t.Fatalf("current name hit: %+v", found.Knowledge)
	}
	res := e.call(t, "read_extracted_text", readExtractedTextInput{Path: "/work/current-version.md", ExpectedVersion: found.Knowledge[0].Version}, nil)
	if !res.IsError || !strings.Contains(errText(res), "indexed text is stale") {
		t.Fatalf("stale extracted read: IsError=%v %s", res.IsError, errText(res))
	}
}

func TestContextSearchWorksWithoutContentIndex(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/project-plan.md", []byte("not fetched"))
	e.listDirs(t, "/work")
	var out contextSearchOutput
	if res := e.call(t, "context_search", contextSearchInput{Query: "project", Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Knowledge) != 1 || len(out.Diagnostics) == 0 {
		t.Fatalf("result = %+v", out)
	}
}

func TestContextTokenBudgetIsGlobalAcrossGroups(t *testing.T) {
	hit := contextHit{Path: "/work/large.md", Snippet: strings.Repeat("large context ", 40)}
	b, _ := json.Marshal(hit)
	one := agent.EstimateTokensBytes(b)
	out := contextSearchOutput{
		Knowledge: []contextHit{hit},
		Memories:  []contextHit{hit},
		Handoffs:  []contextHit{hit},
	}
	trimContextTokens(&out, one*2)
	total := 0
	for _, group := range [][]contextHit{out.Knowledge, out.Memories, out.Handoffs} {
		for _, got := range group {
			b, _ := json.Marshal(got)
			total += agent.EstimateTokensBytes(b)
		}
	}
	if total > one*2 || len(out.Knowledge)+len(out.Memories)+len(out.Handoffs) != 2 || !out.Truncated || out.TruncatedBy != truncatedByTokens {
		t.Fatalf("tokens=%d budget=%d groups=%d/%d/%d truncated=%v by=%q", total, one*2, len(out.Knowledge), len(out.Memories), len(out.Handoffs), out.Truncated, out.TruncatedBy)
	}
}
