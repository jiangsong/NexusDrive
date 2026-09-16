package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"syscall"

	"cloudfs/internal/agent"
	"cloudfs/internal/agent/prompttext"
	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run-time guidance (docs/agent-first-design.md §5.1, TODO.md T-46). The
// server tells the agent how to use it in three places an MCP client
// already reads: the instructions field of initialize, the prompts list,
// and a machine-readable error class beside every refusal's text. None of
// it depends on the person having read docs/mcp.md.

// promptCaps derives what the guidance may mention from the options: a
// sentence about a tool that is not registered is worse than none.
func (s *Server) promptCaps() prompttext.Caps {
	c := prompttext.Caps{
		Allow:        append([]string(nil), s.opt.Allow...),
		ReadOnly:     s.opt.ReadOnly,
		Index:        s.opt.Index != nil,
		Memory:       s.opt.Memory != nil,
		Sessions:     s.opt.Sessions != nil,
		Preimages:    s.opt.Sessions != nil && s.opt.Preimages != nil,
		Export:       s.opt.Export != nil,
		NonOwner:     s.opt.NonOwner,
		MaxReadBytes: s.opt.Limits.MaxBytes,
		MaxTokens:    s.opt.Limits.MaxTokens,
	}
	if s.opt.Scope != nil {
		c.Allow, c.ReadOnly = append([]string(nil), s.opt.Scope.Read...), s.opt.Scope.ReadOnly
	}
	if s.opt.Memory != nil {
		c.MemoryRoot = s.opt.Memory.Root()
	}
	return c
}

// Instructions is the text initialize carries. The MCP server always
// renders English: the reader is a model, and the tool names inside are
// English whatever the person's language.
func (s *Server) Instructions() string { return prompttext.Instructions(s.promptCaps(), i18n.EN) }

// registerPrompts lists the four prompts of prompttext. Each renders for
// this server's capabilities, so prompts/get onboard on a server without
// sessions does not tell the agent to call begin_session.
func (s *Server) registerPrompts() {
	for _, p := range prompttext.Prompts() {
		p := p
		prompt := &mcp.Prompt{Name: p.Name, Description: p.Description}
		for _, a := range p.Args {
			required := false
			for _, r := range p.Required {
				required = required || r == a
			}
			prompt.Arguments = append(prompt.Arguments, &mcp.PromptArgument{Name: a, Required: required})
		}
		s.mcp.AddPrompt(prompt, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			var args map[string]string
			if req.Params != nil {
				args = req.Params.Arguments
			}
			text, err := prompttext.Render(p.Name, args, s.promptCaps(), i18n.EN)
			if err != nil {
				return nil, err
			}
			return &mcp.GetPromptResult{
				Description: p.Description,
				Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
			}, nil
		})
	}
}

// codedError is a refusal with a class an agent can branch on and, when
// the fix is something only the person can do, a separate sentence for
// them. Error() is the plain text, unchanged, which is what the audit row
// keeps (auditError, firstText) and what every existing assertion reads.
type codedError struct {
	Code        string
	Hint        string
	HumanAction string
	Err         error
}

func (e *codedError) Error() string { return e.Err.Error() }
func (e *codedError) Unwrap() error { return e.Err }

// Error codes. Only the six classes an agent can act on differently get
// one; every other failure stays a plain message.
const (
	codeNotOwner    = "not_owner"
	codeScopeDenied = "scope_denied"
	codeReadOnly    = "read_only"
	codeExpired     = "expired"
	codeNoSpace     = "no_space"
	codeNotIndexed  = "not_indexed"
)

// classify finds the class of err: an explicit codedError first, then the
// sentinels the scope, the owner fence, the VFS and the index hand back.
// nil means the error has no class.
func classify(err error) *codedError {
	if err == nil {
		return nil
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce
	}
	switch {
	case errors.Is(err, errRequiresOwner):
		return &codedError{Code: codeNotOwner, Err: err,
			Hint:        "this server cannot change files; do not retry, tell the person what you wanted to change",
			HumanAction: "register the HTTP transport: cloudfs mcp install --transport http"}
	case errors.Is(err, agent.ErrDenied):
		return &codedError{Code: codeScopeDenied, Err: err,
			Hint:        "the path is outside this session's scope; list_roots shows what is reachable, do not retry the same path",
			HumanAction: "widen mcp.allow or create a token with the prefix: cloudfs mcp token create --read <prefix>"}
	case errors.Is(err, agent.ErrReadOnly), errors.Is(err, vfs.ErrReadOnly):
		return &codedError{Code: codeReadOnly, Err: err,
			Hint:        "nothing on this path can be changed from here; report what you would change instead",
			HumanAction: "issue a writable token or set mcp.read_only: false"}
	case errors.Is(err, agent.ErrExpired):
		return &codedError{Code: codeExpired, Err: err,
			Hint:        "this access has expired; every further call will be refused",
			HumanAction: "create a new token: cloudfs mcp token create"}
	case errors.Is(err, vfs.ErrNoSpace), errors.Is(err, syscall.ENOSPC):
		return &codedError{Code: codeNoSpace, Err: err,
			Hint:        "the local disk is full; do not retry until the person frees space",
			HumanAction: "free disk space or raise cache.max_size / lower cache.min_free"}
	case errors.Is(err, errNotIndexed), errors.Is(err, index.ErrNotIndexed):
		return &codedError{Code: codeNotIndexed, Err: err,
			Hint: "call index_status on the path; add a rule with index and wait for it to be indexed, or read the file with read_text if it is plain text"}
	}
	return nil
}

// errorDetail is the second content block of a classified refusal.
type errorDetail struct {
	Code        string `json:"code"`
	Hint        string `json:"hint,omitempty"`
	HumanAction string `json:"human_action,omitempty"`
}

// fail builds a tool refusal. The first text block is err's message, as
// it always was. A classified error adds a JSON block with its code, a
// hint for the agent and, separately, what the person has to do, so the
// agent stops repeating a shell command it cannot run. The SDK fills
// StructuredContent with the tool's zero output on every result, which is
// why the detail goes into a content block (and _meta) rather than there.
func fail(err error) (*mcp.CallToolResult, error) {
	res := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
	if ce := classify(err); ce != nil {
		d := errorDetail{Code: ce.Code, Hint: ce.Hint, HumanAction: ce.HumanAction}
		b, _ := json.Marshal(d)
		res.Content = append(res.Content, &mcp.TextContent{Text: string(b)})
		res.Meta = mcp.Meta{"cloudfs.error": d}
	}
	return res, nil
}

// errorDetailOf reads the detail block back out of a refusal, for tests
// and the control plane.
func errorDetailOf(r *mcp.CallToolResult) (errorDetail, bool) {
	if r == nil || len(r.Content) < 2 {
		return errorDetail{}, false
	}
	tc, ok := r.Content[1].(*mcp.TextContent)
	if !ok {
		return errorDetail{}, false
	}
	var d errorDetail
	if err := json.Unmarshal([]byte(tc.Text), &d); err != nil || d.Code == "" {
		return errorDetail{}, false
	}
	return d, true
}

// nextFor is the rule table behind the `next` field of a search result:
// one sentence naming the tool to call when this result is not the whole
// answer, empty when it is (docs/agent-first-design.md §5.1.4).
func nextFor(hits int, truncated bool, coverage searchCoverage, degraded string, contentSkipped int, root string) string {
	switch {
	case degraded != "":
		return "call index_status to see why semantic search degraded; a keyword-only result may miss what you want"
	case hits == 0 && coverage.Known > coverage.Listed:
		return fmt.Sprintf("no match, but the index lists only %d of %d known directories: call directory_tree or list_directory on %s, then search again", coverage.Listed, coverage.Known, root)
	case hits == 0 && contentSkipped > 0:
		return fmt.Sprintf("no match among cached files; %d were not cached: call pin on %s to search their contents", contentSkipped, root)
	case truncated:
		return "more hits exist: narrow the query or the path, or sort to bring what you want to the top"
	}
	return ""
}
