package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/export"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The export tools let an agent copy a subtree of the mount onto this
// machine's own storage. That is the one thing on this server that writes
// outside the mount, so it carries a second boundary of its own: the
// destination must be inside one of the configured mcp.export_roots. The
// path allowlist answers "what may this agent read"; export_roots answers
// "where may it put it", and neither implies the other. An unconfigured
// export_roots means no destination is permitted at all — a server that
// defaulted to the home directory would be a file writer nobody asked for.

// ExportJobs is the part of *export.Manager these tools use.
type ExportJobs interface {
	Create(ctx context.Context, req export.Request) (export.Job, error)
	Job(ctx context.Context, id string) (export.Job, error)
	Jobs(ctx context.Context, limit int, after string) ([]export.Job, string, error)
	Cancel(ctx context.Context, id string) error
	Progress(ctx context.Context, id string) (export.Progress, error)
}

// exportCursorAAD separates export cursors from the copy and upload cursors
// that share the same key.
const exportCursorAAD = "cloudfs/export-jobs/v1"

func (s *Server) encodeExportCursor(id string) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(s.copyTools.cursor.Seal(nil, nil, []byte(id), []byte(exportCursorAAD)))
}

func (s *Server) decodeExportCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 128 {
		return "", errors.New("invalid export cursor; restart listing")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err == nil {
		var data []byte
		data, err = s.copyTools.cursor.Open(nil, nil, raw, []byte(exportCursorAAD))
		if err == nil && validCopyID(string(data)) {
			return string(data), nil
		}
	}
	return "", errors.New("invalid export cursor; restart listing")
}

type exportInput struct {
	Paths  []string `json:"paths" jsonschema:"Absolute virtual paths inside the mount to copy out; a directory is copied whole"`
	Dest   string   `json:"dest" jsonschema:"Local destination directory; must be inside a configured export root or the request is refused"`
	Mirror bool     `json:"mirror,omitempty" jsonschema:"Delete destination entries this export does not produce; refused unless an earlier export of the same sources marked the directory"`
	Verify bool     `json:"verify,omitempty" jsonschema:"Re-read each finished file from the destination disk to catch write errors"`
}

type exportJobsInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque next_cursor from this server; invalid after server restart"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum jobs to return; server response limits still apply"`
}

type exportJobInput struct {
	ID string `json:"id" jsonschema:"Export job ID returned by export or list_export_jobs"`
}

// exportJobView is what an agent sees of a job. The plan's remote ids,
// versions and mount bindings are never part of it.
type exportJobView struct {
	ID           string   `json:"id"`
	State        string   `json:"state"`
	PauseReason  string   `json:"pause_reason,omitempty"`
	Sources      []string `json:"sources"`
	Dest         string   `json:"dest"`
	Mirror       bool     `json:"mirror"`
	Verify       bool     `json:"verify"`
	FilesTotal   int64    `json:"files_total"`
	FilesDone    int64    `json:"files_done"`
	FilesSkipped int64    `json:"files_skipped"`
	FilesFailed  int64    `json:"files_failed"`
	BytesTotal   int64    `json:"bytes_total"`
	BytesDone    int64    `json:"bytes_done"`
	// Rate and ETASeconds are live, so they are zero on a job that is not
	// moving bytes right now.
	Rate       float64 `json:"rate,omitempty"`
	ETASeconds float64 `json:"eta_seconds,omitempty"`
	LastError  string  `json:"last_error,omitempty"`
}

type exportJobsOutput struct {
	Jobs       []exportJobView `json:"jobs"`
	Truncated  bool            `json:"truncated"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type exportMutationOutput struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Accepted bool   `json:"accepted"`
}

func toExportJobView(j export.Job) exportJobView {
	v := exportJobView{
		ID: j.ID, State: string(j.State), PauseReason: string(j.PauseReason),
		Sources: j.Sources, Dest: j.Dest, Mirror: j.Options.Mirror, Verify: j.Options.Verify,
		FilesTotal: j.FilesTotal, FilesDone: j.FilesDone, FilesSkipped: j.FilesSkipped,
		FilesFailed: j.FilesFailed, BytesTotal: j.BytesTotal, BytesDone: j.BytesDone,
		LastError: j.LastError,
	}
	if v.Sources == nil {
		v.Sources = []string{}
	}
	return v
}

func (s *Server) registerExportTools() {
	if s.opt.Export == nil {
		return
	}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mutating := &mcp.ToolAnnotations{DestructiveHint: ptr(false)}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "export", Description: "Copy virtual paths out of the mount into a local directory. The destination must be inside a configured export root. Returns a job ID at once; the copy runs in the daemon and resumes after a restart. mirror=true also deletes destination entries this export does not produce and is refused without an existing export marker.", Annotations: mutating}, s.startExport)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list_export_jobs", Description: "List export jobs newest first. Follow next_cursor for more. Bytes done are checkpointed, not a promise that a file is complete.", Annotations: ro}, s.listExportJobs)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "get_export_job", Description: "Inspect one export job with its live rate and estimate. A paused job names the reason: user, disk, unavailable, risk_control or auth.", Annotations: ro}, s.getExportJob)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "cancel_export_job", Description: "Stop an export job for good. Finished files and partial files are both kept on the destination; nothing is deleted.", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)}}, s.cancelExportJob)
}

// exportDestination resolves and authorizes a destination directory.
//
// Both sides are compared as the operating system resolves them, so a root
// reached through a symlink (/var/folders on macOS, /home on many Linux
// installs) matches a destination named the other way round — and a symlink
// *inside* a root that points outside it does not, because the resolved
// destination then lands outside the resolved root.
func exportDestination(roots []string, dest string) (string, error) {
	if strings.TrimSpace(dest) == "" {
		return "", errors.New("export needs a destination directory")
	}
	if len(roots) == 0 {
		return "", errors.New("this cloudfs MCP server has no mcp.export_roots configured, so it cannot write anywhere on this machine")
	}
	abs, err := filepath.Abs(config.ExpandHome(dest))
	if err != nil {
		return "", errors.New("the destination is not a usable local path")
	}
	abs = filepath.Clean(abs)
	resolved := resolveExisting(abs)
	for _, root := range roots {
		r, err := filepath.Abs(config.ExpandHome(root))
		if err != nil {
			continue
		}
		r = filepath.Clean(r)
		// Authorization must be based on the paths the OS will actually
		// use. A lexical match is not sufficient: root/link/out may spell a
		// path below root while link sends the write somewhere else.
		if under(resolveExisting(r), resolved) {
			return abs, nil
		}
	}
	return "", errors.New("the destination is outside every configured export root")
}

// resolveExisting follows symlinks as far as the path exists and keeps the
// part that does not exist yet. filepath.EvalSymlinks fails outright on a
// path whose last components are still to be created, which is exactly the
// case of exporting into a new subdirectory of a root.
func resolveExisting(p string) string {
	cur, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// under reports whether p is root or lies beneath it.
func under(root, p string) bool {
	if root == "" {
		return false
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, string(os.PathSeparator))+string(os.PathSeparator))
}

// exportManagementError keeps the manager's own text — which may name a
// destination path or a backend response — out of the tool result.
func exportManagementError(err error) error {
	switch {
	case errors.Is(err, export.ErrNotFound):
		return errors.New("export job not found")
	case errors.Is(err, export.ErrMirrorMarkerMissing):
		return errors.New("a mirror export needs a destination an earlier export of the same sources already owns; run it without mirror first")
	case errors.Is(err, export.ErrInvalid):
		return errors.New("the export request was rejected; check that every source is an absolute path inside the mount")
	case errors.Is(err, export.ErrNotOwner):
		return errors.New("another process owns the export queue on this machine")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.New("export request interrupted; inspect current state before retrying")
	default:
		return errors.New("export management failed; inspect current state before retrying")
	}
}

func (s *Server) startExport(ctx context.Context, _ *mcp.CallToolRequest, in exportInput) (*mcp.CallToolResult, exportJobView, error) {
	err := s.checkWrite(ctx)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, exportJobView{}, nil
	}
	if len(in.Paths) == 0 {
		r, _ := fail(errors.New("export needs at least one source path"))
		return r, exportJobView{}, nil
	}
	sources := make([]string, 0, len(in.Paths))
	for _, p := range in.Paths {
		clean, err := s.checkPath(ctx, p, false)
		if err != nil {
			r, _ := fail(err)
			return r, exportJobView{}, nil
		}
		sources = append(sources, clean)
	}
	dest, err := exportDestination(s.opt.ExportRoots, in.Dest)
	if err != nil {
		r, _ := fail(err)
		return r, exportJobView{}, nil
	}
	job, err := s.opt.Export.Create(ctx, export.Request{
		Sources: sources, Dest: dest, Mirror: in.Mirror, Verify: in.Verify,
	})
	if err != nil {
		r, _ := fail(exportManagementError(err))
		return r, exportJobView{}, nil
	}
	return text("export %s queued to %s; poll get_export_job for progress", job.ID, dest), toExportJobView(job), nil
}

func (s *Server) listExportJobs(ctx context.Context, _ *mcp.CallToolRequest, in exportJobsInput) (*mcp.CallToolResult, exportJobsOutput, error) {
	out := exportJobsOutput{Jobs: []exportJobView{}}
	after, err := s.decodeExportCursor(in.Cursor)
	if err != nil || in.Limit < 0 {
		r, _ := fail(errors.New("invalid export list cursor or limit; restart listing"))
		return r, out, nil
	}
	limit := in.Limit
	if limit == 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	jobs, next, err := s.opt.Export.Jobs(ctx, limit, after)
	if err != nil {
		r, _ := fail(exportManagementError(err))
		return r, out, nil
	}
	for _, j := range jobs {
		candidate := exportJobsOutput{Jobs: append(out.Jobs, toExportJobView(j))}
		data, _ := json.Marshal(candidate)
		if len(data) > s.opt.Limits.MaxBytes {
			if len(out.Jobs) == 0 {
				r, _ := fail(errors.New("export job exceeds the configured response byte limit"))
				return r, exportJobsOutput{}, nil
			}
			out.NextCursor, out.Truncated = s.encodeExportCursor(out.Jobs[len(out.Jobs)-1].ID), true
			return text("%d export jobs; continue with next_cursor", len(out.Jobs)), out, nil
		}
		out = candidate
	}
	out.NextCursor, out.Truncated = s.encodeExportCursor(next), next != ""
	return text("%d export jobs", len(out.Jobs)), out, nil
}

func (s *Server) getExportJob(ctx context.Context, _ *mcp.CallToolRequest, in exportJobInput) (*mcp.CallToolResult, exportJobView, error) {
	if !validCopyID(in.ID) {
		r, _ := fail(errors.New("invalid export job ID"))
		return r, exportJobView{}, nil
	}
	job, err := s.opt.Export.Job(ctx, in.ID)
	if err != nil {
		r, _ := fail(exportManagementError(err))
		return r, exportJobView{}, nil
	}
	v := toExportJobView(job)
	if p, err := s.opt.Export.Progress(ctx, in.ID); err == nil {
		v.Rate, v.ETASeconds = p.Rate, p.ETA.Seconds()
	}
	detail := v.State
	if v.PauseReason != "" {
		detail = fmt.Sprintf("%s (%s)", v.State, v.PauseReason)
	}
	return text("export %s: %s, %d/%d bytes, %d/%d files", v.ID, detail, v.BytesDone, v.BytesTotal, v.FilesDone+v.FilesSkipped, v.FilesTotal), v, nil
}

func (s *Server) cancelExportJob(ctx context.Context, _ *mcp.CallToolRequest, in exportJobInput) (*mcp.CallToolResult, exportMutationOutput, error) {
	err := s.checkWrite(ctx)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, exportMutationOutput{}, nil
	}
	if !validCopyID(in.ID) {
		r, _ := fail(errors.New("invalid export job ID"))
		return r, exportMutationOutput{}, nil
	}
	if err := s.opt.Export.Cancel(ctx, in.ID); err != nil {
		r, _ := fail(exportManagementError(err))
		return r, exportMutationOutput{}, nil
	}
	return text("export %s cancelled; nothing already written to the destination was deleted", in.ID), exportMutationOutput{ID: in.ID, Action: "cancel", Accepted: true}, nil
}
