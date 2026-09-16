package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// directory_tree (docs/agent-first-design.md §5.3, TODO.md T-47) shows an
// agent the shape of a subtree the way `tree` does, from what meta already
// holds and nothing else: it walks meta.WalkSubtree, never
// FS.ReadDirPagePath, so a tree of any size costs zero provider calls.
// The price is honesty about gaps: a directory meta has never listed is
// shown with listed: false and not descended into, which is exactly what
// search's coverage field counts, so an agent that sees one knows where
// to call list_directory before concluding a file does not exist.

type treeInput struct {
	Path       string `json:"path" jsonschema:"Mount-relative directory to show"`
	Depth      int    `json:"depth,omitempty" jsonschema:"How many levels below path to show; default 3, at most 10"`
	MaxEntries int    `json:"max_entries,omitempty" jsonschema:"Stop after this many entries; default 500, the server caps this"`
	Fields     string `json:"fields,omitempty" jsonschema:"minimal (default) or full: full adds size, mtime, cached and state to every entry"`
}

// treeEntry is one node in depth-first, name order. Depth is 1 for the
// children of path.
type treeEntry struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Depth int    `json:"depth"`
	// Listed, on a directory, says meta holds its complete listing; a
	// false one is a gap that list_directory fills. Files omit it.
	Listed *bool   `json:"listed,omitempty"`
	Size   int64   `json:"size,omitempty"`
	MTime  string  `json:"mtime,omitempty"`
	Cached float64 `json:"cached,omitempty"`
	State  string  `json:"state,omitempty"`
}

type treeOutput struct {
	Path    string      `json:"path"`
	Entries []treeEntry `json:"entries"`
	Files   int         `json:"files"`
	Dirs    int         `json:"dirs"`
	// Unlisted counts the directories shown with listed: false, the gaps
	// under path this tree (and search) cannot see into.
	Unlisted int `json:"unlisted"`
	// Truncated says max_entries or the token budget stopped the walk;
	// narrow depth or path, or list the subtrees of interest.
	Truncated   bool   `json:"truncated"`
	TruncatedBy string `json:"truncated_by,omitempty"`
	Note        string `json:"note,omitempty"`
}

const (
	treeDefaultDepth   = 3
	treeMaxDepth       = 10
	treeDefaultEntries = 500
	treeMaxEntries     = 5000
)

// errTreeFull stops the walk at max_entries.
var errTreeFull = errors.New("tree full")

func (s *Server) registerTreeTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "directory_tree",
		Description: "Show the subtree under a directory from the local metadata cache, depth-first, without any provider call. " +
			"Directories the cache has never listed appear with listed: false and are not descended into; call list_directory on them to fill the gap. " +
			"Use it to orient yourself in a large tree before searching or listing page by page.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.directoryTree)
}

func (s *Server) directoryTree(ctx context.Context, _ *mcp.CallToolRequest, in treeInput) (*mcp.CallToolResult, treeOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, treeOutput{}, nil
	}
	full := false
	switch in.Fields {
	case "", "minimal":
	case "full":
		full = true
	default:
		r, _ := fail(fmt.Errorf("fields must be minimal or full, not %q", in.Fields))
		return r, treeOutput{}, nil
	}
	depth := in.Depth
	if depth <= 0 {
		depth = treeDefaultDepth
	}
	if depth > treeMaxDepth {
		depth = treeMaxDepth
	}
	max := in.MaxEntries
	if max <= 0 {
		max = treeDefaultEntries
	}
	if max > treeMaxEntries {
		max = treeMaxEntries
	}
	root, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, treeOutput{}, nil
	}
	if !root.IsDir {
		r, _ := fail(fmt.Errorf("%s is not a directory", p))
		return r, treeOutput{}, nil
	}
	store := s.opt.FS.Meta()
	out := treeOutput{Path: p, Entries: []treeEntry{}}
	rootState, err := store.DirState(ctx, root.Ino)
	if err != nil {
		r, _ := fail(err)
		return r, treeOutput{}, nil
	}
	if !rootState.Complete {
		out.Note = fmt.Sprintf("%s itself has not been listed; call list_directory on it first", p)
	}
	base := strings.TrimSuffix(p, "/")
	err = store.WalkSubtree(ctx, root.Ino, p, func(n meta.Node, np string) error {
		if !s.visible(ctx, np) {
			return meta.SkipDir
		}
		d := strings.Count(strings.TrimPrefix(np, base), "/")
		if len(out.Entries) >= max {
			return errTreeFull
		}
		e := treeEntry{Path: np, Depth: d, Kind: "file"}
		if n.IsDir() {
			e.Kind = "directory"
			st, err := store.DirState(ctx, n.Ino)
			if err != nil {
				return err
			}
			e.Listed = ptr(st.Complete)
			out.Dirs++
			if !st.Complete {
				out.Unlisted++
			}
		} else {
			out.Files++
		}
		if full {
			e.Size = n.Size
			if !n.MTime.IsZero() && n.MTime.Unix() > 0 {
				e.MTime = n.MTime.UTC().Format(time.RFC3339)
			}
			if !n.IsDir() {
				e.State = "synced"
				if vfs.IsLocalOnly(n.RemoteID) {
					e.State = "local"
				}
				if n.RemoteID != "" {
					if have, total := s.opt.FS.Cache().Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}); total > 0 {
						e.Cached = float64(have) / float64(total)
					} else if n.Size == 0 {
						e.Cached = 1
					}
				}
			}
		}
		out.Entries = append(out.Entries, e)
		// A directory at the depth limit, or one meta never listed, is
		// shown but not entered: there is nothing below it we could show
		// without asking the provider, which this tool never does.
		if n.Kind == provider.KindDir && (d >= depth || !*e.Listed) {
			return meta.SkipDir
		}
		return nil
	})
	switch {
	case errors.Is(err, errTreeFull):
		out.Truncated = true
		out.Note = joinNote(out.Note, fmt.Sprintf("stopped at %d entries; lower depth, narrow path, or list the subtrees of interest", max))
	case err != nil:
		r, _ := fail(err)
		return r, treeOutput{}, nil
	}
	if keep, cut := cutItems(len(out.Entries), s.tokenBudget(), func(i int) int { return 4 + len(out.Entries[i].Path)/3 }); cut {
		out.Entries, out.Truncated, out.TruncatedBy = out.Entries[:keep], true, truncatedByTokens
		out.Note = joinNote(out.Note, "cut to the token budget; lower depth or narrow path")
	}
	if out.Unlisted > 0 {
		out.Note = joinNote(out.Note, fmt.Sprintf("%d directories have never been listed (listed: false); search cannot see under them until list_directory does", out.Unlisted))
	}
	msg := fmt.Sprintf("%s: %d files, %d directories to depth %d", p, out.Files, out.Dirs, depth)
	if out.Note != "" {
		msg += ". " + out.Note
	}
	return text("%s", msg), out, nil
}

func joinNote(have, add string) string {
	if have == "" {
		return add
	}
	return have + "; " + add
}
