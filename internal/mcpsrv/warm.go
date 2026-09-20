package mcpsrv

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// warm (docs/mcp.md) is the tool behind the advice the rest of this
// server already gives: search reports how many of the known directories
// its index holds, directory_tree marks the ones meta has never listed,
// and both tell the agent that what lies under them stays invisible until
// warm lists them — which until now named a command only a person could
// run. It is the same FS.Warm the `cloudfs warm` command and the control
// plane call, with the same depth, so a person and an agent reading the
// same documentation get the same walk.
//
// It fills the metadata cache only. Downloading contents is pin, and
// saying so in the description is what keeps an agent from reaching for
// the expensive one.

type warmInput struct {
	Path string `json:"path" jsonschema:"Mount-relative directory to list recursively"`
	// Depth mirrors `cloudfs warm <path> [depth]`: omitted is the whole
	// subtree, which is what the CLI does with no depth argument. It is a
	// pointer because 0 is a depth of its own — path and nothing below it —
	// and an omitted argument must not silently mean that.
	Depth *int `json:"depth,omitempty" jsonschema:"Levels below path to list: -1 (the default) walks the whole subtree, 0 lists only path itself, at most 1024"`
}

type warmOutput struct {
	Path string `json:"path"`
	// Depth is the depth that ran, -1 for the whole subtree.
	Depth int `json:"depth"`
	// Directories counts the directories listed, which is also how many
	// provider List round trips this call was worth.
	Directories int `json:"directories"`
}

// warmMaxDepth is the control plane's bound (control.CacheRequest.Validate);
// the tool refuses the same depths the command does.
const warmMaxDepth = 1024

// warmUnlimited is the depth an omitted argument means, as in the CLI.
const warmUnlimited = -1

func (s *Server) registerWarmTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "warm",
		Description: "List every directory under a path into the local metadata cache, so later list_directory, stat and search over that subtree cost nothing and see everything. " +
			"It does not download file contents: pin is the tool for that. " +
			"The cost is one provider List call per directory visited, so name the narrowest path that covers what you need and use depth to bound how far it goes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, s.warm)
}

func (s *Server) warm(ctx context.Context, _ *mcp.CallToolRequest, in warmInput) (*mcp.CallToolResult, warmOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, warmOutput{}, nil
	}
	depth := warmUnlimited
	if in.Depth != nil {
		depth = *in.Depth
	}
	if depth < warmUnlimited || depth > warmMaxDepth {
		r, _ := fail(fmt.Errorf("depth must be -1 (the whole subtree) or 0 to %d, not %d", warmMaxDepth, depth))
		return r, warmOutput{}, nil
	}
	// FS.Warm walks directories and quietly does nothing for anything else,
	// which as a tool result would be a silent no-op an agent has to infer
	// from a count of zero. Say what is wrong instead.
	a, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, warmOutput{}, nil
	}
	if !a.IsDir {
		r, _ := fail(fmt.Errorf("%s is a file; warm lists directories, and pin is what caches a file's contents", p))
		return r, warmOutput{}, nil
	}
	dirs, err := s.opt.FS.Warm(ctx, p, depth)
	if err != nil {
		// The directories listed before the failure are cached all the
		// same, so say how far it got: calling warm again continues from
		// there rather than starting over.
		r, _ := fail(fmt.Errorf("warm stopped after %d directories: %w", dirs, mapErr(err, p)))
		return r, warmOutput{}, nil
	}
	out := warmOutput{Path: p, Depth: depth, Directories: dirs}
	return text("warmed %s: %d directories are now listed locally; list_directory, stat and search over them make no provider call", p, dirs), out, nil
}
