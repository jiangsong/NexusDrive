package mcpsrv

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type copyToolState struct{ cursor cipher.AEAD }

func (s *copyToolState) init() error {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	s.cursor, err = cipher.NewGCMWithRandomNonce(block)
	return err
}

func validCopyID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

func (s *Server) encodeCopyCursor(id string) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(s.copyTools.cursor.Seal(nil, nil, []byte(id), []byte("cloudfs/copy-jobs/v1")))
}

func (s *Server) decodeCopyCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 128 {
		return "", errors.New("invalid copy cursor; restart listing")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err == nil {
		var data []byte
		data, err = s.copyTools.cursor.Open(nil, nil, raw, []byte("cloudfs/copy-jobs/v1"))
		if err == nil && validCopyID(string(data)) {
			return string(data), nil
		}
	}
	return "", errors.New("invalid copy cursor; restart listing")
}

type copyJobsInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque next_cursor from this server; invalid after server restart"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum visible jobs; server response and scan limits still apply"`
}

type copyJobInput struct {
	ID string `json:"id" jsonschema:"Copy preparation ID returned by list_copy_jobs; submitted means handed to uploads, not remote completion"`
}

type forgetCopyJobInput struct {
	ID      string `json:"id" jsonschema:"Copy preparation ID; only unreferenced terminal preparation data can be removed"`
	Confirm bool   `json:"confirm" jsonschema:"Must be true to permanently remove unreferenced preparation content and history; never deletes files"`
}

type copyJobsOutput struct {
	Jobs       []vfs.CopyInfo `json:"jobs"`
	Truncated  bool           `json:"truncated"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type copyMutationOutput struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Accepted bool   `json:"accepted"`
}

func (s *Server) registerCopyTools() {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mutating := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list_copy_jobs", Description: "List copy preparation jobs visible at BOTH source and target paths. Bounded local scan; follow next_cursor even on an empty page. submitted is upload handoff, not remote success.", Annotations: ro}, s.listCopyJobs)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "get_copy_job", Description: "Inspect a visible copy preparation job without downloads or recovery. Retained checkpoints and state do not prove remote completion.", Annotations: ro}, s.getCopyJob)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "retry_copy_job", Description: "Queue a failed/cancelled preparation for retry after validation. Does not wait for completion. Inspect state after any uncertain error; do not automatically replay.", Annotations: mutating}, s.retryCopyJob)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "cancel_copy_job", Description: "Durably stop copy preparation, retaining downloaded bytes and any readable local version. Cannot cancel an already submitted upload.", Annotations: mutating}, s.cancelCopyJob)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "forget_copy_job", Description: "With confirm=true, remove terminal preparation history and unreferenced retained bytes. Rejects active/referenced data; never deletes a local/remote file. Failed cleanup may require inspection and retry.", Annotations: mutating}, s.forgetCopyJob)
}

func (s *Server) copyInfoAllowed(ctx context.Context, info vfs.CopyInfo) bool {
	for _, p := range []string{info.Source, info.Target} {
		clean, err := s.checkPath(ctx, p, false)
		if err != nil || clean != p || !utf8.ValidString(p) {
			return false
		}
	}
	return true
}

func (s *Server) authorizedCopy(ctx context.Context, id string) (vfs.CopyInfo, error) {
	if !validCopyID(id) {
		return vfs.CopyInfo{}, errors.New("invalid copy ID")
	}
	info, err := s.opt.FS.InspectCopy(ctx, id)
	if err != nil {
		return vfs.CopyInfo{}, err
	}
	if !s.copyInfoAllowed(ctx, info) {
		return vfs.CopyInfo{}, vfs.ErrNotFound
	}
	return info, nil
}

// Never expose raw journal/provider errors: they may contain retained paths,
// backend responses or another account's object names.
func copyManagementError(err error) error {
	switch {
	case errors.Is(err, vfs.ErrNotFound), errors.Is(err, journal.ErrNotFound):
		return errors.New("copy job not found or inaccessible")
	case errors.Is(err, journal.ErrCopyState), errors.Is(err, journal.ErrCopyBusy):
		return errors.New("copy preparation state does not permit this action; inspect before retrying (submitted uploads cannot be cancelled here)")
	case errors.Is(err, journal.ErrCopyReferenced), errors.Is(err, meta.ErrCopyReferenced):
		return errors.New("copy content is still referenced; no file was deleted")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.New("copy request interrupted; inspect current state before retrying")
	case errors.Is(err, vfs.ErrReadOnly):
		return errors.New("copy destination is read-only")
	default:
		return errors.New("copy management failed; inspect current state before retrying")
	}
}

func (s *Server) listCopyJobs(ctx context.Context, _ *mcp.CallToolRequest, in copyJobsInput) (*mcp.CallToolResult, copyJobsOutput, error) {
	out := copyJobsOutput{Jobs: []vfs.CopyInfo{}}
	after, err := s.decodeCopyCursor(in.Cursor)
	if err != nil || in.Limit < 0 {
		r, _ := fail(errors.New("invalid copy list cursor or limit; restart listing"))
		return r, out, nil
	}
	limit := in.Limit
	if limit == 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	// Do not scan an unbounded collection to fill a sparse allowlist page.
	jobs, next, err := s.opt.FS.InspectCopyPage(ctx, after, 200)
	if err != nil {
		r, _ := fail(copyManagementError(err))
		return r, out, nil
	}
	last := after
	for i, info := range jobs {
		if !s.copyInfoAllowed(ctx, info) {
			continue
		}
		candidate := copyJobsOutput{Jobs: append(out.Jobs, info)}
		if i+1 < len(jobs) || next != "" {
			candidate.NextCursor = s.encodeCopyCursor(info.ID)
			candidate.Truncated = true
		}
		data, _ := json.Marshal(candidate)
		if len(data) > s.opt.Limits.MaxBytes {
			if len(out.Jobs) == 0 {
				r, _ := fail(errors.New("copy job exceeds the configured response byte limit"))
				return r, copyJobsOutput{}, nil
			}
			out.NextCursor, out.Truncated = s.encodeCopyCursor(last), true
			return text("%d copy preparation jobs; continue with next_cursor", len(out.Jobs)), out, nil
		}
		out, last = candidate, info.ID
		if len(out.Jobs) == limit {
			return text("%d copy preparation jobs", len(out.Jobs)), out, nil
		}
	}
	out.NextCursor, out.Truncated = s.encodeCopyCursor(next), next != ""
	data, _ := json.Marshal(out)
	if len(data) > s.opt.Limits.MaxBytes {
		r, _ := fail(errors.New("copy page exceeds the configured response byte limit"))
		return r, copyJobsOutput{}, nil
	}
	return text("%d copy preparation jobs", len(out.Jobs)), out, nil
}

func (s *Server) getCopyJob(ctx context.Context, _ *mcp.CallToolRequest, in copyJobInput) (*mcp.CallToolResult, vfs.CopyInfo, error) {
	info, err := s.authorizedCopy(ctx, in.ID)
	if err != nil {
		r, _ := fail(copyManagementError(err))
		return r, vfs.CopyInfo{}, nil
	}
	data, _ := json.Marshal(info)
	if len(data) > s.opt.Limits.MaxBytes {
		r, _ := fail(errors.New("copy job exceeds the configured response byte limit"))
		return r, vfs.CopyInfo{}, nil
	}
	return text("copy preparation %s: %s (submitted is upload handoff, not remote completion)", info.ID, info.State), info, nil
}

func (s *Server) mutateCopyJob(ctx context.Context, id, action string, confirm bool) (*mcp.CallToolResult, copyMutationOutput, error) {
	if err := s.checkWrite(ctx); err != nil {
		r, _ := fail(err)
		return r, copyMutationOutput{}, nil
	}
	if action == "forget" && !confirm {
		r, _ := fail(errors.New("forget_copy_job requires confirm=true"))
		return r, copyMutationOutput{}, nil
	}
	if _, err := s.authorizedCopy(ctx, id); err != nil {
		r, _ := fail(copyManagementError(err))
		return r, copyMutationOutput{}, nil
	}
	var err error
	switch action {
	case "retry":
		err = s.opt.FS.RetryCopy(ctx, id)
	case "cancel":
		err = s.opt.FS.CancelCopy(ctx, id)
	case "forget":
		err = s.opt.FS.ForgetCopy(ctx, id)
	default:
		err = errors.New("unknown copy management operation")
	}
	if err != nil {
		r, _ := fail(copyManagementError(err))
		return r, copyMutationOutput{}, nil
	}
	return text("copy %s accepted; inspect state before any retry; no remote completion is implied", action), copyMutationOutput{ID: id, Action: action, Accepted: true}, nil
}

func (s *Server) retryCopyJob(ctx context.Context, _ *mcp.CallToolRequest, in copyJobInput) (*mcp.CallToolResult, copyMutationOutput, error) {
	return s.mutateCopyJob(ctx, in.ID, "retry", false)
}

func (s *Server) cancelCopyJob(ctx context.Context, _ *mcp.CallToolRequest, in copyJobInput) (*mcp.CallToolResult, copyMutationOutput, error) {
	return s.mutateCopyJob(ctx, in.ID, "cancel", false)
}

func (s *Server) forgetCopyJob(ctx context.Context, _ *mcp.CallToolRequest, in forgetCopyJobInput) (*mcp.CallToolResult, copyMutationOutput, error) {
	return s.mutateCopyJob(ctx, in.ID, "forget", in.Confirm)
}
