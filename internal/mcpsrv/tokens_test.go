package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

// TestReadTextCJKTruncatesByTokens: 256 KiB of Chinese fits the byte
// limit and blows the token budget; the read is cut at the budget with
// truncated_by: tokens, and following next_offset to the end yields the
// file exactly.
func TestReadTextCJKTruncatesByTokens(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxTokens: 20000}})
	// ~85k characters of Chinese and newlines, 256 KiB of UTF-8.
	var b strings.Builder
	for i := 0; b.Len() < 256<<10; i++ {
		b.WriteString("云盘挂载与代理路由的第")
		fmt.Fprintf(&b, "%d", i)
		b.WriteString("段\n")
	}
	want := b.String()
	e.fake.Seed("zh.txt", []byte(want))
	e.fake.Seed("en.txt", []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 8000)))

	var out readTextOutput
	if res := e.call(t, "read_text", readTextInput{Path: "/zh.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if !out.Truncated || out.TruncatedBy != truncatedByTokens || out.NextOffset == 0 {
		t.Fatalf("first page: truncated=%v by=%q next=%d bytes=%d", out.Truncated, out.TruncatedBy, out.NextOffset, out.Bytes)
	}
	if n := agent.EstimateTokens(out.Content); n > 20000 {
		t.Fatalf("page estimates at %d tokens", n)
	}
	if out.Bytes >= 256<<10 {
		t.Fatalf("the byte limit, not the token budget, cut the page: %d bytes", out.Bytes)
	}
	firstPageBytes := out.Bytes
	var got strings.Builder
	got.WriteString(out.Content)
	for pages := 1; out.Truncated; pages++ {
		if pages > 50 {
			t.Fatal("paging never ends")
		}
		next := readTextOutput{}
		if res := e.call(t, "read_text", readTextInput{Path: "/zh.txt", Offset: out.NextOffset}, &next); res.IsError {
			t.Fatal(errText(res))
		}
		if next.Offset != out.NextOffset {
			t.Fatalf("page started at %d, asked for %d", next.Offset, out.NextOffset)
		}
		got.WriteString(next.Content)
		out = next
	}
	if got.String() != want {
		t.Fatalf("pages do not reassemble the file: got %d bytes, want %d", got.Len(), len(want))
	}
	// A tail read keeps its end.
	tail := readTextOutput{}
	if res := e.call(t, "read_text", readTextInput{Path: "/zh.txt", Tail: 100000}, &tail); res.IsError {
		t.Fatal(errText(res))
	}
	if !strings.HasSuffix(want, tail.Content) || tail.TruncatedBy != truncatedByTokens || tail.Offset == 0 {
		t.Fatalf("tail: by=%q offset=%d suffix=%v", tail.TruncatedBy, tail.Offset, strings.HasSuffix(want, tail.Content))
	}
	// 256 KiB is past 20k tokens in any language (the byte default never
	// fit a client's budget); English gets about three times the bytes
	// per page than Chinese does.
	en := readTextOutput{}
	if res := e.call(t, "read_text", readTextInput{Path: "/en.txt"}, &en); res.IsError {
		t.Fatal(errText(res))
	}
	if en.TruncatedBy != truncatedByTokens || en.Bytes <= firstPageBytes {
		t.Fatalf("english: by=%q bytes=%d (chinese page %d)", en.TruncatedBy, en.Bytes, firstPageBytes)
	}
	// No budget: the byte limit alone applies, as before.
	off := newEnv(t, Options{Limits: Limits{MaxTokens: -1}})
	off.fake.Seed("zh.txt", []byte(want))
	out = readTextOutput{}
	if res := off.call(t, "read_text", readTextInput{Path: "/zh.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.TruncatedBy != "" || out.Bytes < 256<<10-3 {
		t.Fatalf("budget off: by=%q bytes=%d", out.TruncatedBy, out.Bytes)
	}
}

// TestAuditRecordsTokensOut: every audit row carries the token estimate
// of its result beside the bytes.
func TestAuditRecordsTokensOut(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("a.txt", []byte(strings.Repeat("中文内容", 500)))
	if res := e.call(t, "read_text", readTextInput{Path: "/a.txt"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Tool: "read_text"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %d", err, len(rows))
	}
	row := rows[0]
	if row.TokensOut < 2000 || row.TokensOut > row.BytesOut || row.Result != "ok" {
		t.Fatalf("tokens_out=%d bytes_out=%d result=%s", row.TokensOut, row.BytesOut, row.Result)
	}
}

// TestOversizeIsObservedNotTruncatedByMiddleware: with a budget no tool
// can meet, the result still reaches the client whole and the audit row
// says oversize — the middleware measures, the tools cut.
func TestOversizeIsObservedNotTruncatedByMiddleware(t *testing.T) {
	e, st := newAgentEnv(t, Options{Limits: Limits{MaxTokens: 1}}, agent.Scope{})
	e.fake.Seed("a.txt", []byte("hello"))
	var out statOutput
	if res := e.call(t, "stat", statInput{Path: "/a.txt"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Path != "/a.txt" || out.Size != 5 {
		t.Fatalf("stat was cut: %+v", out)
	}
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Tool: "stat"})
	if err != nil || len(rows) != 1 || rows[0].Result != "oversize" || rows[0].TokensOut <= 1 {
		t.Fatalf("%v %+v", err, rows)
	}
}

// TestListDirectoryMinimalOmitsCacheFields: fields=minimal drops cached
// and state from the wire, and a page cut by the token budget resumes
// from its cursor without a gap or a repeat.
func TestListDirectoryMinimalOmitsCacheFields(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxTokens: tokenFraming + 120}})
	for i := 0; i < 40; i++ {
		e.fake.Seed(fmt.Sprintf("d/file-with-a-fairly-long-name-%02d.txt", i), []byte("x"))
	}
	res := e.call(t, "list_directory", listInput{Path: "/d", Fields: "minimal"}, nil)
	if res.IsError {
		t.Fatal(errText(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if strings.Contains(string(raw), `"cached"`) || strings.Contains(string(raw), `"state"`) {
		t.Fatalf("minimal listing carries cache fields: %s", raw)
	}
	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 40 {
			t.Fatal("paging never ends")
		}
		var out listOutput
		if res := e.call(t, "list_directory", listInput{Path: "/d", Fields: "minimal", Cursor: cursor}, &out); res.IsError {
			t.Fatal(errText(res))
		}
		if len(out.Entries) == 0 {
			t.Fatal("empty page")
		}
		for _, en := range out.Entries {
			seen = append(seen, en.Name)
		}
		if !out.Truncated {
			break
		}
		if out.TruncatedBy != truncatedByTokens || out.NextCursor == "" {
			t.Fatalf("page %d: by=%q cursor=%q", page, out.TruncatedBy, out.NextCursor)
		}
		cursor = out.NextCursor
	}
	if len(seen) != 40 {
		t.Fatalf("saw %d names: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("names out of order or repeated at %d: %v", i, seen)
		}
	}
	if res := e.call(t, "list_directory", listInput{Path: "/d", Fields: "huge"}, nil); !res.IsError || !strings.Contains(errText(res), "fields") {
		t.Fatalf("bad fields: %s", errText(res))
	}
}

// TestDirectoryTreeCostsNoRemoteCalls: once meta knows a tree, showing
// it costs nothing — the walk never asks the provider, however deep.
func TestDirectoryTreeCostsNoRemoteCalls(t *testing.T) {
	e := newEnv(t, Options{})
	for _, p := range []string{"work/a.md", "work/src/main.go", "work/src/pkg/util.go", "work/src/pkg/deep/x.go", "docs/readme.md"} {
		e.fake.Seed(p, []byte("x"))
	}
	e.listDirs(t, "/", "/work", "/work/src", "/work/src/pkg", "/work/src/pkg/deep", "/docs")
	lists, total := e.fake.Calls("List"), e.fake.TotalCalls()
	var out treeOutput
	if res := e.call(t, "directory_tree", treeInput{Path: "/work", Depth: 10}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if e.fake.Calls("List") != lists || e.fake.TotalCalls() != total {
		t.Fatalf("directory_tree reached the provider: List %d→%d, total %d→%d", lists, e.fake.Calls("List"), total, e.fake.TotalCalls())
	}
	if out.Files != 4 || out.Dirs != 3 || out.Unlisted != 0 || out.Truncated {
		t.Fatalf("%+v", out)
	}
	paths := make([]string, 0, len(out.Entries))
	for _, en := range out.Entries {
		paths = append(paths, fmt.Sprintf("%d:%s", en.Depth, en.Path))
	}
	want := "1:/work/a.md 1:/work/src 2:/work/src/main.go 2:/work/src/pkg 3:/work/src/pkg/deep 4:/work/src/pkg/deep/x.go 3:/work/src/pkg/util.go"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	// Depth bounds the walk; the directory at the limit is shown, not entered.
	out = treeOutput{}
	if res := e.call(t, "directory_tree", treeInput{Path: "/work", Depth: 2}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Files != 2 || out.Dirs != 2 || len(out.Entries) != 4 {
		t.Fatalf("depth 2: %+v", out)
	}
	// max_entries stops the walk and says so.
	out = treeOutput{}
	if res := e.call(t, "directory_tree", treeInput{Path: "/work", Depth: 10, MaxEntries: 3}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Entries) != 3 || !out.Truncated || !strings.Contains(out.Note, "3 entries") {
		t.Fatalf("max_entries: %+v", out)
	}
	// fields=full adds what list_directory would have said.
	out = treeOutput{}
	if res := e.call(t, "directory_tree", treeInput{Path: "/work", Fields: "full"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Entries[0].Size != 1 || out.Entries[0].State != "synced" || out.Entries[0].MTime == "" {
		t.Fatalf("full: %+v", out.Entries[0])
	}
	if e.fake.TotalCalls() != total {
		t.Fatalf("fields=full reached the provider: %d→%d", total, e.fake.TotalCalls())
	}
}

// TestDirectoryTreeMarksUnlistedDirs: a directory meta has never listed
// is shown with listed: false, not entered, and counted, so the agent
// knows where search cannot see; the scope hides what it must.
func TestDirectoryTreeMarksUnlistedDirs(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/seen/a.md", []byte("x"))
	e.fake.Seed("work/unseen/b.md", []byte("x"))
	e.fake.Seed("private/c.md", []byte("x"))
	e.listDirs(t, "/work", "/work/seen")
	before := e.fake.TotalCalls()
	var out treeOutput
	if res := e.call(t, "directory_tree", treeInput{Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if e.fake.TotalCalls() != before {
		t.Fatalf("the tree reached the provider for the unlisted directory: %d→%d", before, e.fake.TotalCalls())
	}
	byPath := map[string]treeEntry{}
	for _, en := range out.Entries {
		byPath[en.Path] = en
	}
	if en := byPath["/work/seen"]; en.Listed == nil || !*en.Listed {
		t.Fatalf("seen: %+v", en)
	}
	if en := byPath["/work/unseen"]; en.Listed == nil || *en.Listed {
		t.Fatalf("unseen: %+v", en)
	}
	if _, leaked := byPath["/work/unseen/b.md"]; leaked || out.Unlisted != 1 || !strings.Contains(out.Note, "never been listed") {
		t.Fatalf("%+v", out)
	}
	if en := byPath["/work/seen/a.md"]; en.Listed != nil {
		t.Fatalf("a file carries listed: %+v", en)
	}
	if res := e.call(t, "directory_tree", treeInput{Path: "/private"}, nil); !res.IsError {
		t.Fatal("a tree outside the scope was shown")
	}
	if res := e.call(t, "directory_tree", treeInput{Path: "/work/seen/a.md"}, nil); !res.IsError || !strings.Contains(errText(res), "not a directory") {
		t.Fatalf("file: %s", errText(res))
	}
}

func TestCutHeadAndTailKeepRuneBoundaries(t *testing.T) {
	s := strings.Repeat("中", 50) + strings.Repeat("a", 40)
	head, cut := cutHead(s, 30)
	if !cut || !strings.HasPrefix(s, head) || agent.EstimateTokens(head) > 30 || len(head) == 0 || len(head)%3 != 0 {
		t.Fatalf("head=%q cut=%v", head, cut)
	}
	tail, cut := cutTail(s, 30)
	if !cut || !strings.HasSuffix(s, tail) || agent.EstimateTokens(tail) > 30 || len(tail) == 0 {
		t.Fatalf("tail=%q cut=%v", tail, cut)
	}
	if got, cut := cutHead(s, 0); cut || got != s {
		t.Fatal("no budget must keep everything")
	}
	if got, cut := cutHead("abc", 100); cut || got != "abc" {
		t.Fatal("under budget must keep everything")
	}
	if keep, cut := cutItems(3, 5, func(int) int { return 10 }); keep != 1 || !cut {
		t.Fatalf("one oversized item is still returned: keep=%d cut=%v", keep, cut)
	}
	if keep, cut := cutItems(3, 25, func(int) int { return 10 }); keep != 2 || !cut {
		t.Fatalf("keep=%d cut=%v", keep, cut)
	}
}
