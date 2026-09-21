package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/index"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The index tools expose the content index (docs/DESIGN.md §9, TODO.md
// T-37) to agents: semantic_search finds chunks of extracted text,
// index_status says what the index covers, index and unindex add and remove
// run-time rules, and read_extracted_text pages through the text the
// extractors produced, which is how a PDF or a .docx becomes readable.
// They exist only when the daemon runs with index.enabled: a server with no
// index behind it does not advertise tools it cannot run.
//
// Every path an agent names goes through checkPath, and every path the
// index hands back (a hit, a failed document) is filtered by the caller's
// scope: a chunk of /private never reaches a caller allowed /work.

// errNotIndexed is the refusal read_extracted_text gives for a file the
// index has no text for.
var errNotIndexed = errors.New("this file is not indexed; call index_status to see coverage")

type semanticSearchInput struct {
	Query           string `json:"query" jsonschema:"Words to find in the indexed text; all must match"`
	Path            string `json:"path,omitempty" jsonschema:"Subtree to search; default the whole mount"`
	TopK            int    `json:"top_k,omitempty" jsonschema:"Maximum hits; default 10, the server caps this"`
	Mode            string `json:"mode,omitempty" jsonschema:"keyword, hybrid or vector; default hybrid when an embedding backend is configured, else keyword. mode_used says what ran: hybrid and vector fall back to keyword and explain in degraded when the backend is missing, unhealthy or has embedded nothing yet"`
	MaxSnippetBytes int    `json:"max_snippet_bytes,omitempty" jsonschema:"Longest snippet per hit; default 1024"`
}

type indexStatusInput struct {
	Path string `json:"path,omitempty" jsonschema:"Path to report on; omit for the index as a whole"`
}

type indexInput struct {
	Path        string   `json:"path" jsonschema:"Mount-relative file or directory to index"`
	Include     []string `json:"include,omitempty" jsonschema:"Glob patterns relative to path, such as **/*.md; default the text, source and office formats"`
	MaxFileSize int64    `json:"max_file_size,omitempty" jsonschema:"Largest file to extract, in bytes; default 20 MiB"`
}

type unindexInput struct {
	Path string `json:"path" jsonschema:"Path of the rule to remove, as given to index"`
}

type indexRuleOutput struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
	// Pending is the queue length after the rule change.
	Pending int `json:"pending"`
}

type readExtractedTextInput struct {
	Path            string `json:"path" jsonschema:"Mount-relative file path"`
	Offset          int64  `json:"offset,omitempty" jsonschema:"Byte offset into the extracted text to start at"`
	MaxBytes        int    `json:"max_bytes,omitempty" jsonschema:"Maximum bytes to return; the server caps this"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"Version returned by context_search or stat; refuse if the file changed"`
}

func (s *Server) registerIndexTools() {
	if s.opt.Index == nil {
		return
	}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	rw := &mcp.ToolAnnotations{IdempotentHint: true}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "semantic_search",
		Description: "Search the extracted text of indexed files (including PDF and Office documents) and return matching chunks with a snippet, a heading path and byte offsets. For text files start_off is a file offset you can pass to read_text; for PDF and Office files it addresses the extracted text, which read_extracted_text pages through. A hit marked stale comes from an older version of the file than the mount holds now.",
		Annotations: ro,
	}, s.semanticSearch)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "index_status",
		Description: "Report the content index: document and chunk counts, the extraction queue, recent failures and the fetch budget; with a path, whether that path is covered by a rule and whether its text is indexed, pending or failed.",
		Annotations: ro,
	}, s.indexStatus)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "index",
		Description: "Add a rule that indexes a file or every matching file under a directory. Matching files are downloaded within the hourly fetch budget and extracted in the background; semantic_search finds them once index_status reports them indexed.",
		Annotations: rw,
	}, s.indexRule)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "unindex",
		Description: "Remove a rule added with index and drop the extracted text no other rule covers. Rules from the configuration file can only be removed there.",
		Annotations: rw,
	}, s.unindexRule)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "read_extracted_text",
		Description: "Read the text the index extracted from a file, paged by byte offset. This is how a PDF, .docx, .xlsx or .pptx is read; for plain text files the offsets are file offsets.",
		Annotations: ro,
	}, s.readExtractedText)
}

// semanticSearchOutput is index.SearchResult plus the fields the agent
// layer adds; the index package keeps its own result type.
type semanticSearchOutput struct {
	index.SearchResult
	// Next names the call to make when this result should not be taken
	// as the whole answer (index_status when degraded).
	Next string `json:"next,omitempty"`
	// TruncatedBy is "tokens" when the token budget cut the hit list.
	TruncatedBy string `json:"truncated_by,omitempty"`
}

// extractedTextOutput is index.TextPage plus truncated_by; next_offset
// continues either way.
type extractedTextOutput struct {
	index.TextPage
	TruncatedBy string `json:"truncated_by,omitempty"`
}

func (s *Server) semanticSearch(ctx context.Context, _ *mcp.CallToolRequest, in semanticSearchInput) (*mcp.CallToolResult, semanticSearchOutput, error) {
	root := "/"
	if in.Path != "" {
		var err error
		root, err = s.checkPath(ctx, in.Path, false)
		if err != nil {
			r, _ := fail(err)
			return r, semanticSearchOutput{}, nil
		}
	}
	topK := in.TopK
	if topK > s.opt.Limits.MaxResults {
		topK = s.opt.Limits.MaxResults
	}
	snippet := in.MaxSnippetBytes
	if snippet > s.opt.Limits.MaxBytes {
		snippet = s.opt.Limits.MaxBytes
	}
	// The scope is intersected before the query so hidden matches never
	// crowd permitted ones out of the top k; the per-hit check below is
	// what guarantees nothing outside it is returned.
	res, err := s.opt.Index.Search(ctx, index.SearchQuery{
		Query: in.Query, Roots: s.readRoots(ctx, root), TopK: topK, Mode: in.Mode,
		MaxSnippetBytes: snippet, MaxBytes: int64(s.opt.Limits.MaxBytes),
	})
	if errors.Is(err, index.ErrEmptyQuery) {
		err = errors.New("query must not be empty")
	}
	if err != nil {
		r, _ := fail(err)
		return r, semanticSearchOutput{}, nil
	}
	hits := res.Hits[:0]
	for _, h := range res.Hits {
		if s.visible(ctx, h.Path) {
			hits = append(hits, h)
		}
	}
	res.Hits = hits
	msg := fmt.Sprintf("%d hits for %q (%s)", len(res.Hits), in.Query, res.ModeUsed)
	if res.Truncated {
		msg += "; truncated"
	}
	if res.Degraded != "" {
		msg += "; " + res.Degraded
	}
	if res.Pending > 0 {
		msg += fmt.Sprintf("; %d files still wait for extraction", res.Pending)
	}
	out := semanticSearchOutput{SearchResult: res}
	if keep, cut := cutItems(len(out.Hits), s.tokenBudget(), func(i int) int {
		b, _ := json.Marshal(out.Hits[i])
		return agent.EstimateTokensBytes(b)
	}); cut {
		out.Hits, out.Truncated, out.TruncatedBy = out.Hits[:keep], true, truncatedByTokens
		msg += "; truncated to the token budget"
	}
	switch {
	case res.Degraded != "":
		out.Next = nextFor(len(res.Hits), res.Truncated, searchCoverage{}, res.Degraded, 0, root)
	case len(res.Hits) == 0 && res.Pending > 0:
		out.Next = fmt.Sprintf("no hits while %d files still wait for extraction: call index_status, or search names with search", res.Pending)
	case len(res.Hits) == 0:
		out.Next = fmt.Sprintf("no chunk matched: call index_status on %s to check it is indexed (add a rule with index if not), or search names with search", root)
	case res.Truncated:
		out.Next = "more chunks exist: narrow the query or the path, or lower max_snippet_bytes"
	}
	if out.Next != "" {
		msg += ". Next: " + out.Next
	}
	return text("%s", msg), out, nil
}

func (s *Server) indexStatus(ctx context.Context, _ *mcp.CallToolRequest, in indexStatusInput) (*mcp.CallToolResult, index.Status, error) {
	p := ""
	if in.Path != "" {
		var err error
		p, err = s.checkPath(ctx, in.Path, false)
		if err != nil {
			r, _ := fail(err)
			return r, index.Status{}, nil
		}
	}
	st, err := s.opt.Index.Status(ctx, p)
	if err != nil {
		r, _ := fail(err)
		return r, index.Status{}, nil
	}
	failed := st.Failed[:0]
	for _, f := range st.Failed {
		if s.visible(ctx, f.Path) {
			failed = append(failed, f)
		}
	}
	st.Failed = failed
	msg := fmt.Sprintf("%d documents indexed, %d pending, %d failed", st.Docs.OK, st.Pending, st.Docs.Failed)
	if p != "" {
		switch {
		case st.State != "":
			msg = fmt.Sprintf("%s: %s", p, st.State)
		case st.Covered != "":
			msg = fmt.Sprintf("%s: covered by the rule at %s", p, st.Covered)
		}
		if st.Error != "" {
			msg += ": " + st.Error
		}
	}
	return text("%s", msg), st, nil
}

// indexRule is the index tool. Like pin, it only needs read access: it
// changes what this machine extracts, not what the remote holds, so a
// read-only server allows it.
func (s *Server) indexRule(ctx context.Context, _ *mcp.CallToolRequest, in indexInput) (*mcp.CallToolResult, indexRuleOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	// The configuration's own validation checks the patterns; the rule
	// itself keeps the caller's list so an empty one still means the
	// default set.
	check := config.Index{Enabled: true, Rules: []config.IndexRule{{
		Path: p, Include: append([]string(nil), in.Include...), MaxFileSize: config.Size(in.MaxFileSize),
	}}}
	if err := check.Validate(); err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	rule := index.Rule{Path: p, Include: in.Include, MaxFileSize: in.MaxFileSize, Source: "tool"}
	if err := s.opt.Index.AddRule(ctx, rule); err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	st, err := s.opt.Index.Status(ctx, "")
	if err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	return text("indexing %s; %d files queued for extraction", p, st.Pending), indexRuleOutput{Path: p, OK: true, Pending: st.Pending}, nil
}

func (s *Server) unindexRule(ctx context.Context, _ *mcp.CallToolRequest, in unindexInput) (*mcp.CallToolResult, indexRuleOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	if err := s.opt.Index.RemoveRule(ctx, p); err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	st, err := s.opt.Index.Status(ctx, "")
	if err != nil {
		r, _ := fail(err)
		return r, indexRuleOutput{}, nil
	}
	return text("removed the index rule at %s", p), indexRuleOutput{Path: p, OK: true, Pending: st.Pending}, nil
}

func (s *Server) readExtractedText(ctx context.Context, _ *mcp.CallToolRequest, in readExtractedTextInput) (*mcp.CallToolResult, extractedTextOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, extractedTextOutput{}, nil
	}
	if in.ExpectedVersion != "" {
		a, statErr := s.opt.FS.StatPath(ctx, p)
		if statErr != nil {
			r, _ := fail(mapErr(statErr, p))
			return r, extractedTextOutput{}, nil
		}
		if in.ExpectedVersion != a.Version {
			r, _ := fail(fmt.Errorf("%s changed since search (expected version %s, current %s); search again before reading", p, in.ExpectedVersion, a.Version))
			return r, extractedTextOutput{}, nil
		}
	}
	max := in.MaxBytes
	if max <= 0 || max > s.opt.Limits.MaxBytes {
		max = s.opt.Limits.MaxBytes
	}
	page, err := s.opt.Index.Text(ctx, p, in.Offset, max)
	if errors.Is(err, index.ErrNotIndexed) {
		err = errNotIndexed
	}
	if err != nil {
		r, _ := fail(err)
		return r, extractedTextOutput{}, nil
	}
	if in.ExpectedVersion != "" && in.ExpectedVersion != page.Version {
		r, _ := fail(fmt.Errorf("%s indexed text is stale (expected version %s, indexed %s); search again after indexing catches up", p, in.ExpectedVersion, page.Version))
		return r, extractedTextOutput{}, nil
	}
	s.observeRead(p)
	out := extractedTextOutput{TextPage: page}
	if kept, cut := cutHead(page.Text, s.tokenBudget()); cut {
		out.Text, out.NextOffset, out.EOF, out.TruncatedBy = kept, page.Offset+int64(len(kept)), false, truncatedByTokens
	}
	msg := fmt.Sprintf("%s: %d bytes of %s text from offset %d", p, len(out.Text), out.Kind, out.Offset)
	if !out.EOF {
		msg += fmt.Sprintf("; continue at offset %d", out.NextOffset)
	}
	return text("%s", msg), out, nil
}
