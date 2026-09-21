package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/index"
	"cloudfs/internal/memory"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// context_search is the agent-facing retrieval entrypoint. It combines the
// mount's name index, extracted content index and durable memory while
// keeping the three kinds of evidence separate in the response.
type contextSearchInput struct {
	Query        string   `json:"query" jsonschema:"Words to find in file names, extracted content and memory"`
	Path         string   `json:"path,omitempty" jsonschema:"Knowledge subtree to search; default the whole mount"`
	Sources      []string `json:"sources,omitempty" jsonschema:"Any of knowledge, memory and handoff; default all"`
	TopK         int      `json:"top_k,omitempty" jsonschema:"Maximum total results; default 12, the server caps this"`
	Mode         string   `json:"mode,omitempty" jsonschema:"keyword, hybrid or vector for indexed content"`
	Scope        string   `json:"scope,omitempty" jsonschema:"Project or workspace memory scope; global memories are included too"`
	IncludeStale bool     `json:"include_stale,omitempty" jsonschema:"Include chunks extracted from an older file version; default false"`
}

type contextHit struct {
	Source     string  `json:"source"`
	Path       string  `json:"path"`
	Version    string  `json:"version,omitempty"`
	Agent      string  `json:"agent,omitempty"`
	Name       string  `json:"name,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	Heading    string  `json:"heading,omitempty"`
	Snippet    string  `json:"snippet,omitempty"`
	Score      float64 `json:"score"`
	StartOff   int64   `json:"start_off,omitempty"`
	EndOff     int64   `json:"end_off,omitempty"`
	OffsetKind string  `json:"offset_kind,omitempty"`
	Stale      bool    `json:"stale,omitempty"`
}

type contextCoverage struct {
	Listed      int64 `json:"listed_dirs"`
	Known       int64 `json:"known_dirs"`
	Indexed     int   `json:"indexed_documents"`
	Pending     int   `json:"pending_documents"`
	Failed      int   `json:"failed_documents"`
	StaleHidden int   `json:"stale_hidden"`
	Crawling    bool  `json:"crawling"`
}

type contextSearchOutput struct {
	Knowledge   []contextHit    `json:"knowledge"`
	Memories    []contextHit    `json:"memories"`
	Handoffs    []contextHit    `json:"handoffs"`
	Coverage    contextCoverage `json:"coverage"`
	ModeUsed    string          `json:"mode_used,omitempty"`
	Degraded    string          `json:"degraded,omitempty"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Truncated   bool            `json:"truncated"`
	TruncatedBy string          `json:"truncated_by,omitempty"`
}

// contextCandidateLimit bounds the work used to refill TopK after permission,
// source, staleness and memory-policy filters reject higher-ranked hits.
func contextCandidateLimit(topK int) int {
	n := topK * 32
	if n < 256 {
		n = 256
	}
	if n > 4096 {
		n = 4096
	}
	return max(n, topK)
}

func (s *Server) registerContextTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "context_search",
		Description: "Search the mounted drive as one knowledge and memory system. It combines file-name matches, indexed content, durable memory and handoff files; returns them in separate groups with versions and offsets for safe follow-up reads. Stale indexed chunks are hidden unless requested.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.contextSearch)
}

func (s *Server) contextSearch(ctx context.Context, req *mcp.CallToolRequest, in contextSearchInput) (*mcp.CallToolResult, contextSearchOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		r, _ := fail(errors.New("query must not be empty"))
		return r, contextSearchOutput{}, nil
	}
	want, err := contextSources(in.Sources)
	if err != nil {
		r, _ := fail(err)
		return r, contextSearchOutput{}, nil
	}
	root := "/"
	if in.Path != "" && (want["knowledge"] || want["handoff"]) {
		root, err = s.checkPath(ctx, in.Path, false)
		if err != nil {
			r, _ := fail(err)
			return r, contextSearchOutput{}, nil
		}
	}
	limit := in.TopK
	if limit <= 0 {
		limit = 12
	}
	if limit > s.opt.Limits.MaxResults {
		limit = s.opt.Limits.MaxResults
	}
	candidateLimit := contextCandidateLimit(limit)
	out := contextSearchOutput{Knowledge: []contextHit{}, Memories: []contextHit{}, Handoffs: []contextHit{}}

	// Metadata search is useful before a file has been extracted and gives
	// exact versions for follow-up reads.
	if want["knowledge"] || want["handoff"] {
		filter, parseErr := meta.ParseQuery(in.Query)
		if parseErr != nil {
			r, _ := fail(parseErr)
			return r, contextSearchOutput{}, nil
		}
		answer, searchErr := s.opt.FS.Search(ctx, meta.SearchQuery{
			Filter: filter, Roots: s.readRoots(ctx, root), Limit: candidateLimit,
		})
		if searchErr != nil {
			r, _ := fail(searchErr)
			return r, contextSearchOutput{}, nil
		}
		out.Coverage.Listed, out.Coverage.Known, out.Coverage.Crawling = answer.Coverage.Listed, answer.Coverage.Known, answer.Crawling
		if !answer.Complete {
			out.Truncated = true
			out.Diagnostics = append(out.Diagnostics, "name search stopped at its work budget")
		}
		if len(answer.Results) == candidateLimit {
			out.Truncated = true
		}
		for _, result := range answer.Results {
			if result.Kind == provider.KindDir || !s.visible(ctx, result.Path) || s.isMemoryPath(result.Path) {
				continue
			}
			h := contextHit{Source: "knowledge", Path: result.Path, Version: result.Version, Kind: "file", Name: result.Name, Snippet: result.Name, Score: .25}
			if isHandoff(result.Path) {
				h.Source = "handoff"
				if want["handoff"] {
					out.Handoffs = appendContextHit(out.Handoffs, h)
				}
			} else if want["knowledge"] {
				out.Knowledge = appendContextHit(out.Knowledge, h)
			}
		}
	}

	if s.opt.Index != nil && (want["knowledge"] || want["handoff"]) {
		res, searchErr := s.opt.Index.Search(ctx, index.SearchQuery{
			Query: in.Query, Roots: s.readRoots(ctx, root), TopK: limit * 3, Mode: in.Mode,
			ScanLimit: candidateLimit, MaxSnippetBytes: 1024, MaxBytes: int64(s.opt.Limits.MaxBytes),
			Accept: func(hit index.Hit) bool {
				if !s.visible(ctx, hit.Path) || s.isMemoryPath(hit.Path) {
					return false
				}
				if hit.Stale && !in.IncludeStale {
					out.Coverage.StaleHidden++
					return false
				}
				if isHandoff(hit.Path) {
					return want["handoff"]
				}
				return want["knowledge"]
			},
		})
		if errors.Is(searchErr, index.ErrEmptyQuery) {
			searchErr = errors.New("query must not be empty")
		}
		if searchErr != nil {
			r, _ := fail(searchErr)
			return r, contextSearchOutput{}, nil
		}
		out.ModeUsed, out.Degraded = res.ModeUsed, res.Degraded
		out.Coverage.Indexed, out.Coverage.Pending = res.Docs, res.Pending
		out.Truncated = out.Truncated || res.Truncated
		for _, hit := range res.Hits {
			h := indexedContextHit("knowledge", hit)
			if isHandoff(hit.Path) {
				h.Source = "handoff"
				if want["handoff"] {
					out.Handoffs = appendContextHit(out.Handoffs, h)
				}
			} else if want["knowledge"] {
				out.Knowledge = appendContextHit(out.Knowledge, h)
			}
		}
		if st, statusErr := s.opt.Index.Status(ctx, root); statusErr == nil {
			out.Coverage.Failed = st.Docs.Failed
		}
	} else if want["knowledge"] || want["handoff"] {
		out.Diagnostics = append(out.Diagnostics, "content index is disabled; only names were searched")
	}

	if want["memory"] {
		s.searchContextMemory(ctx, req, in, limit, &out)
	}
	sortContextHits(out.Knowledge)
	sortContextHits(out.Memories)
	sortContextHits(out.Handoffs)
	trimContextCount(&out, limit)
	trimContextTokens(&out, s.tokenBudget())
	msg := fmt.Sprintf("%d knowledge, %d memory and %d handoff results for %q", len(out.Knowledge), len(out.Memories), len(out.Handoffs), in.Query)
	return text("%s", msg), out, nil
}

func (s *Server) searchContextMemory(ctx context.Context, req *mcp.CallToolRequest, in contextSearchInput, limit int, out *contextSearchOutput) {
	if s.opt.Memory == nil {
		out.Diagnostics = append(out.Diagnostics, "memory is disabled")
		return
	}
	ag, err := s.memoryAgent(ctx, req, "")
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, "memory unavailable: "+err.Error())
		return
	}
	if !s.opt.Memory.HasIndex() {
		out.Diagnostics = append(out.Diagnostics, "memory index is disabled")
		return
	}
	replacements := map[string]map[string]bool{}
	now := time.Now()
	res, err := s.opt.Memory.Search(ctx, memory.SearchOptions{
		Query: in.Query, Agent: ag, IncludeShared: true, TopK: limit, Mode: in.Mode,
		ScanLimit: contextCandidateLimit(limit), MaxSnippetBytes: 1024, MaxBytes: int64(s.opt.Limits.MaxBytes),
		Accept: func(hit memory.SearchHit) bool {
			if !s.visible(ctx, hit.Path) || hit.Name == "" {
				return false
			}
			if hit.Stale && !in.IncludeStale {
				out.Coverage.StaleHidden++
				return false
			}
			fact, getErr := s.opt.Memory.Get(ctx, hit.Agent, hit.Name)
			if getErr != nil || fact.Meta.ExpiresAt != nil && !fact.Meta.ExpiresAt.After(now) ||
				fact.Meta.Scope != "" && fact.Meta.Scope != in.Scope {
				return false
			}
			set, ok := replacements[hit.Agent]
			if !ok {
				set = s.activeReplacements(ctx, hit.Agent, in.Scope)
				replacements[hit.Agent] = set
			}
			return !set[hit.Name]
		},
	})
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, "memory search failed: "+err.Error())
		return
	}
	if out.ModeUsed == "" {
		out.ModeUsed, out.Degraded = res.ModeUsed, res.Degraded
	}
	out.Coverage.Pending = max(out.Coverage.Pending, res.Pending)
	out.Truncated = out.Truncated || res.Truncated
	for _, hit := range res.Hits {
		h := indexedContextHit("memory", hit.Hit)
		h.Agent, h.Name = hit.Agent, hit.Name
		out.Memories = appendContextHit(out.Memories, h)
	}
}

func (s *Server) activeReplacements(ctx context.Context, ag, scope string) map[string]bool {
	out := map[string]bool{}
	cursor := ""
	for {
		facts, next, err := s.opt.Memory.List(ctx, ag, cursor, s.opt.Limits.MaxEntries)
		if err != nil {
			return out
		}
		for _, fact := range facts {
			if fact.Meta.ExpiresAt != nil && !fact.Meta.ExpiresAt.After(time.Now()) {
				continue
			}
			if fact.Meta.Scope != "" && fact.Meta.Scope != scope {
				continue
			}
			for _, name := range fact.Meta.Replaces {
				out[name] = true
			}
		}
		if next == "" {
			return out
		}
		cursor = next
	}
}

func indexedContextHit(source string, h index.Hit) contextHit {
	return contextHit{Source: source, Path: h.Path, Version: h.Version, Heading: h.Heading, Snippet: h.Snippet,
		Score: h.Score, StartOff: h.StartOff, EndOff: h.EndOff, OffsetKind: h.OffsetKind, Stale: h.Stale}
}

func contextSources(in []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(in) == 0 {
		out["knowledge"], out["memory"], out["handoff"] = true, true, true
		return out, nil
	}
	for _, source := range in {
		source = strings.ToLower(strings.TrimSpace(source))
		switch source {
		case "knowledge", "memory", "handoff":
			out[source] = true
		default:
			return nil, fmt.Errorf("unknown context source %q; use knowledge, memory or handoff", source)
		}
	}
	return out, nil
}

func (s *Server) isMemoryPath(p string) bool {
	if s.opt.Memory == nil || s.opt.Memory.Root() == "" {
		return false
	}
	root := path.Join(s.opt.Memory.Root(), "memory")
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

func isHandoff(p string) bool { return strings.EqualFold(path.Base(p), "handoff.md") }

// appendContextHit merges the name-only form of a file with its first
// content chunk, and otherwise keeps at most two chunks per file.
func appendContextHit(dst []contextHit, h contextHit) []contextHit {
	count := 0
	for i := range dst {
		if dst[i].Path != h.Path {
			continue
		}
		count++
		oldNameOnly, newNameOnly := dst[i].EndOff == 0, h.EndOff == 0
		switch {
		case oldNameOnly && !newNameOnly:
			h.Score += dst[i].Score
			if h.Version == "" {
				h.Version = dst[i].Version
			}
			dst[i] = h
			return dst
		case !oldNameOnly && newNameOnly:
			dst[i].Score += h.Score
			if dst[i].Version == "" {
				dst[i].Version = h.Version
			}
			return dst
		case dst[i].StartOff == h.StartOff && dst[i].EndOff == h.EndOff:
			return dst
		}
	}
	if count >= 2 {
		return dst
	}
	return append(dst, h)
}

func sortContextHits(hits []contextHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Path < hits[j].Path
	})
}

// trimContextCount reserves one slot for every non-empty group, then shares
// the remainder round-robin so file results cannot crowd memory out.
func trimContextCount(out *contextSearchOutput, limit int) {
	groups := []*[]contextHit{&out.Knowledge, &out.Memories, &out.Handoffs}
	total := len(out.Knowledge) + len(out.Memories) + len(out.Handoffs)
	if total <= limit {
		return
	}
	keep := [3]int{}
	left := limit
	for i, group := range groups {
		if len(*group) > 0 && left > 0 {
			keep[i]++
			left--
		}
	}
	for left > 0 {
		progress := false
		for i, group := range groups {
			if left > 0 && keep[i] < len(*group) {
				keep[i]++
				left--
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	for i, group := range groups {
		*group = (*group)[:keep[i]]
	}
	out.Truncated = true
}

func trimContextTokens(out *contextSearchOutput, budget int) {
	if budget <= 0 {
		return
	}
	groups := []*[]contextHit{&out.Knowledge, &out.Memories, &out.Handoffs}
	keep := [3]int{}
	blocked := [3]bool{}
	remaining := budget
	for {
		progress := false
		for i, group := range groups {
			if blocked[i] || keep[i] == len(*group) {
				continue
			}
			b, _ := json.Marshal((*group)[keep[i]])
			cost := agent.EstimateTokensBytes(b)
			if cost > remaining {
				// Preserve the group's ranked prefix: a later, smaller item must
				// not leapfrog the result that did not fit.
				blocked[i] = true
				continue
			}
			keep[i]++
			remaining -= cost
			progress = true
		}
		if !progress {
			break
		}
	}
	for i, group := range groups {
		if keep[i] < len(*group) {
			*group = (*group)[:keep[i]]
			out.Truncated, out.TruncatedBy = true, truncatedByTokens
		}
	}
}
