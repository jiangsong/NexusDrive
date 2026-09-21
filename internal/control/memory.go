package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/memory"
)

// The memory routes (docs/agent-roadmap.md §3.11 and §6.2, TODO.md T-40)
// serve the console's memory tab and `cloudfs memory` from the same
// memory.Store the memory_* MCP tools use: a fact written here is the same
// file, with the same name rule, size limits, MEMORY.md line and version
// check, that an agent reads back. The routes add nothing of their own
// beyond the HTTP shape: the store's errors become status codes, a
// destructive delete goes through confirmed(), and a daemon without a
// store or without a root says so in a form the tab can turn into a setup
// hint. Conflict copies are read and removed through /fs/preview and
// /fs/delete, since their names do not fit the fact name rule.
//
// Two words are reserved on this API: /memory/agents is the agent table
// and /memory/search the search box, so an agent literally named "agents"
// or "search" is reachable only through the MCP tools.

// MemoryControl is what the routes need from the memory store; *memory.Store
// satisfies it. A nil Collector.Memory makes /memory/agents answer
// {"enabled":false} and every other memory route 404.
type MemoryControl interface {
	Root() string
	Config() config.Memory
	HasIndex() bool
	Agents(ctx context.Context) ([]memory.AgentSummary, error)
	List(ctx context.Context, agent, cursor string, limit int) ([]memory.FactMeta, string, error)
	Get(ctx context.Context, agent, name string) (memory.Fact, error)
	Put(ctx context.Context, agent, name, content string, opt memory.PutOptions) (memory.Fact, error)
	Delete(ctx context.Context, agent, name string) error
	Search(ctx context.Context, opt memory.SearchOptions) (memory.SearchResult, error)
	// Layout, Migrate and Merge are memory layout v2 (T-56): which layout
	// the drive is in, the one-way move to v2 under an owner, and the
	// merge proposal for a fact with a conflict copy.
	Layout(ctx context.Context) string
	Migrate(ctx context.Context, owner string) ([]string, error)
	Merge(ctx context.Context, agent, name, conflict, ancestor string) (memory.MergeResult, error)
}

// MemoryMigrateRequest is POST /memory/migrate: the owner every v1 agent
// directory moves under (default this machine's user), and the typed
// confirmation the console asks for — the move renames directories on
// the drive, and every other device sees the new layout.
type MemoryMigrateRequest struct {
	Owner   string `json:"owner,omitempty"`
	Confirm bool   `json:"confirm"`
}

// MemoryMigrateResponse is what the migration did.
type MemoryMigrateResponse struct {
	Owner  string   `json:"owner"`
	Moved  []string `json:"moved"`
	Layout string   `json:"layout"`
}

// MemoryAgentsResponse is GET /memory/agents. Enabled is false without a
// store (Reason "unavailable") or without memory.root (Reason "no_root",
// with Example being the configuration block that turns it on), so the
// console shows the setup hint rather than an empty table.
type MemoryAgentsResponse struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	Example string `json:"example,omitempty"`
	Root    string `json:"root,omitempty"`
	// MaxFactBytes and MaxAgentBytes are the limits every put is held to;
	// the editor shows them against the byte count.
	MaxFactBytes  int64                 `json:"max_fact_bytes"`
	MaxAgentBytes int64                 `json:"max_agent_bytes"`
	Agents        []memory.AgentSummary `json:"agents"`
	// Layout is v1 or v2; Owner is this machine's user, the owner an
	// unqualified agent belongs to in v2 and the default a migration uses.
	Layout string `json:"layout,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// MemoryListResponse is GET /memory/{agent}?cursor&limit.
type MemoryListResponse struct {
	Agent      string            `json:"agent"`
	Facts      []memory.FactMeta `json:"facts"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// MemoryPutRequest is PUT /memory/{agent}/{name}. ExpectedVersion, when
// set, must be the version the caller read; the put is otherwise refused
// with 409 and the version to re-read.
type MemoryPutRequest struct {
	Content               string `json:"content"`
	Mode                  string `json:"mode,omitempty"`
	ExpectedVersion       string `json:"expected_version,omitempty"`
	ExpectedRemoteVersion string `json:"expected_remote_version,omitempty"`
	Description           string `json:"description,omitempty"`
	Type                  string `json:"type,omitempty"`
}

// MemoryConflictResponse is the 409 body of a put whose expected_version
// no longer matches: the message and the version the fact has now ("" when
// it was deleted meanwhile).
type MemoryConflictResponse struct {
	Error          string `json:"error"`
	CurrentVersion string `json:"current_version"`
}

// MemoryDeleteRequest is DELETE /memory/{agent}/{name}; Confirm must be
// true, the way every destructive route works.
type MemoryDeleteRequest struct {
	Confirm bool `json:"confirm"`
}

// MemoryDeleteResponse says what was removed.
type MemoryDeleteResponse struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
}

const (
	// memoryRootExample is the configuration block the setup hint shows.
	memoryRootExample = "memory:\n  root: /work/.agent"
	// memoryReasonUnavailable and memoryReasonNoRoot are the two ways
	// /memory/agents says enabled:false.
	memoryReasonUnavailable = "unavailable"
	memoryReasonNoRoot      = "no_root"
	defaultMemoryListLimit  = 100
	maxMemoryListLimit      = 1000
	defaultMemorySearchTopK = 10
	maxMemorySearchTopK     = 100
	// maxMemoryPutRequest bounds the PUT body: the largest fact the
	// configuration allows is 32 MiB (max_fact_bytes may not exceed
	// max_agent_bytes, whose default is that), plus the JSON around it.
	maxMemoryPutRequest = 33 << 20
	maxMemoryRequest    = 4 << 10
)

// memoryError maps a store error onto a status code and catalog key. The
// store's own sentences carry the specifics (which name, how many bytes
// over), so the catalog entry wraps them rather than restating them.
func memoryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, memory.ErrNoRoot):
		httpErrorT(w, r, http.StatusNotFound, "err.memory_no_root")
	case errors.Is(err, memory.ErrBadName):
		httpErrorT(w, r, http.StatusBadRequest, "err.memory_bad_name", err)
	case errors.Is(err, memory.ErrNotFound):
		httpErrorT(w, r, http.StatusNotFound, "err.memory_not_found", err)
	case errors.Is(err, memory.ErrTooLarge):
		httpErrorT(w, r, http.StatusRequestEntityTooLarge, "err.memory_too_large", err)
	case errors.Is(err, memory.ErrBadMode):
		httpErrorT(w, r, http.StatusBadRequest, "err.memory_bad_mode")
	case errors.Is(err, memory.ErrInvalidCursor):
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_cursor")
	case errors.Is(err, memory.ErrNoIndex):
		httpErrorT(w, r, http.StatusConflict, "err.memory_no_index")
	default:
		httpErrorT(w, r, http.StatusInternalServerError, "err.memory_failed", err)
	}
}

// memoryReady is the guard every memory route but /memory/agents starts
// with: the request is local, the method is one of those allowed, and a
// store exists. A store without a root is left to the store itself, which
// answers ErrNoRoot and gets the sentence about memory.root.
func (s *Server) memoryReady(w http.ResponseWriter, r *http.Request, methods ...string) (MemoryControl, bool) {
	if !privateRequest(w, r) {
		return nil, false
	}
	if !allowMethod(w, r, methods...) {
		return nil, false
	}
	if s.collector.Memory == nil {
		httpErrorT(w, r, http.StatusNotFound, "err.memory_disabled")
		return nil, false
	}
	return s.collector.Memory, true
}

// memoryName checks one path segment against the fact and agent name
// rule before it reaches the store: ".." and "/" never get as far as a
// path join, and a name the store would refuse anyway is refused with
// the same sentence.
func memoryName(w http.ResponseWriter, r *http.Request, what, name string) bool {
	if memory.ValidName(name) {
		return true
	}
	// An agent key may be owner/agent (layout v2); the store refuses it
	// in v1 with a pointer at the migration.
	if what == "agent" {
		if owner, ag, ok := strings.Cut(name, "/"); ok && memory.ValidName(owner) && memory.ValidName(ag) && owner != memory.SharedAgent {
			return true
		}
	}
	httpErrorT(w, r, http.StatusBadRequest, "err.memory_bad_name", what+" "+strconv.Quote(name)+": "+memory.ErrBadName.Error())
	return false
}

// GET /memory/agents
func (s *Server) memoryAgents(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	m := s.collector.Memory
	if m == nil {
		writeJSON(w, MemoryAgentsResponse{Reason: memoryReasonUnavailable, Agents: []memory.AgentSummary{}})
		return
	}
	cfg := m.Config()
	out := MemoryAgentsResponse{Root: m.Root(), MaxFactBytes: int64(cfg.MaxFactBytes), MaxAgentBytes: int64(cfg.MaxAgentBytes), Agents: []memory.AgentSummary{}}
	agents, err := m.Agents(r.Context())
	if errors.Is(err, memory.ErrNoRoot) {
		out.Reason, out.Example = memoryReasonNoRoot, memoryRootExample
		writeJSON(w, out)
		return
	}
	if err != nil {
		memoryError(w, r, err)
		return
	}
	out.Enabled = true
	if agents != nil {
		out.Agents = agents
	}
	out.Layout, out.Owner = m.Layout(r.Context()), agent.DefaultOwner()
	writeJSON(w, out)
}

// POST /memory/migrate {"owner":"alice","confirm":true}
func (s *Server) memoryMigrate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.memoryReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q MemoryMigrateRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	if q.Owner == "" {
		q.Owner = agent.DefaultOwner()
	}
	if !confirmed(w, r, q.Confirm, "confirm.memory.migrate", q.Owner) {
		return
	}
	moved, err := m.Migrate(r.Context(), q.Owner)
	if err != nil {
		memoryError(w, r, err)
		return
	}
	if moved == nil {
		moved = []string{}
	}
	writeJSON(w, MemoryMigrateResponse{Owner: q.Owner, Moved: moved, Layout: m.Layout(r.Context())})
}

// GET /memory/merge?agent=&name=&conflict=
func (s *Server) memoryMerge(w http.ResponseWriter, r *http.Request) {
	m, ok := s.memoryReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	q := r.URL.Query()
	res, err := m.Merge(r.Context(), q.Get("agent"), q.Get("name"), q.Get("conflict"), "")
	if err != nil {
		if errors.Is(err, memory.ErrNoConflict) {
			httpErrorT(w, r, http.StatusConflict, "err.memory.no_conflict")
			return
		}
		memoryError(w, r, err)
		return
	}
	writeJSON(w, res)
}

// memoryByPath dispatches everything under /memory/: the search box, an
// agent's listing, and one fact's read, write and delete.
func (s *Server) memoryByPath(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/memory/")
	switch tail {
	case "search":
		s.memorySearch(w, r)
		return
	case "migrate":
		s.memoryMigrate(w, r)
		return
	case "merge":
		s.memoryMerge(w, r)
		return
	case "":
		http.NotFound(w, r)
		return
	}
	// /memory/<agent>, /memory/<agent>/<name>; in layout v2 the agent is
	// owner/agent, so the fact is the third segment and a two-segment
	// path is a listing unless the first segment is shared.
	segs := strings.Split(tail, "/")
	v2 := s.collector.Memory != nil && s.collector.Memory.Layout(r.Context()) == memory.LayoutV2
	switch {
	case len(segs) == 1:
		s.memoryList(w, r, segs[0])
	case len(segs) == 2 && v2 && segs[0] != memory.SharedAgent:
		s.memoryList(w, r, segs[0]+"/"+segs[1])
	case len(segs) == 2:
		s.memoryFact(w, r, segs[0], segs[1])
	case len(segs) == 3 && v2:
		s.memoryFact(w, r, segs[0]+"/"+segs[1], segs[2])
	case len(segs) == 3:
		// A fact name with a slash in v1: refused as a bad name, like
		// every other name the rule excludes.
		s.memoryFact(w, r, segs[0], segs[1]+"/"+segs[2])
	default:
		http.NotFound(w, r)
	}
}

// GET /memory/{agent}?cursor=&limit=
func (s *Server) memoryList(w http.ResponseWriter, r *http.Request, agent string) {
	m, ok := s.memoryReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	if !memoryName(w, r, "agent", agent) {
		return
	}
	limit, ok := queryLimit(w, r, defaultMemoryListLimit, maxMemoryListLimit)
	if !ok {
		return
	}
	facts, next, err := m.List(r.Context(), agent, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		memoryError(w, r, err)
		return
	}
	if facts == nil {
		facts = []memory.FactMeta{}
	}
	writeJSON(w, MemoryListResponse{Agent: agent, Facts: facts, NextCursor: next})
}

// GET|PUT|DELETE /memory/{agent}/{name}
func (s *Server) memoryFact(w http.ResponseWriter, r *http.Request, agent, name string) {
	m, ok := s.memoryReady(w, r, http.MethodGet, http.MethodPut, http.MethodDelete)
	if !ok {
		return
	}
	if !memoryName(w, r, "agent", agent) || !memoryName(w, r, "name", name) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		f, err := m.Get(r.Context(), agent, name)
		if err != nil {
			memoryError(w, r, err)
			return
		}
		writeJSON(w, f)
	case http.MethodPut:
		var q MemoryPutRequest
		if !decodeMutationLimit(w, r, &q, maxMemoryPutRequest) {
			return
		}
		f, err := m.Put(r.Context(), agent, name, q.Content, memory.PutOptions{
			Mode: q.Mode, ExpectedVersion: q.ExpectedVersion, ExpectedRemoteVersion: q.ExpectedRemoteVersion, Description: q.Description, Type: q.Type,
		})
		if errors.Is(err, memory.ErrVersionChanged) {
			// The version to re-read is what the fact has now; a fact
			// deleted meanwhile has none.
			current := ""
			if cur, gerr := m.Get(r.Context(), agent, name); gerr == nil {
				current = cur.Version
			}
			writeJSONStatus(w, http.StatusConflict, MemoryConflictResponse{Error: err.Error(), CurrentVersion: current})
			return
		}
		if err != nil {
			memoryError(w, r, err)
			return
		}
		writeJSON(w, f)
	case http.MethodDelete:
		var q MemoryDeleteRequest
		if !decodeMutationLimit(w, r, &q, maxMemoryRequest) {
			return
		}
		if !confirmed(w, r, q.Confirm, "confirm.memory.delete", agent, name) {
			return
		}
		if err := m.Delete(r.Context(), agent, name); err != nil {
			memoryError(w, r, err)
			return
		}
		writeJSON(w, MemoryDeleteResponse{Agent: agent, Name: name, Path: factPath(m, agent, name), OK: true})
	}
}

// writeJSONStatus is writeJSON with a status other than 200, for the one
// refusal whose body is structured: the console reads current_version out
// of a 409 to offer "reload".
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// factPath is <root>/memory/<agent>/facts/<name>.md, the path the delete
// reply names; the store's own helper is not on the interface.
func factPath(m MemoryControl, agent, name string) string {
	return strings.TrimSuffix(m.Root(), "/") + "/memory/" + agent + "/facts/" + name + ".md"
}

// GET /memory/search?q=&agent=&include_shared=&top_k=&mode=
//
// agent defaults to shared; include_shared defaults to true, as for
// memory_search.
func (s *Server) memorySearch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.memoryReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	params := r.URL.Query()
	opt := memory.SearchOptions{Query: params.Get("q"), Agent: params.Get("agent"), IncludeShared: true, TopK: defaultMemorySearchTopK, Mode: params.Get("mode")}
	if opt.Query == "" {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query")
		return
	}
	if opt.Agent == "" {
		opt.Agent = memory.SharedAgent
	}
	if !memoryName(w, r, "agent", opt.Agent) {
		return
	}
	switch params.Get("include_shared") {
	case "", "1", "true":
	case "0", "false":
		opt.IncludeShared = false
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	switch opt.Mode {
	case "", "keyword", "hybrid", "vector":
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	if raw := params.Get("top_k"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxMemorySearchTopK {
			httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", maxMemorySearchTopK)
			return
		}
		opt.TopK = n
	}
	res, err := m.Search(r.Context(), opt)
	if err != nil {
		memoryError(w, r, err)
		return
	}
	if res.Hits == nil {
		res.Hits = []memory.SearchHit{}
	}
	writeJSON(w, res)
}
