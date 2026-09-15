// Package mcpsrv exposes the VFS over the Model Context Protocol so agents
// such as Claude Code and Codex can read and write mounted cloud storage
// without going through the kernel. Every tool paginates, caps its response
// size and validates paths against an allowlist (docs/DESIGN.md §4.7).
package mcpsrv

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"cloudfs/internal/agent"
	"cloudfs/internal/index"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Limits bound what a single tool call may return, so a result never blows
// past a client's context budget.
type Limits struct {
	// MaxBytes caps a single read (default 256 KiB).
	MaxBytes int
	// MaxRangeBytes caps read_range (default 4 MiB).
	MaxRangeBytes int
	// MaxEntries caps one page of a listing (default 200).
	MaxEntries int
	// MaxResults caps search hits (default 100).
	MaxResults int
}

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 256 << 10
	}
	if l.MaxRangeBytes <= 0 {
		l.MaxRangeBytes = 4 << 20
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 200
	}
	if l.MaxResults <= 0 {
		l.MaxResults = 100
	}
	return l
}

// Options configures the server.
type Options struct {
	FS *vfs.FS
	// Allow lists the mount-relative path prefixes the server may touch. An
	// empty list allows the whole mount.
	Allow []string
	// ReadOnly refuses every mutating tool.
	ReadOnly bool
	// Export, when set, exposes the export tools. nil leaves them
	// unregistered: a server with no export queue behind it should not
	// advertise a tool it cannot run.
	Export ExportJobs
	// ExportRoots bounds where an export may write on this machine. An empty
	// list refuses every destination, which is the safe default for a server
	// whose configuration says nothing about it.
	ExportRoots []string
	Limits      Limits
	// Version is reported to the client.
	Version string
	// Sessions, when set, gives every call a CloudFS session and scope. nil
	// keeps today's behaviour: one process-wide scope from Allow/ReadOnly.
	Sessions *agent.Sessions
	// Scope overrides the scope derived from Allow/ReadOnly. nil derives it.
	Scope *agent.Scope
	// Audit records every tool call. nil uses Sessions.Store(); with both
	// nil nothing is recorded.
	Audit AuditWriter
	// NonOwner says this server runs beside the process that owns the cache
	// (a stdio server started while `cloudfs mount` is up). Its VFS is a
	// separate view, so the tools that need the shared journal and session
	// state refuse with errRequiresOwner and point at the HTTP transport.
	NonOwner bool
	// Workspace is the mount directory begin_session creates session
	// directories under (config mcp.workspace). Empty derives it from the
	// caller's first read prefix plus "/.agent"; a caller who may read the
	// whole mount then gets a configuration error, since the mount root is
	// not writable.
	Workspace string
	// Index, when set, exposes the content index tools (semantic_search,
	// index_status, index, unindex, read_extracted_text). nil leaves them
	// unregistered: with index.enabled false there is no index to search.
	Index IndexService
}

// IndexService is what the index tools need from the daemon's indexer.
type IndexService interface {
	Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error)
	Status(ctx context.Context, p string) (index.Status, error)
	AddRule(ctx context.Context, r index.Rule) error
	RemoveRule(ctx context.Context, p string) error
	Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error)
}

// errRequiresOwner is the refusal a session, rollback or mutating tool
// gives on a NonOwner server (see requireOwner): the work has to happen in
// the process that owns the cache, which the HTTP transport reaches.
var errRequiresOwner = errors.New("requires the storage owner; use the HTTP transport: cloudfs mcp install --transport http")

// Server wraps an MCP server bound to a VFS.
type Server struct {
	opt           Options
	mcp           *mcp.Server
	subscriptions resourceSubscriptions
	copyTools     copyToolState
	// defaultScope is what a call runs under when no session store is
	// configured, and the scope of the principal every stdio, legacy HTTP
	// or loopback session belongs to when one is.
	defaultScope     agent.Scope
	defaultPrincipal agent.Principal
	// audit is where auditMiddleware writes; nil means no audit trail.
	audit AuditWriter

	// principalsMu guards envPrincipalCached, legacyByPrincipal and
	// stdioKeys.
	principalsMu sync.Mutex
	// envPrincipalCached is the principal of the legacy environment token,
	// looked up once on its first request.
	envPrincipalCached *agent.Principal
	// legacyByPrincipal maps a principal to the stateful SDK sessions that
	// authenticated as it, so revoking the principal's token can close them.
	legacyByPrincipal map[string]map[*mcp.ServerSession]struct{}
	// stdioKeys are the connection keys of the stdio (and in-memory)
	// transports this server has resolved a session for, so that
	// FinishStdioSessions can close them after the SDK has already
	// forgotten the connection.
	stdioKeys map[string]struct{}
	// revokeStop ends the revocation poller; revokeWG waits for it.
	revokeStop chan struct{}
	revokeOnce sync.Once
	revokeWG   sync.WaitGroup
}

// revokePollInterval bounds how long a revoked token's stateful session
// survives when the revocation came from another process.
const revokePollInterval = 2 * time.Second

// ErrDenied is returned for paths outside the caller's scope. It is the
// scope package's error so that callers may match either name.
var ErrDenied = agent.ErrDenied

// New builds the MCP server and registers every tool.
func New(opt Options) (*Server, error) {
	if opt.FS == nil {
		return nil, errors.New("mcpsrv: FS is required")
	}
	opt.Limits = opt.Limits.withDefaults()
	opt.Allow = append([]string(nil), opt.Allow...)
	opt.ExportRoots = append([]string(nil), opt.ExportRoots...)
	for i, a := range opt.Allow {
		opt.Allow[i] = normalise(a)
	}
	if opt.Version == "" {
		opt.Version = "0.1.0"
	}
	s := &Server{opt: opt}
	s.defaultScope = agent.Scope{Read: append([]string(nil), opt.Allow...), ReadOnly: opt.ReadOnly}
	if opt.Scope != nil {
		s.defaultScope = *opt.Scope
	}
	if opt.Sessions != nil {
		p, err := opt.Sessions.EnsurePrincipal(context.Background(), "stdio", "local", s.defaultScope)
		if err != nil {
			return nil, fmt.Errorf("mcpsrv: agent principal: %w", err)
		}
		s.defaultPrincipal = p
	}
	s.audit = opt.Audit
	if s.audit == nil && opt.Sessions != nil {
		s.audit = opt.Sessions.Store()
	}
	if err := s.copyTools.init(); err != nil {
		return nil, fmt.Errorf("mcpsrv: initialize copy cursor: %w", err)
	}
	s.subscriptions.init(s)
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "cloudfs",
		Title:   "CloudFS mounted cloud storage",
		Version: opt.Version,
	}, &mcp.ServerOptions{PageSize: opt.Limits.MaxEntries,
		SubscribeHandler:   s.subscriptions.subscribeHook,
		UnsubscribeHandler: s.subscriptions.unsubscribeHook,
		InitializedHandler: s.onInitialized})
	s.mcp.AddReceivingMiddleware(privateResourceResponses)
	s.mcp.AddReceivingMiddleware(s.subscriptions.receive)
	// Each AddReceivingMiddleware wraps the handler built so far, so the last
	// one added runs first. The session must be in the context before the
	// audit row is built and before the subscription reservation checks
	// paths, hence it is added last; the audit sits between the two so the
	// reservation's checks are recorded on the row.
	s.mcp.AddReceivingMiddleware(s.auditMiddleware)
	s.mcp.AddReceivingMiddleware(s.sessionMiddleware)
	s.mcp.AddSendingMiddleware(s.subscriptions.send)
	s.register()
	s.registerCopyTools()
	s.registerUploadTools()
	s.registerExportTools()
	s.registerSessionTools()
	s.registerIndexTools()
	s.registerResources()
	if opt.Sessions != nil {
		s.revokeStop = make(chan struct{})
		s.revokeWG.Add(1)
		go s.watchRevocations(revokePollInterval)
	}
	return s, nil
}

// MCP exposes the underlying server for transports.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Close stops notification delivery, cancels subscription streams, and closes
// remaining client sessions. It is safe to call more than once.
func (s *Server) Close() error {
	if s.revokeStop != nil {
		s.revokeOnce.Do(func() { close(s.revokeStop) })
		s.revokeWG.Wait()
	}
	s.subscriptions.stop()
	for session := range s.mcp.Sessions() {
		_ = session.Close()
	}
	s.subscriptions.wg.Wait()
	return nil
}

// Run serves over the given transport until the context ends.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error { return s.mcp.Run(ctx, t) }

// normalise cleans a mount-relative path.
func normalise(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// entry is one item in a listing or search result.
type entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime,omitempty"`
	// Cached is the fraction of the file present locally, 0..1. A value of 1
	// means reads are served from disk.
	Cached float64 `json:"cached"`
	// State is "synced" or "local" (written here, not uploaded yet).
	State string `json:"state"`
}

func toEntry(dir string, a vfs.Attr) entry {
	kind := "file"
	if a.IsDir {
		kind = "directory"
	}
	state := "synced"
	if a.LocalOnly {
		state = "local"
	}
	e := entry{
		Name: a.Name, Path: path.Join(dir, a.Name), Kind: kind, Size: a.Size,
		Cached: a.Cached, State: state,
	}
	if !a.MTime.IsZero() && a.MTime.Unix() > 0 {
		e.MTime = a.MTime.UTC().Format(time.RFC3339)
	}
	if a.IsDir {
		e.Cached = 0
	}
	return e
}

// --- tool inputs and outputs ---

type listInput struct {
	Path   string `json:"path" jsonschema:"Mount-relative directory path, for example /work/src"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous truncated listing"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum entries to return; the server caps this"`
}

type listOutput struct {
	Path       string  `json:"path"`
	Entries    []entry `json:"entries"`
	Truncated  bool    `json:"truncated"`
	NextCursor string  `json:"next_cursor,omitempty"`
	Total      int     `json:"total"`
}

type statInput struct {
	Path string `json:"path" jsonschema:"Mount-relative path"`
}

type statManyInput struct {
	Paths []string `json:"paths" jsonschema:"Mount-relative paths, at most 100"`
}

type statOutput struct {
	entry
	Remote  string `json:"remote,omitempty"`
	Version string `json:"version,omitempty"`
	Exists  bool   `json:"exists"`
	Error   string `json:"error,omitempty"`
}

type statManyOutput struct {
	Results []statOutput `json:"results"`
}

type readTextInput struct {
	Path     string `json:"path" jsonschema:"Mount-relative file path"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"Byte offset to start at"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"Maximum bytes to return; the server caps this"`
	Head     int    `json:"head,omitempty" jsonschema:"Return only the first N lines"`
	Tail     int    `json:"tail,omitempty" jsonschema:"Return only the last N lines"`
}

type readTextOutput struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Truncated bool   `json:"truncated"`
	// NextOffset is where a follow-up read should start when truncated.
	NextOffset int64 `json:"next_offset,omitempty"`
}

type readRangeInput struct {
	Path   string `json:"path" jsonschema:"Mount-relative file path"`
	Offset int64  `json:"offset" jsonschema:"Byte offset to start at"`
	Length int64  `json:"length" jsonschema:"Number of bytes to read; the server caps this"`
}

type readRangeOutput struct {
	Path      string `json:"path"`
	Base64    string `json:"base64"`
	Bytes     int    `json:"bytes"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Truncated bool   `json:"truncated"`
}

type writeInput struct {
	Path    string `json:"path" jsonschema:"Mount-relative file path"`
	Content string `json:"content" jsonschema:"Text content to write"`
	Mode    string `json:"mode,omitempty" jsonschema:"create, overwrite or append; default overwrite"`
}

type writeOutput struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Size  int64  `json:"size"`
	// State is "local" until the upload queue drains, then "synced".
	State string `json:"state"`
}

type editSpec struct {
	OldText string `json:"old_text" jsonschema:"Exact text to replace; must appear exactly once"`
	NewText string `json:"new_text" jsonschema:"Replacement text"`
}

type editInput struct {
	Path   string     `json:"path" jsonschema:"Mount-relative file path"`
	Edits  []editSpec `json:"edits" jsonschema:"Edits applied in order"`
	DryRun bool       `json:"dry_run,omitempty" jsonschema:"Report the result without writing"`
}

type editOutput struct {
	Path    string `json:"path"`
	Applied int    `json:"applied"`
	DryRun  bool   `json:"dry_run"`
	Diff    string `json:"diff"`
	Size    int64  `json:"size"`
}

type mkdirInput struct {
	Path string `json:"path" jsonschema:"Mount-relative directory path to create"`
}

type moveInput struct {
	From string `json:"from" jsonschema:"Existing mount-relative path"`
	To   string `json:"to" jsonschema:"Destination mount-relative path"`
}

type deleteInput struct {
	Path      string `json:"path" jsonschema:"Mount-relative path to delete"`
	Confirm   bool   `json:"confirm" jsonschema:"Must be true; deletion is irreversible"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"Delete a directory and everything under it"`
}

type okOutput struct {
	Path string `json:"path"`
	OK   bool   `json:"ok"`
}

type searchInput struct {
	Path          string `json:"path,omitempty" jsonschema:"Subtree to search; default the whole mount"`
	Query         string `json:"query,omitempty" jsonschema:"Name query: words are AND-ed substrings, quotes keep spaces, * and ? are wildcards, and ext: size: dm: type: path: filter (e.g. 'report ext:md size:>1m dm:>2026-09'); may be empty when another filter is given"`
	Content       string `json:"content,omitempty" jsonschema:"Also require this text inside the file; only searches locally cached files"`
	Glob          string `json:"glob,omitempty" jsonschema:"Wildcard pattern the whole file name must match, such as *.md"`
	Ext           string `json:"ext,omitempty" jsonschema:"Comma-separated extensions without the dot, any of which matches"`
	MinSize       int64  `json:"min_size,omitempty" jsonschema:"Smallest size in bytes, inclusive"`
	MaxSize       int64  `json:"max_size,omitempty" jsonschema:"Largest size in bytes, inclusive"`
	ModifiedAfter string `json:"modified_after,omitempty" jsonschema:"Only entries modified at or after this RFC 3339 time or YYYY-MM-DD date"`
	Kind          string `json:"kind,omitempty" jsonschema:"dir or file"`
	Sort          string `json:"sort,omitempty" jsonschema:"name, size, mtime or path; prefix - to reverse; default shallowest first. Orders only the hits collected, so with truncated=true the top of the list is not the global top"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"Maximum hits; the server caps this"`
}

type searchHit struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Kind  string    `json:"kind"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	// Cached says the whole file is in the local block cache, which is
	// what content search needs.
	Cached bool `json:"cached"`
	// Line is the first matching line when content was searched.
	Line string `json:"line,omitempty"`
}

type searchCoverage struct {
	// Listed directories are in the index; Known ones exist. The gap is
	// what a search cannot see until warm or the crawler lists it.
	Listed int64 `json:"listed"`
	Known  int64 `json:"known"`
}

type searchOutput struct {
	Hits      []searchHit `json:"hits"`
	Truncated bool        `json:"truncated"`
	// Note explains any limitation that applied, such as content search
	// skipping files that are not cached locally.
	Note     string         `json:"note,omitempty"`
	Coverage searchCoverage `json:"coverage"`
}

type pinInput struct {
	Path string `json:"path" jsonschema:"Mount-relative path to keep fully cached"`
}

type cacheStatusOutput struct {
	Path           string  `json:"path"`
	Cached         float64 `json:"cached"`
	Size           int64   `json:"size"`
	HitRatio       float64 `json:"hit_ratio"`
	CacheBytes     int64   `json:"cache_bytes"`
	PendingUploads int     `json:"pending_uploads"`
}

type rootsOutput struct {
	Roots []rootInfo `json:"roots"`
}

type rootInfo struct {
	Path     string `json:"path"`
	Remote   string `json:"remote"`
	Mode     string `json:"mode"`
	ReadOnly bool   `json:"read_only"`
}

type downloadURLOutput struct {
	Path      string            `json:"path"`
	URL       string            `json:"url"`
	ExpiresAt string            `json:"expires_at,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// text builds the human-readable half of a tool result.
func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

func fail(err error) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}, nil
}

func ptr[T any](v T) *T { return &v }

func (s *Server) register() {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	rw := &mcp.ToolAnnotations{IdempotentHint: true}
	destructive := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_directory",
		Description: "List one directory of the mounted cloud storage. Results are paginated; pass the returned next_cursor to continue.",
		Annotations: ro,
	}, s.listDirectory)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "stat",
		Description: "Return metadata for one path, including how much of the file is cached locally and whether it has been uploaded yet.",
		Annotations: ro,
	}, s.stat)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "stat_many",
		Description: "Return metadata for several paths in one call. Missing paths are reported per entry rather than failing the call.",
		Annotations: ro,
	}, s.statMany)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "read_text",
		Description: "Read a text file. Supports head, tail and byte offsets; large files come back truncated with a next_offset to continue.",
		Annotations: ro,
	}, s.readText)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "read_range",
		Description: "Read a byte range of any file and return it base64-encoded. Use it for binary data or to page through a large file.",
		Annotations: ro,
	}, s.readRange)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search",
		Description: "Find files by name across the mount. Name matching uses a local index and is fast: words AND, quotes keep spaces, * and ? are wildcards, and ext: size: dm: type: path: filter inline or through the structured fields. The index holds only listed directories (see coverage in the output); content matching only inspects files already cached locally.",
		Annotations: ro,
	}, s.search)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "cache_status",
		Description: "Report how much of a path is cached locally, the overall cache hit ratio and how many uploads are still queued.",
		Annotations: ro,
	}, s.cacheStatus)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_roots",
		Description: "List the mounted remotes this server exposes and whether each is writable.",
		Annotations: ro,
	}, s.listRoots)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_download_url",
		Description: "Return a direct download URL for a file, with any headers the provider requires. Use it to fetch large files without routing bytes through this server.",
		Annotations: ro,
	}, s.getDownloadURL)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "write_file",
		Description: "Create, overwrite or append to a text file. The write is durable locally when this returns; the upload to the provider happens in the background unless the mount is in strict mode.",
		Annotations: rw,
	}, s.writeFile)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "edit_file",
		Description: "Apply exact string replacements to a text file. Each old_text must appear exactly once. Use dry_run to preview.",
		Annotations: rw,
	}, s.editFile)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "create_directory",
		Description: "Create a directory, including any missing parents.",
		Annotations: rw,
	}, s.mkdir)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "copy",
		Description: "Copy the last committed file contents to an exact, absent destination, including across remotes. Does not overwrite or recursively copy directories. Writeback returns after local journaling.",
		Annotations: rw,
	}, s.copyFile)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "move",
		Description: "Rename or move a file or directory within the same remote.",
		Annotations: rw,
	}, s.move)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "delete",
		Description: "Delete a file or directory. Requires confirm=true because the deletion also happens on the remote and cannot be undone.",
		Annotations: destructive,
	}, s.deletePath)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "pin",
		Description: "Download a path in full and keep it cached, so later reads and searches over it are local and fast.",
		Annotations: rw,
	}, s.pin)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "unpin",
		Description: "Remove exactly one cache pin rule without deleting files or cached content. Overlapping rules still apply.",
		Annotations: rw,
	}, s.unpin)
}

// --- tool implementations ---

func (s *Server) listDirectory(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, listOutput{}, nil
	}
	limit := in.Limit
	if limit <= 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	opt, err := directoryToolPage(in.Cursor, min(limit, vfs.MaxDirectoryPageSize))
	if err != nil {
		r, _ := fail(err)
		return r, listOutput{}, nil
	}
	page, err := s.opt.FS.ReadDirPagePath(ctx, p, opt)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, listOutput{}, nil
	}
	out := listOutput{Path: p, Total: page.Total, Entries: []entry{}}
	for _, a := range page.Entries {
		out.Entries = append(out.Entries, toEntry(p, a))
	}
	if page.HasMore {
		out.Truncated = true
		out.NextCursor = vfs.NextDirectoryCursor(page)
	}
	msg := fmt.Sprintf("%s: %d entries", p, len(out.Entries))
	if out.Truncated {
		msg += fmt.Sprintf(" (of %d; pass cursor %q for the rest)", out.Total, out.NextCursor)
	}
	return text("%s", msg), out, nil
}

func (s *Server) stat(ctx context.Context, _ *mcp.CallToolRequest, in statInput) (*mcp.CallToolResult, statOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, statOutput{}, nil
	}
	a, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, statOutput{}, nil
	}
	out := statOutput{entry: toEntry(path.Dir(p), a), Remote: a.Remote, Version: a.Version, Exists: true}
	out.Path = p
	out.Name = path.Base(p)
	return text("%s: %s, %d bytes, %s", p, out.Kind, out.Size, out.State), out, nil
}

func (s *Server) statMany(ctx context.Context, _ *mcp.CallToolRequest, in statManyInput) (*mcp.CallToolResult, statManyOutput, error) {
	if len(in.Paths) > 100 {
		r, _ := fail(errors.New("stat_many accepts at most 100 paths"))
		return r, statManyOutput{}, nil
	}
	var out statManyOutput
	for _, raw := range in.Paths {
		p, err := s.checkPath(ctx, raw, false)
		if err != nil {
			out.Results = append(out.Results, statOutput{entry: entry{Path: normalise(raw)}, Error: err.Error()})
			continue
		}
		a, err := s.opt.FS.StatPath(ctx, p)
		if err != nil {
			out.Results = append(out.Results, statOutput{entry: entry{Path: p}, Error: mapErr(err, p).Error()})
			continue
		}
		e := statOutput{entry: toEntry(path.Dir(p), a), Remote: a.Remote, Version: a.Version, Exists: true}
		e.Path = p
		e.Name = path.Base(p)
		out.Results = append(out.Results, e)
	}
	return text("checked %d paths", len(out.Results)), out, nil
}

func (s *Server) readText(ctx context.Context, _ *mcp.CallToolRequest, in readTextInput) (*mcp.CallToolResult, readTextOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, readTextOutput{}, nil
	}
	a, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, readTextOutput{}, nil
	}
	if a.IsDir {
		r, _ := fail(fmt.Errorf("%s is a directory; use list_directory", p))
		return r, readTextOutput{}, nil
	}
	max := in.MaxBytes
	if max <= 0 || max > s.opt.Limits.MaxBytes {
		max = s.opt.Limits.MaxBytes
	}
	// Tail needs the end of the file, so read from a suitable offset.
	offset := in.Offset
	if in.Tail > 0 && in.Offset == 0 && a.Size > int64(max) {
		offset = a.Size - int64(max)
	}
	data, err := s.opt.FS.ReadFileRange(ctx, p, offset, int64(max)+1)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, readTextOutput{}, nil
	}
	truncated := len(data) > max
	if truncated {
		data = data[:max]
	}
	if !utf8.Valid(data) {
		r, _ := fail(fmt.Errorf("%s is not valid UTF-8 text; use read_range for binary data", p))
		return r, readTextOutput{}, nil
	}
	content := string(data)
	if in.Head > 0 {
		content = firstLines(content, in.Head)
	} else if in.Tail > 0 {
		content = lastLines(content, in.Tail)
	}
	out := readTextOutput{
		Path: p, Content: content, Bytes: len(content), Size: a.Size,
		Offset: offset, Truncated: truncated,
	}
	if truncated {
		out.NextOffset = offset + int64(len(data))
	}
	msg := fmt.Sprintf("%s: %d of %d bytes", p, len(content), a.Size)
	if truncated {
		msg += fmt.Sprintf(" (truncated; continue at offset %d)", out.NextOffset)
	}
	return text("%s", msg), out, nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "")
}

func lastLines(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	// A trailing newline produces an empty final element; ignore it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "")
}

func (s *Server) readRange(ctx context.Context, _ *mcp.CallToolRequest, in readRangeInput) (*mcp.CallToolResult, readRangeOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, readRangeOutput{}, nil
	}
	a, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, readRangeOutput{}, nil
	}
	if a.IsDir {
		r, _ := fail(fmt.Errorf("%s is a directory", p))
		return r, readRangeOutput{}, nil
	}
	length := in.Length
	if length <= 0 || length > int64(s.opt.Limits.MaxRangeBytes) {
		length = int64(s.opt.Limits.MaxRangeBytes)
	}
	data, err := s.opt.FS.ReadFileRange(ctx, p, in.Offset, length)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, readRangeOutput{}, nil
	}
	out := readRangeOutput{
		Path: p, Base64: base64.StdEncoding.EncodeToString(data), Bytes: len(data),
		Size: a.Size, Offset: in.Offset,
		Truncated: in.Offset+int64(len(data)) < a.Size,
	}
	return text("%s: %d bytes from offset %d of %d", p, len(data), in.Offset, a.Size), out, nil
}

func (s *Server) writeFile(ctx context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, writeOutput, error) {
	p, err := s.checkPath(ctx, in.Path, true)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, writeOutput{}, nil
	}
	mode := in.Mode
	if mode == "" {
		mode = "overwrite"
	}
	switch mode {
	case "create":
		if _, err := s.opt.FS.StatPath(ctx, p); err == nil {
			r, _ := fail(fmt.Errorf("%s already exists; use mode=overwrite to replace it", p))
			return r, writeOutput{}, nil
		}
	case "overwrite", "append":
	default:
		r, _ := fail(fmt.Errorf("unknown mode %q; use create, overwrite or append", mode))
		return r, writeOutput{}, nil
	}
	a, err := s.opt.FS.WriteFile(ctx, p, []byte(in.Content), mode == "append")
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, writeOutput{}, nil
	}
	state := "synced"
	if a.LocalOnly {
		state = "local"
	}
	out := writeOutput{Path: p, Bytes: len(in.Content), Size: a.Size, State: state}
	return text("wrote %d bytes to %s (%s, now %d bytes)", len(in.Content), p, state, a.Size), out, nil
}

func (s *Server) editFile(ctx context.Context, _ *mcp.CallToolRequest, in editInput) (*mcp.CallToolResult, editOutput, error) {
	p, err := s.checkPath(ctx, in.Path, true)
	if err != nil {
		r, _ := fail(err)
		return r, editOutput{}, nil
	}
	if len(in.Edits) == 0 {
		r, _ := fail(errors.New("no edits given"))
		return r, editOutput{}, nil
	}
	if !in.DryRun {
		if err := s.requireOwner(ctx); err != nil {
			r, _ := fail(err)
			return r, editOutput{}, nil
		}
	}
	data, err := s.opt.FS.ReadFileRange(ctx, p, 0, 0)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, editOutput{}, nil
	}
	if !utf8.Valid(data) {
		r, _ := fail(fmt.Errorf("%s is not valid UTF-8 text", p))
		return r, editOutput{}, nil
	}
	content := string(data)
	var diff strings.Builder
	for i, e := range in.Edits {
		if e.OldText == "" {
			r, _ := fail(fmt.Errorf("edit %d has empty old_text", i+1))
			return r, editOutput{}, nil
		}
		n := strings.Count(content, e.OldText)
		if n == 0 {
			r, _ := fail(fmt.Errorf("edit %d: old_text not found in %s", i+1, p))
			return r, editOutput{}, nil
		}
		if n > 1 {
			r, _ := fail(fmt.Errorf("edit %d: old_text appears %d times in %s; make it unique", i+1, n, p))
			return r, editOutput{}, nil
		}
		content = strings.Replace(content, e.OldText, e.NewText, 1)
		fmt.Fprintf(&diff, "- %s\n+ %s\n", truncateLine(e.OldText), truncateLine(e.NewText))
	}
	out := editOutput{Path: p, Applied: len(in.Edits), DryRun: in.DryRun, Diff: diff.String(), Size: int64(len(content))}
	if in.DryRun {
		return text("dry run: %d edits would apply to %s", len(in.Edits), p), out, nil
	}
	a, err := s.opt.FS.WriteFile(ctx, p, []byte(content), false)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, editOutput{}, nil
	}
	out.Size = a.Size
	return text("applied %d edits to %s", len(in.Edits), p), out, nil
}

func truncateLine(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

func (s *Server) mkdir(ctx context.Context, _ *mcp.CallToolRequest, in mkdirInput) (*mcp.CallToolResult, okOutput, error) {
	p, err := s.checkPath(ctx, in.Path, true)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	if err := s.mkdirAll(ctx, p); err != nil {
		r, _ := fail(mapErr(err, p))
		return r, okOutput{}, nil
	}
	return text("created %s", p), okOutput{Path: p, OK: true}, nil
}

// mkdirAll creates every missing component of p.
func (s *Server) mkdirAll(ctx context.Context, p string) error {
	if p == "/" {
		return nil
	}
	if a, err := s.opt.FS.StatPath(ctx, p); err == nil {
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
	parentAttr, err := s.opt.FS.StatPath(ctx, parent)
	if err != nil {
		return err
	}
	_, err = s.opt.FS.Mkdir(ctx, parentAttr.Ino, path.Base(p))
	if errors.Is(err, vfs.ErrExists) {
		return nil
	}
	return err
}

func (s *Server) move(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, okOutput, error) {
	from, err := s.checkPath(ctx, in.From, true)
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	to, err := s.checkPath(ctx, in.To, true)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	srcParent, err := s.opt.FS.StatPath(ctx, path.Dir(from))
	if err != nil {
		r, _ := fail(mapErr(err, path.Dir(from)))
		return r, okOutput{}, nil
	}
	dstParent, err := s.opt.FS.StatPath(ctx, path.Dir(to))
	if err != nil {
		r, _ := fail(mapErr(err, path.Dir(to)))
		return r, okOutput{}, nil
	}
	if err := s.opt.FS.Rename(ctx, srcParent.Ino, path.Base(from), dstParent.Ino, path.Base(to)); err != nil {
		r, _ := fail(mapErr(err, from))
		return r, okOutput{}, nil
	}
	return text("moved %s to %s", from, to), okOutput{Path: to, OK: true}, nil
}

func (s *Server) copyFile(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, okOutput, error) {
	from, err := s.checkPath(ctx, in.From, true)
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	to, err := s.checkPath(ctx, in.To, true)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	if _, err := s.opt.FS.Copy(ctx, from, to); err != nil {
		r, _ := fail(mapErr(err, to))
		return r, okOutput{}, nil
	}
	return text("copied %s to %s", from, to), okOutput{Path: to, OK: true}, nil
}

func (s *Server) deletePath(ctx context.Context, _ *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, okOutput, error) {
	p, err := s.checkPath(ctx, in.Path, true)
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	if !in.Confirm {
		r, _ := fail(fmt.Errorf("refusing to delete %s without confirm=true; this also deletes it on the remote", p))
		return r, okOutput{}, nil
	}
	if p == "/" {
		r, _ := fail(errors.New("refusing to delete the mount root"))
		return r, okOutput{}, nil
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
	if err := s.opt.FS.Remove(ctx, parent.Ino, path.Base(p), in.Recursive); err != nil {
		r, _ := fail(mapErr(err, p))
		return r, okOutput{}, nil
	}
	return text("deleted %s", p), okOutput{Path: p, OK: true}, nil
}

func (s *Server) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	root := "/"
	if in.Path != "" {
		var err error
		root, err = s.checkPath(ctx, in.Path, false)
		if err != nil {
			r, _ := fail(err)
			return r, searchOutput{}, nil
		}
	}
	filter, err := searchFilter(in)
	if err != nil {
		r, _ := fail(err)
		return r, searchOutput{}, nil
	}
	if filter.Empty() {
		r, _ := fail(errors.New("query must not be empty"))
		return r, searchOutput{}, nil
	}
	limit := in.MaxResults
	if limit <= 0 || limit > s.opt.Limits.MaxResults {
		limit = s.opt.Limits.MaxResults
	}
	// Intersect scope before the database limit: hidden matches must not crowd
	// authorized matches out of a page.
	roots := s.readRoots(ctx, root)
	candidateLimit := limit + 1
	if in.Content != "" {
		candidateLimit = limit * 4
	}
	answer, err := s.opt.FS.Search(ctx, meta.SearchQuery{Filter: filter, Roots: roots, Limit: candidateLimit, Sort: in.Sort})
	if err != nil {
		r, _ := fail(err)
		return r, searchOutput{}, nil
	}
	results := answer.Results
	out := searchOutput{Coverage: searchCoverage{Listed: answer.Coverage.Listed, Known: answer.Coverage.Known}}
	if !answer.Complete {
		// The index stopped at its work budget, so "no more hits" would be a
		// claim this call cannot make.
		out.Truncated = true
		out.Note = "the index stopped at its work budget; narrow the query or search within a subtree"
	}
	if in.Content != "" && len(results) == candidateLimit {
		out.Truncated = true
		out.Note = "content search reached its metadata candidate budget; narrow the path or query"
	}
	var skippedUncached int
	for _, r := range results {
		if len(out.Hits) >= limit {
			out.Truncated = true
			break
		}
		if root != "/" && r.Path != root && !strings.HasPrefix(r.Path, strings.TrimSuffix(root, "/")+"/") {
			continue
		}
		if !s.visible(ctx, r.Path) {
			continue
		}
		hit := searchHit{Path: r.Path, Name: r.Name, Kind: "file", Size: r.Size, MTime: r.MTime, Cached: r.Cached}
		if r.Kind == provider.KindDir {
			hit.Kind = "dir"
		}
		if in.Content != "" {
			if r.Kind == provider.KindDir {
				continue
			}
			if !r.Cached {
				skippedUncached++
				continue
			}
			data, err := s.opt.FS.ReadFileRange(ctx, r.Path, 0, int64(s.opt.Limits.MaxBytes))
			if err != nil || !strings.Contains(string(data), in.Content) {
				continue
			}
			hit.Line = firstMatchingLine(string(data), in.Content)
		}
		out.Hits = append(out.Hits, hit)
	}
	if skippedUncached > 0 {
		if out.Note != "" {
			out.Note += "; "
		}
		out.Note += fmt.Sprintf("skipped %d files that are not cached locally; call pin on the directory first to search their contents", skippedUncached)
	}
	if out.Coverage.Listed < out.Coverage.Known {
		if out.Note != "" {
			out.Note += "; "
		}
		out.Note += fmt.Sprintf("the index holds %d of %d known directories; entries under the rest are invisible until warm lists them", out.Coverage.Listed, out.Coverage.Known)
	}
	msg := fmt.Sprintf("%d matches for %q", len(out.Hits), searchDescription(in))
	if out.Note != "" {
		msg += ". " + out.Note
	}
	return text("%s", msg), out, nil
}

// searchFilter parses the query and layers the structured parameters over
// it, so an agent can pass either form or both.
func searchFilter(in searchInput) (meta.Filter, error) {
	f, err := meta.ParseQuery(in.Query)
	if err != nil {
		return f, err
	}
	if in.Glob != "" {
		f.Terms = append(f.Terms, meta.Term{Text: in.Glob, Glob: true})
	}
	for _, ext := range strings.Split(in.Ext, ",") {
		if ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), "."); ext != "" {
			f.Ext = append(f.Ext, ext)
		}
	}
	if in.MinSize > 0 {
		f.MinSize = in.MinSize
	}
	if in.MaxSize > 0 {
		f.MaxSize = in.MaxSize
	}
	if in.ModifiedAfter != "" {
		t, err := time.Parse(time.RFC3339, in.ModifiedAfter)
		if err != nil {
			if t, err = time.ParseInLocation("2006-01-02", in.ModifiedAfter, time.Local); err != nil {
				return f, fmt.Errorf("modified_after must be RFC 3339 or YYYY-MM-DD: %q", in.ModifiedAfter)
			}
		}
		f.ModifiedAfter = t
	}
	if in.Kind != "" {
		switch in.Kind {
		case "dir", "file":
			f.Kind = in.Kind
		default:
			return f, fmt.Errorf("kind must be dir or file, not %q", in.Kind)
		}
	}
	return f, nil
}

func searchDescription(in searchInput) string {
	parts := []string{}
	if q := strings.TrimSpace(in.Query); q != "" {
		parts = append(parts, q)
	}
	if in.Glob != "" {
		parts = append(parts, in.Glob)
	}
	if in.Ext != "" {
		parts = append(parts, "ext:"+in.Ext)
	}
	if in.Kind != "" {
		parts = append(parts, "type:"+in.Kind)
	}
	if in.MinSize > 0 || in.MaxSize > 0 {
		parts = append(parts, fmt.Sprintf("size:%d..%d", in.MinSize, in.MaxSize))
	}
	if in.ModifiedAfter != "" {
		parts = append(parts, "modified_after:"+in.ModifiedAfter)
	}
	return strings.Join(parts, " ")
}

func firstMatchingLine(content, needle string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, needle) {
			if len(line) > 200 {
				return line[:197] + "..."
			}
			return line
		}
	}
	return ""
}

func (s *Server) pin(ctx context.Context, _ *mcp.CallToolRequest, in pinInput) (*mcp.CallToolResult, okOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	if err := s.opt.FS.Pin(ctx, p); err != nil {
		r, _ := fail(mapErr(err, p))
		return r, okOutput{}, nil
	}
	return text("cached %s locally and pinned it", p), okOutput{Path: p, OK: true}, nil
}

func (s *Server) unpin(ctx context.Context, _ *mcp.CallToolRequest, in pinInput) (*mcp.CallToolResult, okOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err == nil {
		err = s.opt.FS.Unpin(ctx, p)
	}
	if err != nil {
		r, _ := fail(err)
		return r, okOutput{}, nil
	}
	return text("removed pin rule %s; overlapping rules still apply", p), okOutput{Path: p, OK: true}, nil
}

func (s *Server) cacheStatus(ctx context.Context, _ *mcp.CallToolRequest, in statInput) (*mcp.CallToolResult, cacheStatusOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, cacheStatusOutput{}, nil
	}
	a, err := s.opt.FS.StatPath(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, cacheStatusOutput{}, nil
	}
	cs := s.opt.FS.Cache().Stats()
	out := cacheStatusOutput{
		Path: p, Cached: a.Cached, Size: a.Size,
		HitRatio: cs.HitRatio(), CacheBytes: cs.Bytes,
	}
	if j := s.opt.FS.Journal(); j != nil {
		if js, err := j.Stats(ctx); err == nil {
			out.PendingUploads = js.Pending + js.Uploading
		}
	}
	return text("%s: %.0f%% cached, %d uploads queued", p, a.Cached*100, out.PendingUploads), out, nil
}

func (s *Server) listRoots(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, rootsOutput, error) {
	var out rootsOutput
	for _, m := range s.opt.FS.Mounts() {
		if !s.visible(ctx, m.Prefix) {
			continue
		}
		out.Roots = append(out.Roots, rootInfo{
			Path: m.Prefix, Remote: m.Remote, Mode: string(m.Mode),
			ReadOnly: s.scopeOf(ctx).ReadOnly || string(m.Mode) == "readonly",
		})
	}
	sort.Slice(out.Roots, func(i, j int) bool { return out.Roots[i].Path < out.Roots[j].Path })
	return text("%d roots", len(out.Roots)), out, nil
}

func (s *Server) getDownloadURL(ctx context.Context, _ *mcp.CallToolRequest, in statInput) (*mcp.CallToolResult, downloadURLOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, downloadURLOutput{}, nil
	}
	link, err := s.opt.FS.DownloadURL(ctx, p)
	if err != nil {
		r, _ := fail(mapErr(err, p))
		return r, downloadURLOutput{}, nil
	}
	out := downloadURLOutput{Path: p, URL: link.URL, Headers: link.Headers}
	if !link.ExpiresAt.IsZero() {
		out.ExpiresAt = link.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return text("direct URL for %s (expires %s)", p, out.ExpiresAt), out, nil
}

// mapErr turns VFS errors into messages an agent can act on.
func mapErr(err error, p string) error {
	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return fmt.Errorf("%s does not exist", p)
	case errors.Is(err, vfs.ErrExists):
		return fmt.Errorf("%s already exists", p)
	case errors.Is(err, vfs.ErrIsDir):
		return fmt.Errorf("%s is a directory", p)
	case errors.Is(err, vfs.ErrNotDir):
		return fmt.Errorf("%s is not a directory", p)
	case errors.Is(err, vfs.ErrNotEmpty):
		return fmt.Errorf("%s is not empty; pass recursive=true to delete it and its contents", p)
	case errors.Is(err, vfs.ErrReadOnly):
		return fmt.Errorf("%s is on a read-only mount", p)
	case errors.Is(err, vfs.ErrNotOwner):
		// The VFS's own fence; requireOwner normally answers first.
		return errNonOwnerWrite
	case errors.Is(err, vfs.ErrUploadCancelled):
		return errors.New("retained cancelled upload requires reconciliation before this operation; local content has not been discarded")
	case errors.Is(err, vfs.ErrUploadPurging):
		return errors.New("local upload cleanup is pending; do not retry the upload or infer remote completion")
	case errors.Is(err, vfs.ErrNoSpace), errors.Is(err, syscall.ENOSPC):
		return errors.New("local storage has insufficient space; free disk space or review cache.max_size and cache.min_free")
	case errors.Is(err, vfs.ErrCrossMount):
		return errors.New("cannot move between different remotes; copy the file instead")
	default:
		return err
	}
}
