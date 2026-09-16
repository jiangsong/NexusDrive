package mcpsrv

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"cloudfs/internal/agent"
	"cloudfs/internal/cache"
	"cloudfs/internal/meta"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Recursive delete (docs/agent-first-design.md §5.4, TODO.md T-48). Phase
// one recorded one row saying "dir" and rollback skipped the whole
// subtree. Now the subtree is walked in meta first — no provider call —
// to produce a plan the agent can show the person, and with confirm every
// file the cache holds in full gets its own preimage row, so a rollback
// restores the directory, its subdirectories and those files. Files that
// are not cached are recorded as not_cached rather than downloaded: a
// delete must never cost a download.

// planSampleMax bounds the paths a plan lists; the counts say the rest.
const planSampleMax = 50

// DefaultPreimageFiles is how many files under one recursive delete get
// a preimage when the configuration sets nothing.
const DefaultPreimageFiles = 500

// subtreeEntry is one node under a recursive delete, from meta.
type subtreeEntry struct {
	path   string
	depth  int
	dir    bool
	size   int64
	cached bool
}

// walkForDelete lists what meta holds under ino, and whether each file is
// fully cached, without touching the provider.
func (s *Server) walkForDelete(ctx context.Context, ino uint64, base string) ([]subtreeEntry, int, error) {
	var out []subtreeEntry
	store := s.opt.FS.Meta()
	unlisted := 0
	listed := func(dir uint64) error {
		st, err := store.DirState(ctx, dir)
		if err != nil {
			return err
		}
		if !st.Complete {
			unlisted++
		}
		return nil
	}
	if err := listed(ino); err != nil {
		return nil, 0, err
	}
	root := strings.TrimSuffix(base, "/")
	err := store.WalkSubtree(ctx, ino, base, func(n meta.Node, p string) error {
		e := subtreeEntry{path: p, depth: strings.Count(strings.TrimPrefix(p, root), "/"), dir: n.IsDir(), size: n.Size}
		if n.IsDir() {
			if err := listed(n.Ino); err != nil {
				return err
			}
		} else if n.RemoteID != "" {
			have, total := s.opt.FS.Cache().Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
			e.cached = (total > 0 && have == total) || (total == 0 && n.Size == 0)
		}
		out = append(out, e)
		return nil
	})
	return out, unlisted, err
}

// planOf summarises a walk.
func planOf(entries []subtreeEntry) *deletePlan {
	plan := &deletePlan{Sample: []string{}}
	for _, e := range entries {
		if e.dir {
			plan.Dirs++
		} else {
			plan.Files++
			plan.Bytes += e.size
			if e.cached {
				plan.Cached++
			}
		}
		if len(plan.Sample) < planSampleMax {
			plan.Sample = append(plan.Sample, e.path)
		} else {
			plan.More = true
		}
	}
	return plan
}

// deleteRecursive is delete with recursive=true: a plan without confirm,
// the deletion with it, in both cases from what meta already holds.
func (s *Server) deleteRecursive(ctx context.Context, p string, confirm bool) (*mcp.CallToolResult, okOutput, error) {
	target, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, okOutput{}, nil
	}
	var entries []subtreeEntry
	unlisted := 0
	if target.IsDir {
		entries, unlisted, err = s.walkForDelete(ctx, target.Ino, p)
		if err != nil {
			r, _ := fail(err)
			return r, okOutput{}, nil
		}
	}
	plan := planOf(entries)
	plan.Unlisted = unlisted
	if !confirm {
		out := okOutput{Path: p, OK: false, Plan: plan, PreimageReason: preimageNotRecorded}
		msg := fmt.Sprintf("plan only: deleting %s would remove %d files (%d bytes) and %d directories; %d files are cached and could be restored.",
			p, plan.Files, plan.Bytes, plan.Dirs, plan.Cached)
		if plan.Unlisted > 0 {
			msg += fmt.Sprintf(" %d directories under it were never listed, so the plan is incomplete; call directory_tree or list_directory first.", plan.Unlisted)
		}
		return text("%s Pass confirm=true to delete.", msg), out, nil
	}
	if err := s.requireOwner(ctx); err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	parent, err := s.opt.FS.StatPath(ctx, path.Dir(p))
	if err != nil {
		r, _ := fail(mapErr(err, path.Dir(p)))
		return r, okOutput{}, nil
	}
	if !target.IsDir {
		// recursive on a file is just a delete.
		rec := s.beforeWrite(ctx, "delete", p, "", "")
		if err := s.opt.FS.Remove(ctx, parent.Ino, path.Base(p), true); err != nil {
			rec.failed(ctx)
			r, _ := fail(mapErr(err, p))
			return r, okOutput{}, nil
		}
		rec.done(ctx, "")
		out := okOutput{Path: p, OK: true}
		out.Reversible, out.PreimageReason = rec.reversibility()
		return text("deleted %s (%s)", p, reversibleText(out.Reversible, out.PreimageReason)), out, nil
	}
	// Rows are undone newest first, so they are recorded in the order the
	// undo must run backwards: files, then directories deepest first,
	// then the directory itself. Rolling back then makes the directory,
	// its subdirectories from the top down, and finally the files.
	recs := s.recordSubtree(ctx, entries, plan)
	top := s.beforeWrite(ctx, "delete", p, "", "")
	recs = append(recs, top)
	if err := s.opt.FS.Remove(ctx, parent.Ino, path.Base(p), true); err != nil {
		for _, rec := range recs {
			rec.failed(ctx)
		}
		r, _ := fail(mapErr(err, p))
		return r, okOutput{}, nil
	}
	for _, rec := range recs {
		rec.done(ctx, "")
	}
	out := okOutput{Path: p, OK: true, Plan: plan}
	switch {
	case top == nil:
		out.PreimageReason = preimageNotRecorded
	case plan.Unkept == 0 && plan.Unlisted == 0:
		out.Reversible, out.PreimageReason = true, preimageOK
	default:
		// The directory and the kept files come back; the rest does not.
		out.PreimageReason = preimagePartial
	}
	return text("deleted %s: %d files and %d directories (%s; %d of %d files kept for rollback)", p, plan.Files, plan.Dirs,
		reversibleText(out.Reversible, out.PreimageReason), plan.Kept, plan.Files), out, nil
}

// recordSubtree records one row per entry under a recursive delete and
// fills the plan's kept/unkept counts. A cached file within the budget
// is captured (a cache hit: the blob is a hard link); every other file
// gets a reason and no download. Without a session or store, nothing is
// recorded and the plan says every file is unkept.
func (s *Server) recordSubtree(ctx context.Context, entries []subtreeEntry, plan *deletePlan) []*opRecord {
	budget := s.opt.PreimageFiles
	if budget <= 0 {
		budget = DefaultPreimageFiles
	}
	var recs []*opRecord
	var dirs []subtreeEntry
	for _, e := range entries {
		if e.dir {
			dirs = append(dirs, e)
			continue
		}
		var rec *opRecord
		switch {
		case !e.cached:
			rec = s.recordPre(ctx, "delete", e.path, agent.Pre{State: "file", Size: e.size, Reason: "not_cached"})
		case plan.Kept >= budget:
			rec = s.recordPre(ctx, "delete", e.path, agent.Pre{State: "file", Size: e.size, Reason: preimageTooMany})
		default:
			rec = s.beforeWrite(ctx, "delete", e.path, "", "")
		}
		if rec != nil && rec.pre.Reason == "" {
			plan.Kept++
		} else {
			plan.Unkept++
		}
		recs = append(recs, rec)
	}
	sort.SliceStable(dirs, func(i, j int) bool { return dirs[i].depth > dirs[j].depth })
	for _, d := range dirs {
		recs = append(recs, s.recordPre(ctx, "delete", d.path, agent.Pre{State: "dir"}))
	}
	return recs
}

// String is for the summary line of a plan-only call.
func (p *deletePlan) String() string {
	return fmt.Sprintf("%d files, %d dirs, %d bytes", p.Files, p.Dirs, p.Bytes)
}
