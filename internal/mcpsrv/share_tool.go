package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"cloudfs/internal/agent"
	"cloudfs/internal/share"
	"cloudfs/internal/vfs"
)

// The share tool (docs/agent-first-design.md §8.2, T-55): a public link
// to a file on the drive. The policy is internal/share's — confirm, only
// a synced and fully cached file, a credential scan, one CreateShare —
// and the tool adds the scope check and the console link. It is the one
// tool in the agent-first plan whose success costs a provider call, and
// the only one whose effect cannot be rolled back, which is why confirm
// is not optional.

type shareInput struct {
	Path    string `json:"path" jsonschema:"Mount-relative file to share"`
	Expires string `json:"expires,omitempty" jsonschema:"How long the link lives, as a duration such as 168h; default from share.default_expiry (7 days)"`
	Confirm bool   `json:"confirm" jsonschema:"Must be true: a public link is an exposure that cannot be taken back"`
	Force   bool   `json:"force,omitempty" jsonschema:"Share even when the content looks like it contains credentials (the refusal names the rule and line)"`
	Code    string `json:"code,omitempty" jsonschema:"Extraction code viewers must type, for drives that support one"`
}

type shareOutput struct {
	share.Result
	// ConsoleURL is the console's render page for the same file, for a
	// person on this machine; "" when console links are off.
	ConsoleURL string `json:"console_url,omitempty"`
	Reversible bool   `json:"reversible"`
}

func (s *Server) registerShareTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "share",
		Description: "Create a public link to a file on the drive. Requires confirm=true. The file must have finished uploading and be fully cached (pin it first), " +
			"and its content is checked for credential-looking text before the link is made; force=true overrides that check. The link cannot be revoked from here.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, s.share)
}

func (s *Server) share(ctx context.Context, _ *mcp.CallToolRequest, in shareInput) (*mcp.CallToolResult, shareOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, shareOutput{}, nil
	}
	var expires time.Duration
	if in.Expires != "" {
		d, err := time.ParseDuration(in.Expires)
		if err != nil || d <= 0 {
			r, _ := fail(fmt.Errorf("expires %q: give a positive duration such as 168h", in.Expires))
			return r, shareOutput{}, nil
		}
		expires = d
	}
	if expires == 0 {
		expires = s.opt.ShareExpiry
	}
	res, err := share.Create(ctx, s.opt.FS, share.Request{Path: p, Expires: expires, Confirm: in.Confirm, Force: in.Force, Code: in.Code})
	if err != nil {
		switch {
		case errors.Is(err, share.ErrConfirm), errors.Is(err, share.ErrNotSynced), errors.Is(err, share.ErrNotCached), errors.Is(err, share.ErrCredentials), errors.Is(err, share.ErrUnsupported), errors.Is(err, vfs.ErrIsDir):
			r, _ := fail(err)
			return r, shareOutput{Result: res}, nil
		}
		r, _ := fail(mapErr(err, p))
		return r, shareOutput{}, nil
	}
	out := shareOutput{Result: res, ConsoleURL: agent.ConsoleURL(s.opt.ConsoleURL, p)}
	msg := fmt.Sprintf("shared %s: %s", p, res.URL)
	if res.Code != "" {
		msg += " (code " + res.Code + ")"
	}
	if !res.ExpiresAt.IsZero() {
		msg += " until " + res.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if res.Forced {
		msg += fmt.Sprintf("; shared despite %d credential-looking finding(s)", len(res.Findings))
	}
	return text("%s", msg), out, nil
}
