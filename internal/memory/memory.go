// Package memory is the agent memory store of docs/agent-roadmap.md §3.11:
// one Markdown file per fact under <root>/memory/<agent>/facts/, an index
// line per fact in <root>/memory/<agent>/MEMORY.md, and memory/shared/ with
// the same shape for facts every agent may read. It is a thin layer over
// the VFS and the content index: the drive synchronises the files across
// devices, the existing conflict-copy mechanism keeps both sides of a
// concurrent write, and the built-in index rule (IndexRule) makes the facts
// searchable. The MCP tools and the control plane share this one
// implementation, so a fact written by either obeys the same names,
// limits and version check.
//
// The store knows nothing about scopes: the caller decides whether the
// root is readable or writable for the session at hand and asks the store
// only once it is.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/index"
	"cloudfs/internal/vfs"
)

// FS is what the store needs from the VFS; *vfs.FS satisfies it.
type FS interface {
	StatPath(ctx context.Context, p string) (vfs.Attr, error)
	ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error)
	WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (vfs.Attr, error)
	ReadDirPath(ctx context.Context, p string) ([]vfs.Attr, error)
	ReadDirPagePath(ctx context.Context, p string, opt vfs.DirectoryPageOptions) (vfs.DirectoryPage, error)
	Mkdir(ctx context.Context, parent uint64, name string) (vfs.Attr, error)
	Remove(ctx context.Context, parent uint64, name string, recursive bool) error
	Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error
	// RemoteVersionOf is the version of a path as the provider last
	// reported it, "" when the file is local-only or unknown: what a
	// put may compare so that a change from another device is noticed
	// even while this device's own upload is in flight.
	RemoteVersionOf(ctx context.Context, p string) (string, error)
}

// Searcher is the content index; nil means memory_search cannot run.
type Searcher interface {
	Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error)
}

// Options configures New.
type Options struct {
	FS       FS
	Searcher Searcher
	Config   config.Memory
	// Now is the clock updated_at is stamped from; nil means time.Now.
	Now func() time.Time
}

// Store reads and writes facts.
type Store struct {
	fs  FS
	idx Searcher
	cfg config.Memory
	now func() time.Time
	// mu serialises writes so the version check and the write of one put
	// are not interleaved with another put in this process. Across
	// processes and devices the drive's conflict copies are the safety net.
	mu sync.Mutex
	// layout caches the marker read (Layout).
	layoutMu sync.Mutex
	layout   string
	layoutAt time.Time
	// reviewMu serialises candidate proposal/review transitions. Fact writes
	// keep using mu, so accepting a candidate never deadlocks with Put.
	reviewMu sync.Mutex
}

// The store's errors. Callers match on them to choose a status code or a
// message; the text of each is what an agent sees.
var (
	ErrNoRoot         = errors.New("memory.root is not configured; set memory.root (or mcp.workspace) to a directory inside mcp.allow")
	ErrBadName        = errors.New("names are lower-case letters, digits and dashes, 1 to 64 characters, starting with a letter or digit")
	ErrNotFound       = errors.New("no such memory")
	ErrTooLarge       = errors.New("memory too large")
	ErrVersionChanged = errors.New("memory changed elsewhere; re-read")
	ErrNoIndex        = errors.New("memory_search needs the content index, but index.enabled: false; enable it in the configuration")
	ErrInvalidCursor  = errors.New("invalid cursor")
	ErrBadMode        = errors.New("mode must be replace or append")
)

// SharedAgent is the drive-wide pseudo-agent whose facts every agent may
// search. PersonalAgent is the tool alias for an owner's cross-agent area;
// in layout v2 it resolves to <owner>/shared and in v1 to SharedAgent.
const SharedAgent = "shared"
const PersonalAgent = "personal"

// defaultListLimit is the page size List uses when given none.
const defaultListLimit = 100

// New builds a store. A nil FS is a programming error and panics at the
// first call; an empty Config.Root makes every call answer ErrNoRoot.
func New(opt Options) *Store {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Config.MaxFactBytes <= 0 {
		opt.Config.MaxFactBytes = config.DefaultMemoryMaxFactBytes
	}
	if opt.Config.MaxAgentBytes <= 0 {
		opt.Config.MaxAgentBytes = config.DefaultMemoryMaxAgentBytes
	}
	return &Store{fs: opt.FS, idx: opt.Searcher, cfg: opt.Config, now: opt.Now}
}

// Root is the configured memory root, "" when there is none.
func (s *Store) Root() string { return s.cfg.Root }

// Config is the limits the store enforces.
func (s *Store) Config() config.Memory { return s.cfg }

// HasIndex reports whether Search can run.
func (s *Store) HasIndex() bool { return s.idx != nil }

// AgentDir is <root>/memory/<agent>.
func (s *Store) AgentDir(agent string) string { return path.Join(s.cfg.Root, "memory", agent) }

// FactsDir is <root>/memory/<agent>/facts.
func (s *Store) FactsDir(agent string) string { return path.Join(s.AgentDir(agent), "facts") }

// FactPath is <root>/memory/<agent>/facts/<name>.md.
func (s *Store) FactPath(agent, name string) string {
	return path.Join(s.FactsDir(agent), name+".md")
}

// IndexPath is <root>/memory/<agent>/MEMORY.md.
func (s *Store) IndexPath(agent string) string { return path.Join(s.AgentDir(agent), "MEMORY.md") }

// IndexRule is the built-in index rule that makes every fact and MEMORY.md
// under root searchable. Source "builtin" marks it as following the
// configuration: the index tools and routes refuse to remove it.
func IndexRule(root string) index.Rule {
	return index.Rule{Path: path.Join(root, "memory"), Include: []string{"**/*.md"}, Exclude: []string{"**/candidates/**"}, Source: index.SourceBuiltin}
}

// NormalizeAgent turns a client or principal name into an agent directory
// name: lower-case, runs of anything outside [a-z0-9] become one dash,
// dashes are trimmed at both ends, and an empty result becomes "agent".
// "Claude Code" → "claude-code", "OpenClaw_v2" → "openclaw-v2".
func NormalizeAgent(clientName string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(clientName) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "-")
	}
	if name == "" {
		return "agent"
	}
	return name
}

// AgentSummary is one agent directory as Agents reports it.
type AgentSummary struct {
	// Name is the key: "agent" in v1 and for shared, "owner/agent" in v2.
	Name string `json:"name"`
	// Owner is the person the agent acts for (v2); "" in v1 and for shared.
	Owner string `json:"owner,omitempty"`
	// Facts is the number of well-formed fact files under facts/.
	Facts int `json:"facts"`
	// Bytes is the size of everything under facts/, conflict copies
	// included, against MaxBytes (memory.max_agent_bytes).
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
	// Conflicts counts the entries of facts/ that are not fact files.
	Conflicts int `json:"conflicts"`
}

// FactMeta is one fact as List reports it: the file, its frontmatter and
// the conflict copies beside it.
type FactMeta struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Meta  Meta   `json:"meta"`
	// Conflicts are the paths of the conflict copies of this fact.
	Conflicts []string `json:"conflicts"`
}

// Fact is a fact with its content, as Get and Put return it.
type Fact struct {
	FactMeta
	// Content is the body below the frontmatter.
	Content string `json:"content"`
	// Version identifies the file's current bytes; Put's ExpectedVersion
	// compares against it. It is derived from the content, not from the
	// drive's version, so it stays the same while an upload is in flight
	// and changes exactly when the file does.
	Version string `json:"version"`
	// RemoteVersion is the drive's version of the file as last seen,
	// "" while the file exists only locally. Put's ExpectedRemoteVersion
	// compares against it: it moves when another device's write lands,
	// and not when this device's own write is still uploading.
	RemoteVersion string `json:"remote_version,omitempty"`
}

// PutOptions tunes Put.
type PutOptions struct {
	// Mode is replace (default) or append.
	Mode string
	// ExpectedVersion, when set, must equal the fact's current Version or
	// the put is refused with ErrVersionChanged.
	ExpectedVersion string
	// ExpectedAbsent refuses when a fact exists. Candidate acceptance uses it
	// to make "proposed before creation" atomic with the eventual write.
	ExpectedAbsent bool
	// ExpectedRemoteVersion, when set, must equal the fact's current
	// RemoteVersion: a change that landed from another device refuses the
	// put even when the content happens to hash the same, and this
	// device's own pending upload does not. With both set, a moved remote
	// version refuses first; an unchanged one falls back to the content.
	ExpectedRemoteVersion string
	// Description and Type go into the frontmatter; an empty one keeps
	// what the file already says.
	Description string
	Type        string
	// Scope optionally binds a fact to a project or workspace. SourcePaths
	// and SourceSession retain its evidence; ExpiresAt lets retrieval ignore
	// knowledge that is no longer current.
	Scope         string
	SourcePaths   []string
	SourceSession string
	Replaces      []string
	ExpiresAt     *time.Time
}

// SearchOptions tunes Search.
type SearchOptions struct {
	Query string
	Agent string
	// IncludeShared adds memory/shared to the roots.
	IncludeShared bool
	TopK          int
	// ScanLimit bounds ranked candidates considered before Accept. Accept is
	// applied before a hit counts toward TopK.
	ScanLimit int
	Accept    func(SearchHit) bool
	// Mode is passed to the index: keyword, hybrid or vector.
	Mode string
	// MaxSnippetBytes and MaxBytes bound the response like semantic_search.
	MaxSnippetBytes int
	MaxBytes        int64
}

// SearchHit is an index hit with the agent and fact it belongs to.
type SearchHit struct {
	index.Hit
	Agent string `json:"agent,omitempty"`
	Name  string `json:"name,omitempty"`
}

// SearchResult is what Search returns.
type SearchResult struct {
	Hits      []SearchHit `json:"hits"`
	ModeUsed  string      `json:"mode_used"`
	Degraded  string      `json:"degraded,omitempty"`
	Truncated bool        `json:"truncated"`
	Pending   int         `json:"pending"`
}

func (s *Store) check(agent, name string, needName bool) error {
	if s.cfg.Root == "" {
		return ErrNoRoot
	}
	if !validKey(agent) {
		return fmt.Errorf("agent %q: %w", agent, ErrBadName)
	}
	if needName && !ValidName(name) {
		return fmt.Errorf("name %q: %w", name, ErrBadName)
	}
	return nil
}

// validKey accepts a v1 agent name or a v2 "owner/agent" key; whether the
// layout allows a qualified key is ParseKey's decision, made by the
// caller with the layout in hand.
func validKey(agent string) bool {
	owner, name, qualified := strings.Cut(agent, "/")
	if !qualified {
		return ValidName(agent)
	}
	return ValidName(owner) && ValidName(name) && owner != SharedAgent
}

// Version is the version string of a fact file's bytes.
func Version(file []byte) string {
	sum := sha256.Sum256(file)
	return hex.EncodeToString(sum[:12])
}

// Agents lists the agent directories under <root>/memory with their usage.
// A root or memory directory that does not exist yet is an empty list.
func (s *Store) Agents(ctx context.Context) ([]AgentSummary, error) {
	if s.cfg.Root == "" {
		return nil, ErrNoRoot
	}
	dirs, err := s.fs.ReadDirPath(ctx, path.Join(s.cfg.Root, "memory"))
	if errors.Is(err, vfs.ErrNotFound) {
		return []AgentSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []AgentSummary{}
	v2 := s.Layout(ctx) == LayoutV2
	summarise := func(key, owner string) error {
		sum := AgentSummary{Name: key, Owner: owner, MaxBytes: int64(s.cfg.MaxAgentBytes)}
		files, err := s.factFiles(ctx, key)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Name)
			sum.Bytes += f.Size
			if _, ok := factName(f.Name); ok {
				sum.Facts++
			}
		}
		sum.Conflicts = countConflicts(names)
		out = append(out, sum)
		return nil
	}
	for _, d := range dirs {
		if !d.IsDir || !ValidName(d.Name) {
			continue
		}
		if !v2 || d.Name == SharedAgent {
			if err := summarise(d.Name, ""); err != nil {
				return nil, err
			}
			continue
		}
		// v2: memory/<owner>/<agent>/. A v1 directory not yet migrated
		// (facts/ directly under it) is reported as it is, so a person
		// sees it and Migrate can be asked to finish.
		sub, err := s.fs.ReadDirPath(ctx, path.Join(s.cfg.Root, "memory", d.Name))
		if err != nil && !errors.Is(err, vfs.ErrNotFound) {
			return nil, err
		}
		nested := false
		for _, a := range sub {
			if a.IsDir && ValidName(a.Name) && a.Name != "facts" {
				nested = true
				if err := summarise(d.Name+"/"+a.Name, d.Name); err != nil {
					return nil, err
				}
			}
		}
		if !nested {
			if err := summarise(d.Name, ""); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// factFiles lists the files of an agent's facts/ directory; a missing
// directory is an empty list.
func (s *Store) factFiles(ctx context.Context, agent string) ([]vfs.Attr, error) {
	entries, err := s.fs.ReadDirPath(ctx, s.FactsDir(agent))
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files := entries[:0]
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	return files, nil
}

// List pages through an agent's facts in name order, limit per page
// (default 100), with the frontmatter of each parsed from its first 4 KiB.
// next is the cursor of the following page, "" at the end. Conflict copies
// are paired within the page: a copy that sorts onto the neighbouring
// page is reported by Get, which reads the whole directory.
func (s *Store) List(ctx context.Context, agent, cursor string, limit int) (facts []FactMeta, next string, err error) {
	if err := s.check(agent, "", false); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, vfs.MaxDirectoryPageSize)
	opt, err := vfs.ParseDirectoryCursor(cursor, limit)
	if errors.Is(err, vfs.ErrInvalidCursor) {
		return nil, "", ErrInvalidCursor
	}
	if err != nil {
		return nil, "", err
	}
	opt.Count = false
	page, err := s.fs.ReadDirPagePath(ctx, s.FactsDir(agent), opt)
	if errors.Is(err, vfs.ErrNotFound) {
		return []FactMeta{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		if !e.IsDir {
			names = append(names, e.Name)
		}
	}
	facts = []FactMeta{}
	for _, e := range page.Entries {
		name, ok := factName(e.Name)
		if e.IsDir || !ok {
			continue
		}
		fm := FactMeta{Name: name, Agent: agent, Path: path.Join(s.FactsDir(agent), e.Name), Size: e.Size, Conflicts: s.conflictPaths(agent, names, name)}
		head, err := s.fs.ReadFileRange(ctx, fm.Path, 0, frontmatterHead)
		if err != nil && !errors.Is(err, vfs.ErrNotFound) {
			return nil, "", err
		}
		fm.Meta, _, _ = parseFrontmatter(head)
		if fm.Meta.Name == "" {
			fm.Meta.Name = name
		}
		facts = append(facts, fm)
	}
	return facts, vfs.NextDirectoryCursor(page), nil
}

func (s *Store) conflictPaths(agent string, names []string, name string) []string {
	out := []string{}
	for _, n := range conflictsOf(names, name) {
		out = append(out, path.Join(s.FactsDir(agent), n))
	}
	return out
}

// readFact reads a fact file whole. found is false for ErrNotFound.
func (s *Store) readFact(ctx context.Context, agent, name string) (file []byte, found bool, err error) {
	file, err = s.fs.ReadFileRange(ctx, s.FactPath(agent, name), 0, 0)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return file, true, nil
}

// Get returns a fact with its content, version and conflict copies.
func (s *Store) Get(ctx context.Context, agent, name string) (Fact, error) {
	if err := s.check(agent, name, true); err != nil {
		return Fact{}, err
	}
	file, found, err := s.readFact(ctx, agent, name)
	if err != nil {
		return Fact{}, err
	}
	if !found {
		return Fact{}, fmt.Errorf("%w: %s/%s", ErrNotFound, agent, name)
	}
	f := s.factOf(agent, name, file)
	f.RemoteVersion = s.remoteVersion(ctx, agent, name)
	files, err := s.factFiles(ctx, agent)
	if err != nil {
		return Fact{}, err
	}
	names := make([]string, 0, len(files))
	for _, e := range files {
		names = append(names, e.Name)
	}
	f.Conflicts = s.conflictPaths(agent, names, name)
	return f, nil
}

func (s *Store) factOf(agent, name string, file []byte) Fact {
	m, body, _ := parseFrontmatter(file)
	if m.Name == "" {
		m.Name = name
	}
	return Fact{
		FactMeta: FactMeta{Name: name, Agent: agent, Path: s.FactPath(agent, name), Size: int64(len(file)), Meta: m, Conflicts: []string{}},
		Content:  body, Version: Version(file),
	}
}

// remoteVersion asks the FS for the drive's version of a fact, "" when
// it cannot say.
func (s *Store) remoteVersion(ctx context.Context, agent, name string) string {
	v, err := s.fs.RemoteVersionOf(ctx, s.FactPath(agent, name))
	if err != nil {
		return ""
	}
	return v
}

// Put creates or updates a fact: it checks the name, the version the
// caller expects, the fact size and the agent budget, writes
// facts/<name>.md with its frontmatter, then makes MEMORY.md point at it
// (the one matching line is replaced, otherwise a line is appended).
func (s *Store) Put(ctx context.Context, agent, name, content string, opt PutOptions) (Fact, error) {
	if err := s.check(agent, name, true); err != nil {
		return Fact{}, err
	}
	switch opt.Mode {
	case "", "replace", "append":
	default:
		return Fact{}, fmt.Errorf("%w, got %q", ErrBadMode, opt.Mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, found, err := s.readFact(ctx, agent, name)
	if err != nil {
		return Fact{}, err
	}
	var cur Fact
	if found {
		cur = s.factOf(agent, name, existing)
		cur.RemoteVersion = s.remoteVersion(ctx, agent, name)
	}
	if opt.ExpectedRemoteVersion != "" && (!found || cur.RemoteVersion != opt.ExpectedRemoteVersion) {
		return Fact{}, fmt.Errorf("%w on the drive (remote version %q, expected %q)", ErrVersionChanged, cur.RemoteVersion, opt.ExpectedRemoteVersion)
	}
	if opt.ExpectedAbsent && found {
		return Fact{}, fmt.Errorf("%w: fact %s/%s was created after the write was prepared", ErrVersionChanged, agent, name)
	}
	if opt.ExpectedVersion != "" && (!found || cur.Version != opt.ExpectedVersion) {
		return Fact{}, fmt.Errorf("%w (current version %q)", ErrVersionChanged, cur.Version)
	}
	body := content
	if opt.Mode == "append" && cur.Content != "" {
		body = cur.Content
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += content
	}
	m := Meta{Name: name, Description: opt.Description, Type: opt.Type, Scope: opt.Scope,
		SourcePaths: append([]string(nil), opt.SourcePaths...), SourceSession: opt.SourceSession,
		Replaces: append([]string(nil), opt.Replaces...), ExpiresAt: opt.ExpiresAt, UpdatedAt: s.now()}
	if m.Description == "" {
		m.Description = cur.Meta.Description
	}
	if m.Type == "" {
		m.Type = cur.Meta.Type
	}
	if m.Scope == "" {
		m.Scope = cur.Meta.Scope
	}
	if len(m.SourcePaths) == 0 {
		m.SourcePaths = append([]string(nil), cur.Meta.SourcePaths...)
	}
	if m.SourceSession == "" {
		m.SourceSession = cur.Meta.SourceSession
	}
	if len(m.Replaces) == 0 {
		m.Replaces = append([]string(nil), cur.Meta.Replaces...)
	}
	if m.ExpiresAt == nil {
		m.ExpiresAt = cur.Meta.ExpiresAt
	}
	file := []byte(render(m, body))
	if !frontmatterFits(file) {
		return Fact{}, fmt.Errorf("%w: %s/%s frontmatter exceeds the %d-byte readable header", ErrTooLarge, agent, name, frontmatterHead)
	}
	if int64(len(file)) > int64(s.cfg.MaxFactBytes) {
		return Fact{}, fmt.Errorf("%w: %s/%s would be %d bytes with its frontmatter, over memory.max_fact_bytes (%d)", ErrTooLarge, agent, name, len(file), s.cfg.MaxFactBytes)
	}
	files, err := s.factFiles(ctx, agent)
	if err != nil {
		return Fact{}, err
	}
	var used int64
	count := 0
	for _, f := range files {
		used += f.Size
		if _, ok := factName(f.Name); ok {
			count++
		}
	}
	after := used - cur.Size + int64(len(file))
	if after > int64(s.cfg.MaxAgentBytes) {
		return Fact{}, fmt.Errorf("%w: agent %s would use %d bytes, over memory.max_agent_bytes (%d); currently %d bytes in %d facts", ErrTooLarge, agent, after, s.cfg.MaxAgentBytes, used, count)
	}
	if err := s.mkdirAll(ctx, s.FactsDir(agent)); err != nil {
		return Fact{}, err
	}
	if _, err := s.fs.WriteFile(ctx, s.FactPath(agent, name), file, false); err != nil {
		return Fact{}, err
	}
	if err := s.editIndex(ctx, agent, func(text string) string { return upsertIndexLine(text, name, m.Description) }); err != nil {
		return Fact{}, err
	}
	out := s.factOf(agent, name, file)
	out.RemoteVersion = s.remoteVersion(ctx, agent, name)
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	out.Conflicts = s.conflictPaths(agent, names, name)
	return out, nil
}

// Delete removes a fact file and every MEMORY.md line pointing at it.
// Conflict copies stay: the agent reads and removes them itself.
func (s *Store) Delete(ctx context.Context, agent, name string) error {
	if err := s.check(agent, name, true); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.fs.StatPath(ctx, s.FactsDir(agent))
	if errors.Is(err, vfs.ErrNotFound) {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, agent, name)
	}
	if err != nil {
		return err
	}
	if err := s.fs.Remove(ctx, dir.Ino, name+".md", false); errors.Is(err, vfs.ErrNotFound) {
		return fmt.Errorf("%w: %s/%s", ErrNotFound, agent, name)
	} else if err != nil {
		return err
	}
	return s.editIndex(ctx, agent, func(text string) string { return removeIndexLines(text, name) })
}

// editIndex rewrites MEMORY.md through edit, writing only when the text
// changed and creating the file when there is none.
func (s *Store) editIndex(ctx context.Context, agent string, edit func(string) string) error {
	p := s.IndexPath(agent)
	old, err := s.fs.ReadFileRange(ctx, p, 0, 0)
	if err != nil && !errors.Is(err, vfs.ErrNotFound) {
		return err
	}
	text := edit(string(old))
	if err == nil && text == string(old) {
		return nil
	}
	_, err = s.fs.WriteFile(ctx, p, []byte(text), false)
	return err
}

// mkdirAll creates every missing component of p.
func (s *Store) mkdirAll(ctx context.Context, p string) error {
	if p == "/" {
		return nil
	}
	if a, err := s.fs.StatPath(ctx, p); err == nil {
		if a.IsDir {
			return nil
		}
		return fmt.Errorf("%s exists and is not a directory", p)
	} else if !errors.Is(err, vfs.ErrNotFound) {
		return err
	}
	parent := path.Dir(p)
	if err := s.mkdirAll(ctx, parent); err != nil {
		return err
	}
	pa, err := s.fs.StatPath(ctx, parent)
	if err != nil {
		return err
	}
	if _, err := s.fs.Mkdir(ctx, pa.Ino, path.Base(p)); err != nil && !errors.Is(err, vfs.ErrExists) {
		return err
	}
	return nil
}

// Search runs the index over memory/<agent>, the owner's cross-agent memory
// in v2, and memory/shared when asked, then names the agent and fact of every
// hit.
func (s *Store) Search(ctx context.Context, opt SearchOptions) (SearchResult, error) {
	if err := s.check(opt.Agent, "", false); err != nil {
		return SearchResult{}, err
	}
	if s.idx == nil {
		return SearchResult{}, ErrNoIndex
	}
	roots := []string{s.AgentDir(opt.Agent)}
	if opt.IncludeShared {
		owner, _, qualified := strings.Cut(opt.Agent, "/")
		if qualified {
			personal := owner + "/" + SharedAgent
			if opt.Agent != personal {
				roots = append(roots, s.AgentDir(personal))
			}
		}
		if opt.Agent != SharedAgent {
			roots = append(roots, s.AgentDir(SharedAgent))
		}
	}
	query := index.SearchQuery{
		Query: opt.Query, Roots: roots, TopK: opt.TopK, Mode: opt.Mode,
		ScanLimit: opt.ScanLimit, MaxSnippetBytes: opt.MaxSnippetBytes, MaxBytes: opt.MaxBytes,
	}
	if opt.Accept != nil {
		query.Accept = func(h index.Hit) bool {
			agent, name := s.locate(h.Path)
			return opt.Accept(SearchHit{Hit: h, Agent: agent, Name: name})
		}
	}
	res, err := s.idx.Search(ctx, query)
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Hits: []SearchHit{}, ModeUsed: res.ModeUsed, Degraded: res.Degraded, Truncated: res.Truncated, Pending: res.Pending}
	for _, h := range res.Hits {
		sh := SearchHit{Hit: h}
		sh.Agent, sh.Name = s.locate(h.Path)
		out.Hits = append(out.Hits, sh)
	}
	return out, nil
}

// locate names the agent and fact a path under the memory tree belongs
// to; the fact is "" for MEMORY.md and anything that is not a fact file.
func (s *Store) locate(p string) (agent, name string) {
	rel, ok := strings.CutPrefix(p, path.Join(s.cfg.Root, "memory")+"/")
	if !ok {
		return "", ""
	}
	agent, rest, _ := strings.Cut(rel, "/")
	// v2: memory/<owner>/<agent>/facts/… — the key is owner/agent.
	if !strings.HasPrefix(rest, "facts/") && rest != "MEMORY.md" && agent != SharedAgent {
		if sub, more, ok := strings.Cut(rest, "/"); ok && ValidName(sub) {
			agent, rest = agent+"/"+sub, more
		}
	}
	if file, ok := strings.CutPrefix(rest, "facts/"); ok {
		if n, ok := factName(file); ok && !strings.Contains(file, "/") {
			return agent, n
		}
	}
	return agent, ""
}

// --- MEMORY.md lines ---

// indexLine is the MEMORY.md line of a fact: "- [name](facts/name.md) —
// description", without the description when there is none.
func indexLine(name, description string) string {
	line := fmt.Sprintf("- [%s](facts/%s.md)", name, name)
	if description != "" {
		line += " — " + description
	}
	return line
}

// linkOf is the part of a line that identifies the fact it points at.
func linkOf(name string) string { return fmt.Sprintf("](facts/%s.md)", name) }

// pointsAt reports whether a MEMORY.md line links to the fact name.
func pointsAt(line, name string) bool {
	return strings.Contains(line, linkOf(name))
}

// upsertIndexLine returns text with the fact's line set: replaced when
// exactly one line points at the fact, appended otherwise (none, or more
// than one, in which case nothing is guessed at). The result always ends
// with a newline.
func upsertIndexLine(text, name, description string) string {
	line := indexLine(name, description)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	matches := 0
	at := -1
	for i, l := range lines {
		if pointsAt(l, name) {
			matches++
			at = i
		}
	}
	if matches == 1 {
		lines[at] = line
	} else {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

// removeIndexLines drops every line pointing at the fact.
func removeIndexLines(text, name string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !pointsAt(l, name) {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}
