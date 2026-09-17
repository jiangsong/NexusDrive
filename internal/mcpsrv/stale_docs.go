package mcpsrv

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"cloudfs/internal/meta"
)

// stale_docs (T-58, borrowed from BearDrive's `bdrive stale`): the
// Markdown documents under a directory that link to files changed after
// the document was — the README that still describes last month's
// config, the plan that points at a script someone rewrote. It is
// advisory: a listing, no judgement. Zero provider calls: the tree and
// every mtime come from meta, and a document's links are read only when
// the cache holds all of it; an uncached document is reported as skipped
// rather than downloaded. hot_paths says what is read but old; this says
// what is old relative to what it points at.

type staleDocsInput struct {
	Path  string `json:"path,omitempty" jsonschema:"Directory to look under; default /"`
	Limit int    `json:"limit,omitempty" jsonschema:"Documents to return; default 50, at most 200"`
}

type staleLink struct {
	Target   string    `json:"target"`
	Modified time.Time `json:"modified"`
}

type staleDoc struct {
	Path     string      `json:"path"`
	Modified time.Time   `json:"modified"`
	Newer    []staleLink `json:"newer"`
}

type staleDocsOutput struct {
	Path string     `json:"path"`
	Docs []staleDoc `json:"docs"`
	// Skipped counts the documents whose links could not be read without
	// a download; pin them to have them checked.
	Skipped   int  `json:"skipped"`
	Truncated bool `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// mdLinkRE finds the target of a Markdown link or image.
var mdLinkRE = regexp.MustCompile(`!?\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// staleDocsMax bounds how much of a document is read for links.
const staleDocsMax = 1 << 20

func (s *Server) registerStaleDocs() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "stale_docs",
		Description: "List the Markdown documents under a directory that link to files changed after the document was last changed: what may describe an older state of what it points at. " +
			"Advisory only. Reads only cached documents (the rest are counted as skipped); costs no provider call.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.staleDocs)
}

func (s *Server) staleDocs(ctx context.Context, _ *mcp.CallToolRequest, in staleDocsInput) (*mcp.CallToolResult, staleDocsOutput, error) {
	root := "/"
	if in.Path != "" {
		root = in.Path
	}
	p, err := s.checkPath(ctx, root, false)
	if err != nil {
		r, _ := fail(err)
		return r, staleDocsOutput{}, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	store := s.opt.FS.Meta()
	node, err := store.Resolve(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, staleDocsOutput{}, nil
	}
	out := staleDocsOutput{Path: p, Docs: []staleDoc{}}
	// One pass over the subtree collects every file's mtime; the
	// documents are checked against that map afterwards, so a link to a
	// sibling costs a lookup and not a resolve.
	mtimes := map[string]time.Time{}
	var docs []string
	visited := 0
	err = store.WalkSubtree(ctx, node.Ino, p, func(n meta.Node, np string) error {
		if !s.visible(ctx, np) {
			return meta.SkipDir
		}
		if n.IsDir() {
			return nil
		}
		visited++
		if visited > 200000 {
			return errTreeFull
		}
		mtimes[np] = n.MTime
		if ext := strings.ToLower(path.Ext(np)); ext == ".md" || ext == ".markdown" {
			docs = append(docs, np)
		}
		return nil
	})
	if err != nil && err != errTreeFull {
		r, _ := fail(mapErr(err, p))
		return r, staleDocsOutput{}, nil
	}
	sort.Strings(docs)
	for _, d := range docs {
		if len(out.Docs) >= limit {
			out.Truncated = true
			break
		}
		a, err := s.opt.FS.StatPath(ctx, d)
		if err != nil || (a.Cached < 1 && !a.LocalOnly) || a.Size == 0 {
			if err == nil && a.Size > 0 {
				out.Skipped++
			}
			continue
		}
		data, err := s.opt.FS.ReadFileRange(ctx, d, 0, min(a.Size, staleDocsMax))
		if err != nil {
			out.Skipped++
			continue
		}
		doc := staleDoc{Path: d, Modified: mtimes[d], Newer: []staleLink{}}
		seen := map[string]bool{}
		for _, m := range mdLinkRE.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if i := strings.IndexAny(target, "#?"); i >= 0 {
				target = target[:i]
			}
			if target == "" {
				continue
			}
			abs := target
			if !strings.HasPrefix(abs, "/") {
				abs = path.Join(path.Dir(d), target)
			}
			abs = path.Clean(abs)
			if seen[abs] || abs == d {
				continue
			}
			seen[abs] = true
			mt, ok := mtimes[abs]
			if !ok {
				continue
			}
			if mt.After(doc.Modified) {
				doc.Newer = append(doc.Newer, staleLink{Target: abs, Modified: mt})
			}
		}
		if len(doc.Newer) > 0 {
			out.Docs = append(out.Docs, doc)
		}
	}
	if out.Skipped > 0 {
		out.Note = fmt.Sprintf("%d document(s) not cached were not checked; pin them to include them", out.Skipped)
	}
	msg := fmt.Sprintf("%d document(s) under %s link to files changed after them", len(out.Docs), p)
	if out.Skipped > 0 {
		msg += fmt.Sprintf(" (%d not cached, skipped)", out.Skipped)
	}
	return text("%s", msg), out, nil
}
