package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/index"
	"cloudfs/internal/memory"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The memory tools (docs/agent-roadmap.md §3.11, TODO.md T-40) give an
// agent a memory that follows it across devices: facts are Markdown files
// under <memory.root>/memory/<agent>/, the drive synchronises them, and
// the content index makes them searchable. Every tool is a thin call into
// memory.Store; what this file adds is the agent's identity (from the
// session's client or token), the scope check on the root, and the owner
// fence on writes.
//
// The root is checked like any path the caller names: a root outside the
// session's scope makes all five tools refuse with one message, so a
// misconfiguration reads the same whichever tool an agent tries first.

type memoryListInput struct {
	Agent  string `json:"agent,omitempty" jsonschema:"Agent whose memory to list; default your own. personal is shared by your agents; shared is drive-wide"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous truncated listing"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum facts to return; the server caps this"`
}

type memoryListOutput struct {
	Agent      string            `json:"agent"`
	Root       string            `json:"root"`
	Facts      []memory.FactMeta `json:"facts"`
	Truncated  bool              `json:"truncated"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type memoryGetInput struct {
	Name  string `json:"name" jsonschema:"Fact name: lower-case letters, digits and dashes"`
	Agent string `json:"agent,omitempty" jsonschema:"Agent whose memory to read; default your own"`
}

type memoryPutInput struct {
	Name                  string   `json:"name" jsonschema:"Fact name: lower-case letters, digits and dashes, at most 64 characters"`
	Content               string   `json:"content" jsonschema:"The fact's Markdown body; the frontmatter is written for you"`
	Agent                 string   `json:"agent,omitempty" jsonschema:"Agent whose memory to write; default your own. personal shares with your agents; shared is drive-wide"`
	Mode                  string   `json:"mode,omitempty" jsonschema:"replace (default) or append"`
	ExpectedVersion       string   `json:"expected_version,omitempty" jsonschema:"Version from memory_get; the put is refused when the fact changed since"`
	ExpectedRemoteVersion string   `json:"expected_remote_version,omitempty" jsonschema:"remote_version from memory_get; the put is refused when another device's write landed on the drive since, and not when only your own write is still uploading"`
	Description           string   `json:"description,omitempty" jsonschema:"One line for MEMORY.md and the frontmatter; kept from the file when omitted"`
	Type                  string   `json:"type,omitempty" jsonschema:"Free-form kind such as preference, project or person; kept from the file when omitted"`
	Scope                 string   `json:"scope,omitempty" jsonschema:"Project or workspace scope; empty keeps the current scope"`
	SourcePaths           []string `json:"source_paths,omitempty" jsonschema:"Evidence paths that support this fact"`
	SourceSession         string   `json:"source_session,omitempty" jsonschema:"Session that produced this fact"`
	Replaces              []string `json:"replaces,omitempty" jsonschema:"Older fact names in the same memory area that this fact supersedes"`
	ExpiresAt             string   `json:"expires_at,omitempty" jsonschema:"RFC 3339 expiry time; expired facts are hidden by context_search"`
}

type memoryPutOutput struct {
	memory.FactMeta
	Version       string `json:"version"`
	RemoteVersion string `json:"remote_version,omitempty"`
	// State is "local" until the upload queue drains, then "synced".
	State string `json:"state"`
}

type memoryMergeInput struct {
	Name     string `json:"name" jsonschema:"Fact with a conflict copy beside it"`
	Agent    string `json:"agent,omitempty" jsonschema:"Agent whose memory; default your own"`
	Conflict string `json:"conflict,omitempty" jsonschema:"Which conflict copy (path or file name) when there are several; default the first"`
	Ancestor string `json:"ancestor,omitempty" jsonschema:"The body both sides started from, when you have it (the content you read before your own memory_put); with it the merge is three-way"`
}

type memoryDeleteInput struct {
	Name    string `json:"name" jsonschema:"Fact to delete"`
	Agent   string `json:"agent,omitempty" jsonschema:"Agent whose memory to change; default your own"`
	Confirm bool   `json:"confirm" jsonschema:"Must be true; the file and its MEMORY.md line are removed"`
}

type memoryDeleteOutput struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
}

type memorySearchInput struct {
	Query         string `json:"query" jsonschema:"Words to find in the facts; all must match"`
	Agent         string `json:"agent,omitempty" jsonschema:"Agent whose memory to search; default your own"`
	IncludeShared *bool  `json:"include_shared,omitempty" jsonschema:"Also search your personal cross-agent area and drive-wide shared memory; default true"`
	TopK          int    `json:"top_k,omitempty" jsonschema:"Maximum hits; default 10, the server caps this"`
	Mode          string `json:"mode,omitempty" jsonschema:"keyword, hybrid or vector, as for semantic_search"`
}

type memoryProposeInput struct {
	ID            string   `json:"id,omitempty" jsonschema:"Stable candidate id for idempotent retries; generated when omitted"`
	Name          string   `json:"name" jsonschema:"Fact name to create if this candidate is accepted"`
	Content       string   `json:"content" jsonschema:"Proposed Markdown body"`
	Agent         string   `json:"agent,omitempty" jsonschema:"Target memory; default your own, personal shares with your agents"`
	Description   string   `json:"description,omitempty"`
	Type          string   `json:"type,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	SourcePaths   []string `json:"source_paths,omitempty"`
	SourceSession string   `json:"source_session,omitempty"`
	Replaces      []string `json:"replaces,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty" jsonschema:"RFC 3339 expiry time"`
}

type memoryCandidatesInput struct {
	Agent  string `json:"agent,omitempty" jsonschema:"Memory whose candidate inbox to list; default your own"`
	Status string `json:"status,omitempty" jsonschema:"pending (default) or reviewed"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous response"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum candidates to return"`
}

type memoryCandidatesOutput struct {
	Agent       string                   `json:"agent"`
	Candidates  []memoryCandidateSummary `json:"candidates"`
	NextCursor  string                   `json:"next_cursor,omitempty"`
	Truncated   bool                     `json:"truncated"`
	TruncatedBy string                   `json:"truncated_by,omitempty"`
}

// memoryCandidateSummary keeps list/propose/review results bounded. The full
// candidate remains an ordinary file at Path and can be paged with read_text.
type memoryCandidateSummary struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Agent           string     `json:"agent"`
	Path            string     `json:"path"`
	Status          string     `json:"status"`
	Decision        string     `json:"decision,omitempty"`
	ProposedAt      time.Time  `json:"proposed_at"`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty"`
	FactVersion     string     `json:"fact_version,omitempty"`
	BaseFactVersion string     `json:"base_fact_version,omitempty"`
	BaseFactAbsent  bool       `json:"base_fact_absent,omitempty"`
	ContentBytes    int        `json:"content_bytes"`
	Version         string     `json:"version"`
}

func summarizeCandidate(c memory.Candidate) memoryCandidateSummary {
	return memoryCandidateSummary{ID: c.ID, Name: c.Name, Agent: c.Agent, Path: c.Path, Status: c.Status,
		Decision: c.Decision, ProposedAt: c.ProposedAt, ReviewedAt: c.ReviewedAt, FactVersion: c.FactVersion,
		BaseFactVersion: c.BaseFactVersion, BaseFactAbsent: c.BaseFactAbsent, ContentBytes: len(c.Content), Version: c.Version}
}

type memoryReviewInput struct {
	ID              string `json:"id" jsonschema:"Candidate id from memory_candidates or memory_propose"`
	Agent           string `json:"agent,omitempty" jsonschema:"Candidate memory; default your own"`
	Decision        string `json:"decision" jsonschema:"accept or reject"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"Candidate version previously reviewed"`
	Confirm         bool   `json:"confirm" jsonschema:"Must be true; accepting writes a durable fact and rejecting closes the candidate"`
}

// memoryRootMessage is the one refusal every memory tool gives when the
// root lies outside the caller's scope.
const memoryRootMessage = "memory root %s is outside the allowed directories of this session; set memory.root inside mcp.allow"

func (s *Server) registerMemoryTools() {
	if s.opt.Memory == nil {
		return
	}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	rw := &mcp.ToolAnnotations{IdempotentHint: true}
	propose := &mcp.ToolAnnotations{IdempotentHint: false}
	destructive := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_list",
		Description: "List the facts in an agent's memory (default your own) with their descriptions, types, update times and conflict copies. Memory lives in files under memory.root, synchronised across devices by the drive.",
		Annotations: ro,
	}, s.memoryList)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_get",
		Description: "Read one fact: its Markdown body, frontmatter, version (pass it to memory_put as expected_version) and the paths of conflict copies another device produced. Read a copy with read_text, merge with memory_put, then delete the copy.",
		Annotations: ro,
	}, s.memoryGet)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_put",
		Description: "Create or update a fact in an agent's memory (default your own): writes facts/<name>.md with its frontmatter and keeps one line for it in MEMORY.md. mode=append adds to the body. expected_version refuses the write when the fact changed since you read it.",
		Annotations: rw,
	}, s.memoryPut)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_delete",
		Description: "Delete a fact and its MEMORY.md line. Requires confirm=true. Conflict copies are left for you to delete explicitly.",
		Annotations: destructive,
	}, s.memoryDelete)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_merge",
		Description: "Propose a merge of a fact with one of its conflict copies (another device wrote it at the same time): lines only one side changed are taken, lines both changed become conflict blocks for you to resolve. Writes nothing; store the result with memory_put using the version and remote_version returned, then delete the copy.",
		Annotations: ro,
	}, s.memoryMerge)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_search",
		Description: "Search the facts of an agent's memory (default your own) and the shared area through the content index; every hit names the agent and fact it belongs to. Needs index.enabled.",
		Annotations: ro,
	}, s.memorySearch)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_propose",
		Description: "Put a possible memory into the candidate inbox without making it retrievable as a durable fact. Use a stable id for idempotent retries; it must later be explicitly reviewed.",
		Annotations: propose,
	}, s.memoryPropose)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_candidates",
		Description: "List bounded summaries of pending memory candidates or reviewed audit records. Read a summary's path with read_text before review; candidate files sync but are excluded from retrieval.",
		Annotations: ro,
	}, s.memoryCandidates)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "memory_review",
		Description: "Accept or reject a memory candidate with confirm=true. Accept writes the durable fact; reject does not. The reviewed record makes retrying the same decision idempotent.",
		Annotations: rw,
	}, s.memoryReview)
}

// memoryAgent decides whose memory a call is about: the explicit agent,
// validated; otherwise the configured override; otherwise the session's
// identity: over HTTP with a token the token's name, over stdio the
// client's name from initialize; normalised to a directory name.
func (s *Server) memoryAgent(ctx context.Context, req *mcp.CallToolRequest, explicit string) (string, error) {
	// The layout may be read from the drive (the marker under the root),
	// which a caller whose scope excludes the root must not cause: the
	// root is checked first, with the message every memory tool gives.
	root := s.opt.Memory.Root()
	if root == "" {
		return "", memory.ErrNoRoot
	}
	if _, err := s.scopeOf(ctx).Check(path.Join(root, "memory"), false); err != nil {
		if errors.Is(err, agent.ErrDenied) {
			return "", fmt.Errorf(memoryRootMessage, root)
		}
		return "", err
	}
	layout := s.opt.Memory.Layout(ctx)
	owner := s.memoryOwner(ctx)
	if explicit != "" {
		k, err := memory.ParseKey(layout, explicit, owner)
		if err != nil {
			return "", err
		}
		return k.String(), nil
	}
	name := memory.NormalizeAgent("")
	switch {
	case s.opt.Agent != "":
		name = s.opt.Agent
	default:
		if sess, ok := agent.FromContext(ctx); ok {
			if sess.Transport == "http-token" && s.opt.Sessions != nil {
				if p, err := s.opt.Sessions.Principal(ctx, sess.PrincipalID); err == nil && p.Kind == "token" {
					name = memory.NormalizeAgent(p.Name)
					break
				}
			}
			if sess.ClientName != "" {
				name = memory.NormalizeAgent(sess.ClientName)
				break
			}
		}
		ss, _ := req.GetSession().(*mcp.ServerSession)
		if info := clientInfoOf(req, ss); info != nil {
			name = memory.NormalizeAgent(info.Name)
		}
	}
	k, err := memory.ParseIdentity(layout, name, owner)
	if err != nil {
		return "", err
	}
	return k.String(), nil
}

// memoryOwner is the person the caller acts for (memory layout v2): the
// session's principal's owner, else this machine's user.
func (s *Server) memoryOwner(ctx context.Context) string {
	if sess, ok := agent.FromContext(ctx); ok && s.opt.Sessions != nil {
		if p, err := s.opt.Sessions.Principal(ctx, sess.PrincipalID); err == nil {
			return agent.OwnerOf(p)
		}
	}
	return agent.DefaultOwner()
}

// memoryDir resolves the agent's memory directory and checks it against
// the caller's scope for reading or writing. An unset root and a root
// outside the scope are configuration problems and say so; a read-only or
// expired scope keeps its own message.
func (s *Server) memoryDir(ctx context.Context, ag string, write bool) (string, error) {
	root := s.opt.Memory.Root()
	if root == "" {
		return "", memory.ErrNoRoot
	}
	dir := s.opt.Memory.AgentDir(ag)
	if _, err := s.checkPath(ctx, dir, write); err != nil {
		if errors.Is(err, agent.ErrDenied) {
			return "", fmt.Errorf(memoryRootMessage, root)
		}
		return "", err
	}
	return dir, nil
}

// memoryErr turns a store error into the message an agent sees.
func memoryErr(err error, p string) error {
	if errors.Is(err, memory.ErrNotFound) || errors.Is(err, memory.ErrBadName) || errors.Is(err, memory.ErrTooLarge) ||
		errors.Is(err, memory.ErrVersionChanged) || errors.Is(err, memory.ErrNoIndex) || errors.Is(err, memory.ErrNoRoot) ||
		errors.Is(err, memory.ErrInvalidCursor) || errors.Is(err, memory.ErrBadMode) {
		return err
	}
	return mapErr(err, p)
}

func (s *Server) memoryList(ctx context.Context, req *mcp.CallToolRequest, in memoryListInput) (*mcp.CallToolResult, memoryListOutput, error) {
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryListOutput{}, nil
	}
	dir, err := s.memoryDir(ctx, ag, false)
	if err != nil {
		r, _ := fail(err)
		return r, memoryListOutput{}, nil
	}
	limit := in.Limit
	if limit <= 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	facts, next, err := s.opt.Memory.List(ctx, ag, in.Cursor, limit)
	if err != nil {
		r, _ := fail(memoryErr(err, dir))
		return r, memoryListOutput{}, nil
	}
	out := memoryListOutput{Agent: ag, Root: s.opt.Memory.Root(), Facts: facts, NextCursor: next, Truncated: next != ""}
	msg := fmt.Sprintf("%s: %d facts under %s", ag, len(facts), dir)
	if out.Truncated {
		msg += fmt.Sprintf(" (more; pass cursor %q for the rest)", next)
	}
	return text("%s", msg), out, nil
}

func (s *Server) memoryGet(ctx context.Context, req *mcp.CallToolRequest, in memoryGetInput) (*mcp.CallToolResult, memory.Fact, error) {
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memory.Fact{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, false); err != nil {
		r, _ := fail(err)
		return r, memory.Fact{}, nil
	}
	f, err := s.opt.Memory.Get(ctx, ag, in.Name)
	if err != nil {
		r, _ := fail(memoryErr(err, s.opt.Memory.FactPath(ag, in.Name)))
		return r, memory.Fact{}, nil
	}
	msg := fmt.Sprintf("%s/%s: %d bytes, version %s", ag, f.Name, len(f.Content), f.Version)
	if len(f.Conflicts) > 0 {
		msg += fmt.Sprintf("; %d conflict copies beside it: read them with read_text, merge with memory_put, then delete them", len(f.Conflicts))
	}
	return text("%s", msg), f, nil
}

func (s *Server) memoryPut(ctx context.Context, req *mcp.CallToolRequest, in memoryPutInput) (*mcp.CallToolResult, memoryPutOutput, error) {
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, memoryPutOutput{}, nil
	}
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryPutOutput{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, true); err != nil {
		r, _ := fail(err)
		return r, memoryPutOutput{}, nil
	}
	if !memory.ValidName(in.Name) {
		r, _ := fail(fmt.Errorf("name %q: %w", in.Name, memory.ErrBadName))
		return r, memoryPutOutput{}, nil
	}
	// The concrete files go on the audit row too, so a session's writes
	// name the fact and not just the directory.
	p := s.opt.Memory.FactPath(ag, in.Name)
	for _, q := range []string{p, s.opt.Memory.IndexPath(ag)} {
		if _, err := s.checkPath(ctx, q, true); err != nil {
			r, _ := fail(err)
			return r, memoryPutOutput{}, nil
		}
	}
	for _, source := range in.SourcePaths {
		if _, err := s.checkPath(ctx, source, false); err != nil {
			r, _ := fail(fmt.Errorf("source path %s: %w", source, err))
			return r, memoryPutOutput{}, nil
		}
	}
	var expiresAt *time.Time
	if in.ExpiresAt != "" {
		at, parseErr := time.Parse(time.RFC3339, in.ExpiresAt)
		if parseErr != nil {
			r, _ := fail(fmt.Errorf("expires_at must be RFC 3339: %w", parseErr))
			return r, memoryPutOutput{}, nil
		}
		expiresAt = &at
	}
	for _, replaced := range in.Replaces {
		if !memory.ValidName(replaced) || replaced == in.Name {
			r, _ := fail(fmt.Errorf("replacement %q must be another valid fact name", replaced))
			return r, memoryPutOutput{}, nil
		}
	}
	f, err := s.opt.Memory.Put(ctx, ag, in.Name, in.Content, memory.PutOptions{
		Mode: in.Mode, ExpectedVersion: in.ExpectedVersion, ExpectedRemoteVersion: in.ExpectedRemoteVersion, Description: in.Description, Type: in.Type,
		Scope: in.Scope, SourcePaths: in.SourcePaths, SourceSession: in.SourceSession, Replaces: in.Replaces, ExpiresAt: expiresAt,
	})
	if err != nil {
		r, _ := fail(memoryErr(err, p))
		return r, memoryPutOutput{}, nil
	}
	out := memoryPutOutput{FactMeta: f.FactMeta, Version: f.Version, RemoteVersion: f.RemoteVersion, State: "local"}
	if a, err := s.opt.FS.StatPath(ctx, f.Path); err == nil && !a.LocalOnly {
		out.State = "synced"
	}
	msg := fmt.Sprintf("wrote %s (%d bytes, version %s)", f.Path, f.Size, f.Version)
	if len(f.Conflicts) > 0 {
		msg += fmt.Sprintf("; %d conflict copies beside it", len(f.Conflicts))
	}
	return text("%s", msg), out, nil
}

func (s *Server) memoryDelete(ctx context.Context, req *mcp.CallToolRequest, in memoryDeleteInput) (*mcp.CallToolResult, memoryDeleteOutput, error) {
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, memoryDeleteOutput{}, nil
	}
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryDeleteOutput{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, true); err != nil {
		r, _ := fail(err)
		return r, memoryDeleteOutput{}, nil
	}
	if !memory.ValidName(in.Name) {
		r, _ := fail(fmt.Errorf("name %q: %w", in.Name, memory.ErrBadName))
		return r, memoryDeleteOutput{}, nil
	}
	p := s.opt.Memory.FactPath(ag, in.Name)
	for _, q := range []string{p, s.opt.Memory.IndexPath(ag)} {
		if _, err := s.checkPath(ctx, q, true); err != nil {
			r, _ := fail(err)
			return r, memoryDeleteOutput{}, nil
		}
	}
	if !in.Confirm {
		r, _ := fail(fmt.Errorf("refusing to delete %s without confirm=true; this also deletes it on the remote", p))
		return r, memoryDeleteOutput{}, nil
	}
	if err := s.opt.Memory.Delete(ctx, ag, in.Name); err != nil {
		r, _ := fail(memoryErr(err, p))
		return r, memoryDeleteOutput{}, nil
	}
	return text("deleted %s and its MEMORY.md line", p), memoryDeleteOutput{Name: in.Name, Agent: ag, Path: p, OK: true}, nil
}

func (s *Server) memoryMerge(ctx context.Context, req *mcp.CallToolRequest, in memoryMergeInput) (*mcp.CallToolResult, memory.MergeResult, error) {
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memory.MergeResult{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, false); err != nil {
		r, _ := fail(err)
		return r, memory.MergeResult{}, nil
	}
	if !memory.ValidName(in.Name) {
		r, _ := fail(fmt.Errorf("name %q: %w", in.Name, memory.ErrBadName))
		return r, memory.MergeResult{}, nil
	}
	res, err := s.opt.Memory.Merge(ctx, ag, in.Name, in.Conflict, in.Ancestor)
	if err != nil {
		if errors.Is(err, memory.ErrNoConflict) {
			r, _ := fail(err)
			return r, memory.MergeResult{}, nil
		}
		r, _ := fail(memoryErr(err, s.opt.Memory.FactPath(ag, in.Name)))
		return r, memory.MergeResult{}, nil
	}
	msg := fmt.Sprintf("merge of %s with %s: %d conflict block(s)", res.Base, res.Theirs, res.Conflicts)
	if res.Clean {
		msg = fmt.Sprintf("clean merge of %s with %s; store it with memory_put (expected_version %s) and delete the copy", res.Base, res.Theirs, res.Version)
	}
	return text("%s", msg), res, nil
}

func (s *Server) memorySearch(ctx context.Context, req *mcp.CallToolRequest, in memorySearchInput) (*mcp.CallToolResult, memory.SearchResult, error) {
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memory.SearchResult{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, false); err != nil {
		r, _ := fail(err)
		return r, memory.SearchResult{}, nil
	}
	if !s.opt.Memory.HasIndex() {
		r, _ := fail(memory.ErrNoIndex)
		return r, memory.SearchResult{}, nil
	}
	topK := in.TopK
	if topK > s.opt.Limits.MaxResults {
		topK = s.opt.Limits.MaxResults
	}
	res, err := s.opt.Memory.Search(ctx, memory.SearchOptions{
		Query: in.Query, Agent: ag, IncludeShared: in.IncludeShared == nil || *in.IncludeShared,
		TopK: topK, Mode: in.Mode, MaxBytes: int64(s.opt.Limits.MaxBytes),
	})
	if errors.Is(err, index.ErrEmptyQuery) {
		err = errors.New("query must not be empty")
	}
	if err != nil {
		r, _ := fail(memoryErr(err, s.opt.Memory.AgentDir(ag)))
		return r, memory.SearchResult{}, nil
	}
	// The shared area may lie outside a scope that contains the agent's
	// own directory; the per-hit check keeps nothing the caller may not
	// read.
	hits := res.Hits[:0]
	for _, h := range res.Hits {
		if s.visible(ctx, h.Path) {
			hits = append(hits, h)
		}
	}
	res.Hits = hits
	msg := fmt.Sprintf("%d hits for %q in %s's memory (%s)", len(res.Hits), in.Query, ag, res.ModeUsed)
	if res.Truncated {
		msg += "; truncated"
	}
	if res.Degraded != "" {
		msg += "; " + res.Degraded
	}
	if res.Pending > 0 {
		msg += fmt.Sprintf("; %d files still wait for extraction", res.Pending)
	}
	return text("%s", msg), res, nil
}

func (s *Server) memoryPropose(ctx context.Context, req *mcp.CallToolRequest, in memoryProposeInput) (*mcp.CallToolResult, memoryCandidateSummary, error) {
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, true); err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	for _, source := range in.SourcePaths {
		if _, err := s.checkPath(ctx, source, false); err != nil {
			r, _ := fail(fmt.Errorf("source path %s: %w", source, err))
			return r, memoryCandidateSummary{}, nil
		}
	}
	var expiresAt *time.Time
	if in.ExpiresAt != "" {
		at, parseErr := time.Parse(time.RFC3339, in.ExpiresAt)
		if parseErr != nil {
			r, _ := fail(fmt.Errorf("expires_at must be RFC 3339: %w", parseErr))
			return r, memoryCandidateSummary{}, nil
		}
		expiresAt = &at
	}
	c, err := s.opt.Memory.Propose(ctx, ag, in.Name, in.Content, memory.CandidateOptions{
		ID: in.ID, Description: in.Description, Type: in.Type, Scope: in.Scope,
		SourcePaths: in.SourcePaths, SourceSession: in.SourceSession, Replaces: in.Replaces, ExpiresAt: expiresAt,
	})
	if err != nil {
		r, _ := fail(memoryErr(err, s.opt.Memory.AgentDir(ag)))
		return r, memoryCandidateSummary{}, nil
	}
	recordCheck(ctx, c.Path, nil)
	return text("proposed memory candidate %s for %s/%s; read %s before review", c.ID, ag, c.Name, c.Path), summarizeCandidate(c), nil
}

func (s *Server) memoryCandidates(ctx context.Context, req *mcp.CallToolRequest, in memoryCandidatesInput) (*mcp.CallToolResult, memoryCandidatesOutput, error) {
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryCandidatesOutput{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, false); err != nil {
		r, _ := fail(err)
		return r, memoryCandidatesOutput{}, nil
	}
	limit := in.Limit
	if limit <= 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	candidates, next, err := s.opt.Memory.Candidates(ctx, ag, in.Status, in.Cursor, limit)
	if err != nil {
		r, _ := fail(memoryErr(err, s.opt.Memory.AgentDir(ag)))
		return r, memoryCandidatesOutput{}, nil
	}
	summaries := make([]memoryCandidateSummary, len(candidates))
	for i, c := range candidates {
		summaries[i] = summarizeCandidate(c)
	}
	out := memoryCandidatesOutput{Agent: ag, Candidates: summaries, NextCursor: next, Truncated: next != ""}
	if keep, cut := cutItems(len(out.Candidates), s.tokenBudget(), func(i int) int {
		b, _ := json.Marshal(out.Candidates[i])
		return agent.EstimateTokensBytes(b)
	}); cut {
		out.Candidates = out.Candidates[:keep]
		out.NextCursor = vfs.DirectoryCursorAfter(path.Base(candidates[keep-1].Path))
		out.Truncated, out.TruncatedBy = true, truncatedByTokens
	}
	return text("%s: %d %s memory candidates", ag, len(out.Candidates), candidateStatus(in.Status)), out, nil
}

func (s *Server) memoryReview(ctx context.Context, req *mcp.CallToolRequest, in memoryReviewInput) (*mcp.CallToolResult, memoryCandidateSummary, error) {
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	ag, err := s.memoryAgent(ctx, req, in.Agent)
	if err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	if _, err := s.memoryDir(ctx, ag, true); err != nil {
		r, _ := fail(err)
		return r, memoryCandidateSummary{}, nil
	}
	if !in.Confirm {
		r, _ := fail(errors.New("refusing to review memory without confirm=true"))
		return r, memoryCandidateSummary{}, nil
	}
	c, err := s.opt.Memory.Review(ctx, ag, in.ID, in.Decision, in.ExpectedVersion)
	if err != nil {
		r, _ := fail(memoryErr(err, s.opt.Memory.AgentDir(ag)))
		return r, memoryCandidateSummary{}, nil
	}
	recordCheck(ctx, c.Path, nil)
	return text("%sed memory candidate %s for %s/%s", in.Decision, c.ID, ag, c.Name), summarizeCandidate(c), nil
}

func candidateStatus(status string) string {
	if status == "" {
		return "pending"
	}
	return status
}
