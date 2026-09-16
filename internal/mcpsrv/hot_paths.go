package mcpsrv

import (
	"context"
	"fmt"
	"path"
	"sort"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hot_paths (docs/agent-first-design.md §6.3, TODO.md T-53) shows an
// agent what is being read — by agents, by programs on the mount, by the
// console — and how long ago each file changed, which is the "hot but
// stale" quadrant: the files everyone relies on that nobody has updated.
// It only suggests: a pin or an index rule downloads, and nothing here
// downloads on its own.

// ReadHeat is what hot_paths reads; agent.Store satisfies it.
type ReadHeat interface {
	HotPaths(ctx context.Context, prefix string, days, limit int) ([]agent.HotPath, error)
}

// ReadObserver is where the tools that read outside the VFS
// (read_extracted_text) report; agent.ReadObserver satisfies it.
type ReadObserver interface {
	Observe(path, kind string)
}

type hotPathsInput struct {
	Path  string `json:"path,omitempty" jsonschema:"Subtree to report on; default everything you may read"`
	Days  int    `json:"days,omitempty" jsonschema:"Window in days; default 7"`
	Limit int    `json:"limit,omitempty" jsonschema:"Paths to return; default 50"`
}

type hotPath struct {
	Path     string         `json:"path"`
	Reads    int64          `json:"reads"`
	ByKind   map[string]int `json:"by_kind"`
	LastRead string         `json:"last_read"`
	// MTime is the file's modification time as the mount knows it; Stale
	// says it was last changed before the window began while being read
	// inside it — the files worth a second look.
	MTime string `json:"mtime,omitempty"`
	Stale bool   `json:"stale"`
}

type hotPathsOutput struct {
	Path  string    `json:"path"`
	Days  int       `json:"days"`
	Paths []hotPath `json:"paths"`
	// Suggestions are things the person might do with what is hot:
	// pin a directory read often but not cached, index one for search.
	// Nothing here does them.
	Suggestions []string `json:"suggestions,omitempty"`
	Note        string   `json:"note,omitempty"`
}

func (s *Server) registerHotPaths() {
	if s.heat == nil {
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "hot_paths",
		Description: "List the most-read paths under a directory over the last days, by kind of reader (agent, kernel, console, webdav), with each file's " +
			"modification time and a stale flag for files read recently but changed before the window. Suggests pins or index rules; makes none.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.hotPaths)
}

func (s *Server) hotPaths(ctx context.Context, _ *mcp.CallToolRequest, in hotPathsInput) (*mcp.CallToolResult, hotPathsOutput, error) {
	root := "/"
	if in.Path != "" {
		var err error
		if root, err = s.checkPath(ctx, in.Path, false); err != nil {
			r, _ := fail(err)
			return r, hotPathsOutput{}, nil
		}
	}
	days := in.Days
	if days <= 0 {
		days = 7
	}
	limit := in.Limit
	if limit <= 0 || limit > s.opt.Limits.MaxResults {
		limit = s.opt.Limits.MaxResults
	}
	// Over-fetch so the scope filter leaves a full page.
	rows, err := s.heat.HotPaths(ctx, root, days, limit*2)
	if err != nil {
		r, _ := fail(err)
		return r, hotPathsOutput{}, nil
	}
	out := hotPathsOutput{Path: root, Days: days, Paths: []hotPath{}}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	uncachedDirs := map[string]int{}
	for _, h := range rows {
		if !s.visible(ctx, h.Path) {
			continue
		}
		hp := hotPath{Path: h.Path, Reads: h.Reads, ByKind: h.ByKind, LastRead: h.LastRead.UTC().Format(time.RFC3339)}
		// meta only: a hot path is one the mount has seen, so this never
		// asks the provider.
		if n, err := s.opt.FS.Meta().Resolve(ctx, h.Path); err == nil {
			if !n.MTime.IsZero() && n.MTime.Unix() > 0 {
				hp.MTime = n.MTime.UTC().Format(time.RFC3339)
				hp.Stale = n.MTime.Before(since)
			}
			if !n.IsDir() {
				if a, err := s.opt.FS.StatPath(ctx, h.Path); err == nil && a.Cached < 1 && !a.Pinned {
					uncachedDirs[path.Dir(h.Path)]++
				}
			}
		}
		out.Paths = append(out.Paths, hp)
		if len(out.Paths) >= limit {
			break
		}
	}
	if keep, cut := cutItems(len(out.Paths), s.tokenBudget(), func(int) int { return 60 }); cut {
		out.Paths = out.Paths[:keep]
	}
	dirs := make([]string, 0, len(uncachedDirs))
	for d := range uncachedDirs {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return uncachedDirs[dirs[i]] > uncachedDirs[dirs[j]] || uncachedDirs[dirs[i]] == uncachedDirs[dirs[j]] && dirs[i] < dirs[j]
	})
	for i, d := range dirs {
		if i >= 3 {
			break
		}
		out.Suggestions = append(out.Suggestions, fmt.Sprintf("pin %s: %d hot files under it are not fully cached, so their reads still cost downloads", d, uncachedDirs[d]))
	}
	if s.opt.Index != nil {
		for _, hp := range out.Paths {
			if hp.Stale && hp.ByKind[agent.ReadByAgent] > 0 {
				out.Suggestions = append(out.Suggestions, fmt.Sprintf("review %s: agents read it %d times this window but it last changed %s", hp.Path, hp.ByKind[agent.ReadByAgent], hp.MTime))
				break
			}
		}
	}
	if len(out.Paths) == 0 {
		out.Note = "no reads recorded under this path in the window; the record starts when this daemon began counting"
	}
	return text("%d hot paths under %s over %d days", len(out.Paths), root, days), out, nil
}

// observeRead reports a read that did not go through the VFS.
func (s *Server) observeRead(p string) {
	if s.opt.ReadObserver != nil {
		s.opt.ReadObserver.Observe(p, agent.ReadByAgent)
	}
}
