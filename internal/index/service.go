package index

import (
	"context"
	"errors"
	"path"
	"strings"

	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
)

// This file is the indexer's service surface: what the MCP tools, the
// control plane and the CLI call. Every method is safe to use from a
// process that does not own the index (search, status and rules work
// through the shared index.db); only the worker itself is owner-only.

// Status is what index_status and the console show. The per-path fields
// are filled only when a path was asked about.
type Status struct {
	Enabled bool `json:"enabled"`
	// Covered is the path of the rule covering the queried path.
	Covered string `json:"covered,omitempty"`
	// RuleSource is where that rule came from: config | ui | tool.
	RuleSource string `json:"rule_source,omitempty"`
	// State is ok | dirty | failed | pending | uncovered for the queried
	// file. A directory, or a path meta does not know, has no state of
	// its own when a rule covers it: Covered says what would apply.
	State string `json:"state,omitempty"`
	// Chunks is the number of chunks the queried file has.
	Chunks int `json:"chunks,omitempty"`
	// Error is the extraction failure of the queried file.
	Error string `json:"error,omitempty"`

	Docs        DocCounts `json:"docs"`
	ChunksTotal int       `json:"chunks_total"`
	Pending     int       `json:"pending"`
	// Failed lists the most recent failures, at most maxStatusFailed.
	Failed       []FailedDoc `json:"failed,omitempty"`
	TextBytes    int64       `json:"text_bytes"`
	MaxTotalText int64       `json:"max_total_text"`
	FetchBudget  BudgetUse   `json:"fetch_budget"`
	// FetchBytesTotal counts the bytes fetched from remotes since the
	// process started, for cloudfs_index_fetch_bytes_total.
	FetchBytesTotal int64    `json:"fetch_bytes_total"`
	Progress        Progress `json:"progress"`
}

// DocCounts splits the documents by state.
type DocCounts struct {
	OK     int `json:"ok"`
	Dirty  int `json:"dirty"`
	Failed int `json:"failed"`
}

// BudgetUse is the hourly fetch budget: bytes used in the current window
// and the limit (0 when unlimited).
type BudgetUse struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

// RuleView is a rule with the number of documents it currently covers.
type RuleView struct {
	Rule
	Documents int `json:"documents"`
}

// Per-path states reported by Status.
const (
	StateOK        = "ok"
	StateDirty     = "dirty"
	StateFailed    = "failed"
	StatePending   = "pending"
	StateUncovered = "uncovered"
)

// maxStatusFailed bounds the failure list embedded in a Status.
const maxStatusFailed = 20

// Search runs q against the index, resolving every hit against the live
// tree so a renamed file is reported at its current path and an outdated
// document is marked stale.
func (x *Indexer) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	return x.store.Search(ctx, q, x.current(ctx))
}

// current is the Current the searches use: the node meta holds for a
// remote id, and its path from the root of the mount.
func (x *Indexer) current(ctx context.Context) Current {
	m := x.fs.Meta()
	return func(remote, remoteID string) (string, string, bool) {
		n, err := m.ByRemoteID(ctx, remote, remoteID)
		if err != nil {
			return "", "", false
		}
		p, err := m.Path(ctx, n.Ino)
		if err != nil {
			// The version still says whether the text is current; the
			// indexed path is the best path available.
			return "", n.Version, true
		}
		return p, n.Version, true
	}
}

// Status reports the index as a whole and, when p is not empty, how the
// index sees that path.
func (x *Indexer) Status(ctx context.Context, p string) (Status, error) {
	st, err := x.store.Stats(ctx)
	if err != nil {
		return Status{}, err
	}
	failed, _, err := x.store.Failed(ctx, "", maxStatusFailed)
	if err != nil {
		return Status{}, err
	}
	used, limit := x.budget.Used()
	out := Status{
		Enabled:         true,
		Docs:            DocCounts{OK: st.DocsOK, Dirty: st.DocsDirty, Failed: st.DocsFailed},
		ChunksTotal:     st.Chunks,
		Pending:         st.Pending,
		Failed:          failed,
		TextBytes:       st.TextBytes,
		MaxTotalText:    int64(x.opt.Config.MaxTotalText),
		FetchBudget:     BudgetUse{Used: used, Limit: limit},
		FetchBytesTotal: x.fetched.Load(),
		Progress:        x.Progress(),
	}
	if p == "" {
		return out, nil
	}
	if err := x.pathStatus(ctx, path.Clean("/"+strings.TrimPrefix(p, "/")), &out); err != nil {
		return Status{}, err
	}
	return out, nil
}

// pathStatus fills the per-path half of a Status.
func (x *Indexer) pathStatus(ctx context.Context, p string, out *Status) error {
	m := x.matcher.Load()
	if r, ok := m.Covering(p); ok {
		out.Covered, out.RuleSource = r.Path, r.Source
	}
	n, err := x.rootNode(ctx, p)
	if errors.Is(err, vfs.ErrNotFound) || errors.Is(err, meta.ErrNotFound) {
		if out.Covered == "" {
			out.State = StateUncovered
		}
		return nil
	}
	if err != nil {
		return err
	}
	if n.IsDir() {
		if out.Covered == "" && !x.pinCoversDir(p) {
			out.State = StateUncovered
		}
		return nil
	}
	d, found, err := x.store.DocumentByPath(ctx, p)
	if err != nil {
		return err
	}
	if found {
		out.Error = d.Error
		out.Chunks, err = x.store.ChunkCount(ctx, d.ID)
		if err != nil {
			return err
		}
		switch d.State {
		case DocOK:
			out.State = StateOK
		case DocDirty:
			out.State = StateDirty
		case DocFailed:
			out.State = StateFailed
		}
		return nil
	}
	queued, err := x.store.Queued(ctx, n.Ino)
	if err != nil {
		return err
	}
	if queued || x.selects(n, p) {
		out.State = StatePending
		return nil
	}
	out.State = StateUncovered
	return nil
}

// pinCoversDir reports whether pinned mode would index files under p.
func (x *Indexer) pinCoversDir(p string) bool {
	return x.opt.Config.Pinned && x.pinCovers(p)
}

// Covering returns the deepest rule whose path is p or an ancestor of p,
// without consulting include patterns or size: what a directory or a
// not-yet-listed file is covered by.
func (m *Matcher) Covering(p string) (Rule, bool) {
	for _, r := range m.rules {
		if covers(r.Path, p) {
			return r, true
		}
	}
	return Rule{}, false
}

// AddRule persists a run-time rule (Source defaults to "tool"), rebuilds
// the matcher and reconciles the rule's subtree at once, so files meta
// already knows are queued before the tool returns.
func (x *Indexer) AddRule(ctx context.Context, r Rule) error {
	r.Path = cleanRulePath(r.Path)
	if err := x.store.AddRule(ctx, r); err != nil {
		return err
	}
	if err := x.ReloadRules(ctx); err != nil {
		return err
	}
	_, err := x.reconcile(ctx, []string{r.Path}, false)
	return err
}

// RemoveRule deletes a run-time rule (ErrConfigRule for a configured one)
// and drops the documents no rule or pin covers any more.
func (x *Indexer) RemoveRule(ctx context.Context, p string) error {
	if err := x.store.RemoveRule(ctx, cleanRulePath(p)); err != nil {
		return err
	}
	if err := x.ReloadRules(ctx); err != nil {
		return err
	}
	// A full pass keeps only what a remaining root reaches; the worker
	// then drains whatever it queued.
	_, err := x.reconcile(ctx, nil, true)
	return err
}

// Text pages through the extracted text of the document at p.
func (x *Indexer) Text(ctx context.Context, p string, off int64, max int) (TextPage, error) {
	return x.store.Text(ctx, path.Clean("/"+strings.TrimPrefix(p, "/")), off, max)
}

// Rules lists every rule with the number of documents under its path.
func (x *Indexer) Rules(ctx context.Context) ([]RuleView, error) {
	rules, err := x.store.Rules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RuleView, 0, len(rules))
	for _, r := range rules {
		n, err := x.store.DocumentsUnder(ctx, r.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, RuleView{Rule: r, Documents: n})
	}
	return out, nil
}

// Rebuild empties the index and queues a full pass. Rules survive.
func (x *Indexer) Rebuild(ctx context.Context) error {
	x.runMu.Lock()
	err := x.store.Reset(ctx)
	x.runMu.Unlock()
	if err != nil {
		return err
	}
	x.Kick()
	return nil
}

// Retry queues the failed documents at or under p ("" for all) again and
// reports how many.
func (x *Indexer) Retry(ctx context.Context, p string) (int64, error) {
	n, err := x.store.RetryFailed(ctx, p)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		x.kick()
	}
	return n, nil
}

// Failed pages through the failed documents, most recent first.
func (x *Indexer) Failed(ctx context.Context, cursor string, limit int) ([]FailedDoc, string, error) {
	return x.store.Failed(ctx, cursor, limit)
}
