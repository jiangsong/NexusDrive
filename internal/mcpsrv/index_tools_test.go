package mcpsrv

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/index"
)

var indexToolNames = []string{"semantic_search", "index_status", "index", "unindex", "read_extracted_text"}

func rulesOn(paths ...string) config.Index {
	c := config.Index{Enabled: true}
	for _, p := range paths {
		c.Rules = append(c.Rules, config.IndexRule{Path: p})
	}
	return c
}

func toolNames(t *testing.T, e *env) map[string]bool {
	t.Helper()
	tools, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestIndexToolsAbsentWhenDisabled(t *testing.T) {
	names := toolNames(t, newEnv(t, Options{}))
	for _, n := range indexToolNames {
		if names[n] {
			t.Fatalf("%s registered without an index", n)
		}
	}
	e, _ := newIndexEnv(t, Options{}, config.Index{Enabled: true})
	names = toolNames(t, e)
	for _, n := range indexToolNames {
		if !names[n] {
			t.Fatalf("%s missing with an index", n)
		}
	}
}

func TestSemanticSearchRespectsAllow(t *testing.T) {
	e, x := newIndexEnv(t, Options{Allow: []string{"/work"}}, rulesOn("/"))
	e.fake.Seed("work/a.md", []byte("shared marker phrase"))
	e.fake.Seed("private/b.md", []byte("shared marker phrase"))
	e.listDirs(t, "/work", "/private")
	reconcile(t, x)
	if st, _ := x.Status(context.Background(), ""); st.Docs.OK != 2 {
		t.Fatalf("both files must be indexed before the scope is tested: %+v", st.Docs)
	}
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "marker phrase", "top_k": 10}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	for _, h := range out.Hits {
		if !strings.HasPrefix(h.Path, "/work/") {
			t.Fatalf("leaked %s", h.Path)
		}
	}
	if len(out.Hits) != 1 || out.Hits[0].Path != "/work/a.md" || out.ModeUsed != "keyword" {
		t.Fatalf("%+v", out)
	}
	// Naming the hidden subtree is refused outright, not answered empty.
	res := e.call(t, "semantic_search", map[string]any{"query": "marker phrase", "path": "/private"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("search below /private: %v %s", res.IsError, errText(res))
	}
}

func TestSemanticSearchRespectsTokenScope(t *testing.T) {
	e, _, x := newIndexAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}}, rulesOn("/"))
	e.fake.Seed("work/a.md", []byte("shared marker phrase"))
	e.fake.Seed("private/b.md", []byte("shared marker phrase"))
	e.listDirs(t, "/work", "/private")
	reconcile(t, x)
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "marker phrase"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 1 || out.Hits[0].Path != "/work/a.md" {
		t.Fatalf("%+v", out.Hits)
	}
	// A root the scope does not contain is refused like any other path.
	res := e.call(t, "semantic_search", map[string]any{"query": "marker phrase", "path": "/"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("root above the scope: %v %s", res.IsError, errText(res))
	}
	var st index.Status
	if res := e.call(t, "index_status", map[string]any{}, &st); res.IsError {
		t.Fatal(errText(res))
	}
	if st.Docs.OK != 2 {
		t.Fatalf("%+v", st)
	}
	res = e.call(t, "read_extracted_text", map[string]any{"path": "/private/b.md"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("extracted text outside the scope: %v %s", res.IsError, errText(res))
	}
}

func TestMarkdownHitOffsetFeedsReadText(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/notes"))
	body := "# Plan\r\n\r\nintro line\r\n## Scope\r\nthe needle sentence lives here\r\n"
	if res := e.call(t, "create_directory", map[string]any{"path": "/notes"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "write_file", map[string]any{"path": "/notes/plan.md", "content": body}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	reconcile(t, x)
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "needle sentence"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 1 || out.Hits[0].OffsetKind != "file" || out.Hits[0].Path != "/notes/plan.md" {
		t.Fatalf("%+v", out.Hits)
	}
	hit := out.Hits[0]
	if hit.Heading != "Plan > Scope" {
		t.Fatalf("heading path %q", hit.Heading)
	}
	var rt readTextOutput
	if res := e.call(t, "read_text", map[string]any{"path": "/notes/plan.md", "offset": hit.StartOff, "max_bytes": 16}, &rt); res.IsError {
		t.Fatal(errText(res))
	}
	if rt.Content == "" || !strings.HasPrefix(body[hit.StartOff:], rt.Content) {
		t.Fatalf("read_text at start_off %d gave %q", hit.StartOff, rt.Content)
	}
	// The extracted text of a text file is the file, so the same offset
	// reads the same bytes there.
	var page index.TextPage
	if res := e.call(t, "read_extracted_text", map[string]any{"path": "/notes/plan.md", "offset": hit.StartOff, "max_bytes": 16}, &page); res.IsError {
		t.Fatal(errText(res))
	}
	if page.Text != rt.Content {
		t.Fatalf("extracted %q, file %q", page.Text, rt.Content)
	}
}

// docxWithHeading builds a minimal .docx with one Heading1 paragraph and
// one body paragraph, the way textract's office tests do.
func docxWithHeading(t *testing.T, heading, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`+
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>`+heading+`</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>`+body+`</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDocxHeadingIsReturnedWithTheHit(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/docs"))
	e.fake.Seed("docs/design.docx", docxWithHeading(t, "第二章 设计", "the needle paragraph"))
	e.listDirs(t, "/docs")
	reconcile(t, x)
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "needle paragraph"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 1 {
		t.Fatalf("%+v", out)
	}
	hit := out.Hits[0]
	if hit.Heading != "第二章 设计" || hit.OffsetKind != "text" || hit.Path != "/docs/design.docx" {
		t.Fatalf("%+v", hit)
	}
	// The offsets address the extracted text, which read_extracted_text
	// pages through.
	var page index.TextPage
	if res := e.call(t, "read_extracted_text", map[string]any{"path": "/docs/design.docx", "offset": hit.StartOff}, &page); res.IsError {
		t.Fatal(errText(res))
	}
	if !strings.Contains(page.Text, "the needle paragraph") || page.Kind != "docx" || !page.EOF {
		t.Fatalf("%+v", page)
	}
}

func TestReadExtractedTextPages(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	body := strings.Repeat("0123456789abcdef", 192) // 3 KiB
	e.fake.Seed("work/big.txt", []byte(body))
	e.listDirs(t, "/work")
	reconcile(t, x)
	var got strings.Builder
	var page index.TextPage
	for off, pages := int64(0), 0; ; pages++ {
		if res := e.call(t, "read_extracted_text", map[string]any{"path": "/work/big.txt", "offset": off, "max_bytes": 1024}, &page); res.IsError {
			t.Fatal(errText(res))
		}
		if page.Offset != off || len(page.Text) != 1024 {
			t.Fatalf("page %d: %+v", pages, page)
		}
		got.WriteString(page.Text)
		if page.EOF {
			if pages != 2 || page.NextOffset != int64(len(body)) {
				t.Fatalf("eof after %d pages at %d", pages+1, page.NextOffset)
			}
			break
		}
		if page.NextOffset != off+1024 {
			t.Fatalf("next_offset %d after %d", page.NextOffset, off)
		}
		off = page.NextOffset
	}
	if got.String() != body {
		t.Fatal("pages do not reassemble the text")
	}
	res := e.call(t, "read_extracted_text", map[string]any{"path": "/work/missing.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "not indexed") || !strings.Contains(errText(res), "index_status") {
		t.Fatalf("unindexed file: %v %s", res.IsError, errText(res))
	}
}

func TestUnindexRefusesConfigRules(t *testing.T) {
	e, _ := newIndexEnv(t, Options{}, rulesOn("/work"))
	res := e.call(t, "unindex", map[string]any{"path": "/work"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "configuration file") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
	res = e.call(t, "index", map[string]any{"path": "/work"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "configuration file") {
		t.Fatalf("overriding a config rule: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "index", map[string]any{"path": "/notes"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var st index.Status
	if res := e.call(t, "index_status", map[string]any{"path": "/notes"}, &st); res.IsError {
		t.Fatal(errText(res))
	}
	if st.Covered != "/notes" || st.RuleSource != "tool" {
		t.Fatalf("%+v", st)
	}
	if res := e.call(t, "unindex", map[string]any{"path": "/notes"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	res = e.call(t, "unindex", map[string]any{"path": "/notes"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "no such rule") {
		t.Fatalf("removing twice: %v %s", res.IsError, errText(res))
	}
	res = e.call(t, "index", map[string]any{"path": "/notes", "include": []string{"[bad"}}, nil)
	if !res.IsError || !strings.Contains(errText(res), "pattern") {
		t.Fatalf("bad glob: %v %s", res.IsError, errText(res))
	}
}

func TestIndexToolIsAllowedOnAReadOnlyServer(t *testing.T) {
	e, x := newIndexEnv(t, Options{ReadOnly: true}, config.Index{Enabled: true})
	e.fake.Seed("notes/a.md", []byte("read-only marker"))
	e.listDirs(t, "/notes")
	if res := e.call(t, "index", map[string]any{"path": "/notes"}, nil); res.IsError {
		t.Fatalf("index refused on read-only: %s", errText(res))
	}
	var st index.Status
	if res := e.call(t, "index_status", map[string]any{"path": "/notes/a.md"}, &st); res.IsError {
		t.Fatal(errText(res))
	}
	if st.State != index.StatePending {
		t.Fatalf("queued by the rule: %+v", st)
	}
	reconcile(t, x)
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "read-only marker"}, &out); res.IsError || len(out.Hits) != 1 {
		t.Fatalf("%+v %s", out, errText(res))
	}
	if res := e.call(t, "unindex", map[string]any{"path": "/notes"}, nil); res.IsError {
		t.Fatalf("unindex refused on read-only: %s", errText(res))
	}
}

func TestHybridModeReportsDegraded(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, rulesOn("/work"))
	e.fake.Seed("work/a.md", []byte("hybrid marker"))
	e.listDirs(t, "/work")
	reconcile(t, x)
	for _, mode := range []string{"hybrid", "vector"} {
		var out index.SearchResult
		if res := e.call(t, "semantic_search", map[string]any{"query": "hybrid marker", "mode": mode}, &out); res.IsError {
			t.Fatalf("%s: %s", mode, errText(res))
		}
		if out.ModeUsed != "keyword" || out.Degraded == "" || len(out.Hits) != 1 {
			t.Fatalf("%s: %+v", mode, out)
		}
	}
	res := e.call(t, "semantic_search", map[string]any{"query": "hybrid marker", "mode": "quantum"}, nil)
	if !res.IsError {
		t.Fatal("an unknown mode was accepted")
	}
	res = e.call(t, "semantic_search", map[string]any{"query": "   "}, nil)
	if !res.IsError || !strings.Contains(errText(res), "empty") {
		t.Fatalf("empty query: %v %s", res.IsError, errText(res))
	}
}

func TestSemanticSearchCapsTopKAndFiltersFailures(t *testing.T) {
	e, x := newIndexEnv(t, Options{Allow: []string{"/work"}, Limits: Limits{MaxResults: 2}}, rulesOn("/"))
	for _, n := range []string{"a", "b", "c", "d"} {
		e.fake.Seed("work/"+n+".md", []byte("capped marker "+n))
	}
	e.fake.Seed("private/broken.docx", []byte("not a zip"))
	e.listDirs(t, "/work", "/private")
	reconcile(t, x)
	var out index.SearchResult
	if res := e.call(t, "semantic_search", map[string]any{"query": "capped marker", "top_k": 50}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Hits) != 2 {
		t.Fatalf("top_k was not capped at MaxResults: %d hits", len(out.Hits))
	}
	var st index.Status
	if res := e.call(t, "index_status", map[string]any{}, &st); res.IsError {
		t.Fatal(errText(res))
	}
	if st.Docs.Failed != 1 || len(st.Failed) != 0 {
		t.Fatalf("a failure outside the scope leaked into index_status: %+v", st)
	}
}

func TestIndexStatusChecksItsOptionalPath(t *testing.T) {
	e, _ := newIndexEnv(t, Options{Allow: []string{"/work"}}, rulesOn("/"))
	res := e.call(t, "index_status", map[string]any{"path": "/private"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "index_status", map[string]any{}, nil); res.IsError {
		t.Fatalf("whole-index status: %s", errText(res))
	}
}
