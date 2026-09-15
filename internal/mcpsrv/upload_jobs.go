package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloudfs/internal/journal"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const uploadCursorAAD = "cloudfs/uploads/v1"

var errUnrestrictedUploadManagement = errors.New("upload operation requires an unrestricted MCP server")

type uploadsInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque next_cursor from this server; invalid after server restart"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum visible uploads; server response and scan limits still apply"`
}

type uploadInput struct {
	ID string `json:"id" jsonschema:"Upload ID returned by list_uploads"`
}

type confirmedUploadInput struct {
	ID      string `json:"id" jsonschema:"Upload ID returned by list_uploads"`
	Confirm bool   `json:"confirm" jsonschema:"Must be true to acknowledge the operation-specific permanent data-loss or remote replay risk"`
}

type uploadsOutput struct {
	Uploads    []vfs.UploadInfo `json:"uploads"`
	Truncated  bool             `json:"truncated"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type uploadMutationOutput struct {
	ID       string        `json:"id"`
	Action   string        `json:"action"`
	Accepted bool          `json:"accepted"`
	State    journal.State `json:"state,omitempty"`
	Warning  string        `json:"warning,omitempty"`
}

type uploadFlushOutput struct {
	Completed bool        `json:"completed"`
	Stats     uploadStats `json:"stats"`
	Warning   string      `json:"warning,omitempty"`
}

type uploadStats struct {
	Pending       int   `json:"pending"`
	Uploading     int   `json:"uploading"`
	Dead          int   `json:"dead"`
	Done          int   `json:"done"`
	Cancelling    int   `json:"cancelling"`
	Cancelled     int   `json:"cancelled"`
	Purging       int   `json:"purging"`
	RetainedBytes int64 `json:"retained_bytes"`
	QueuedBytes   int64 `json:"queued_bytes"`
}

func validUploadID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.ContainsAny(id, "\x00\r\n")
}

func (s *Server) encodeUploadCursor(id string) string {
	if id == "" {
		return ""
	}
	raw := s.copyTools.cursor.Seal(nil, nil, []byte(id), []byte(uploadCursorAAD))
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Server) decodeUploadCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 512 {
		return "", errors.New("invalid upload cursor; restart listing")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err == nil {
		var data []byte
		data, err = s.copyTools.cursor.Open(nil, nil, raw, []byte(uploadCursorAAD))
		if err == nil && validUploadID(string(data)) {
			return string(data), nil
		}
	}
	return "", errors.New("invalid upload cursor; restart listing")
}

func (s *Server) registerUploadTools() {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mutating := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list_uploads", Description: "List visible active uploads without provider sessions, local blob paths, hashes, remote versions or raw backend errors. Follow next_cursor even on an empty filtered page.", Annotations: ro}, s.listUploads)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "get_upload", Description: "Inspect one visible upload. A missing path means an unrestricted server can see the durable row but it is no longer bound to a current namespace entry.", Annotations: ro}, s.getUpload)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "retry_upload", Description: "Requeue one dead upload only while it remains bound to the authorized current local version. Inspect after uncertain errors; never automatically replay.", Annotations: mutating}, s.retryUpload)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "cancel_upload", Description: "Durably stop one visible upload. Cancellation retains local content and cannot undo or prove remote effects.", Annotations: mutating}, s.cancelUpload)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "resume_upload", Description: "With confirm=true, start a fresh attempt for a cancelled current local version, accepting possible duplicate or overwritten remote data.", Annotations: mutating}, s.resumeUpload)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "discard_upload", Description: "Unrestricted servers only. With confirm=true, permanently remove a stopped current local version and private upload records; never undoes remote effects.", Annotations: mutating}, s.discardUpload)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "flush_uploads", Description: "Unrestricted servers only. Wait for the current upload queue snapshot, including scheduled retries; fails on retained dead, cancelled or cleanup records.", Annotations: mutating}, s.flushUploads)
}

func (s *Server) uploadInfoAllowed(ctx context.Context, info vfs.UploadInfo) bool {
	if s.unrestricted(ctx) {
		return true
	}
	if info.Path == "" {
		return false
	}
	clean, err := s.checkPath(ctx, info.Path, false)
	return err == nil && clean == info.Path
}

func (s *Server) authorizedUpload(ctx context.Context, id string) (vfs.UploadInfo, error) {
	if !validUploadID(id) {
		return vfs.UploadInfo{}, errors.New("invalid upload ID")
	}
	info, err := s.opt.FS.InspectUpload(ctx, id)
	if err != nil {
		return vfs.UploadInfo{}, err
	}
	if !s.uploadInfoAllowed(ctx, info) {
		return vfs.UploadInfo{}, journal.ErrNotFound
	}
	return info, nil
}

// Never expose raw journal, SQLite, cache or provider errors: upload records
// contain private paths, signed sessions and backend object names.
func uploadManagementError(err error) error {
	switch {
	case errors.Is(err, journal.ErrNotFound), errors.Is(err, vfs.ErrNotFound):
		return errors.New("upload not found or inaccessible")
	case errors.Is(err, vfs.ErrUploadManagementTarget), errors.Is(err, vfs.ErrUploadResumeTarget),
		errors.Is(err, vfs.ErrUploadCleanupTarget), errors.Is(err, vfs.ErrUploadCleanupBusy):
		return errors.New("upload is no longer bound to an eligible current local version; inspect state and path")
	case errors.Is(err, journal.ErrInFlight), errors.Is(err, journal.ErrNotDead),
		errors.Is(err, journal.ErrCannotCancel), errors.Is(err, journal.ErrCancelled),
		errors.Is(err, journal.ErrUploadPurging), errors.Is(err, journal.ErrUploadCleanupState),
		errors.Is(err, journal.ErrResumeChanged), errors.Is(err, journal.ErrResumeContent),
		errors.Is(err, upload.ErrDeadLetters), errors.Is(err, upload.ErrCancelledUploads),
		errors.Is(err, upload.ErrCleanupPending):
		return errors.New("upload state does not permit this action; inspect the upload before retrying")
	case errors.Is(err, vfs.ErrReadOnly):
		return errors.New("upload target is read-only")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.New("upload request interrupted; inspect current state before retrying")
	default:
		return errors.New("upload management failed; inspect current state before retrying")
	}
}

func (s *Server) listUploads(ctx context.Context, _ *mcp.CallToolRequest, in uploadsInput) (*mcp.CallToolResult, uploadsOutput, error) {
	out := uploadsOutput{Uploads: []vfs.UploadInfo{}}
	after, err := s.decodeUploadCursor(in.Cursor)
	if err != nil || in.Limit < 0 {
		r, _ := fail(errors.New("invalid upload list cursor or limit; restart listing"))
		return r, out, nil
	}
	limit := in.Limit
	if limit == 0 || limit > s.opt.Limits.MaxEntries {
		limit = s.opt.Limits.MaxEntries
	}
	rows, next, err := s.opt.FS.InspectUploadPage(ctx, after, 200)
	if err != nil {
		r, _ := fail(uploadManagementError(err))
		return r, out, nil
	}
	last := after
	for i, info := range rows {
		if !s.uploadInfoAllowed(ctx, info) {
			continue
		}
		candidate := uploadsOutput{Uploads: append(out.Uploads, info)}
		if i+1 < len(rows) || next != "" {
			candidate.NextCursor = s.encodeUploadCursor(info.ID)
			candidate.Truncated = true
		}
		data, _ := json.Marshal(candidate)
		if len(data) > s.opt.Limits.MaxBytes {
			if len(out.Uploads) == 0 {
				r, _ := fail(errors.New("upload record exceeds the configured response byte limit"))
				return r, uploadsOutput{}, nil
			}
			out.NextCursor, out.Truncated = s.encodeUploadCursor(last), true
			return text("%d uploads; continue with next_cursor", len(out.Uploads)), out, nil
		}
		out, last = candidate, info.ID
		if len(out.Uploads) == limit {
			return text("%d uploads", len(out.Uploads)), out, nil
		}
	}
	out.NextCursor, out.Truncated = s.encodeUploadCursor(next), next != ""
	return text("%d uploads", len(out.Uploads)), out, nil
}

func (s *Server) getUpload(ctx context.Context, _ *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, vfs.UploadInfo, error) {
	info, err := s.authorizedUpload(ctx, in.ID)
	if err != nil {
		r, _ := fail(uploadManagementError(err))
		return r, vfs.UploadInfo{}, nil
	}
	data, _ := json.Marshal(info)
	if len(data) > s.opt.Limits.MaxBytes {
		r, _ := fail(errors.New("upload record exceeds the configured response byte limit"))
		return r, vfs.UploadInfo{}, nil
	}
	return text("upload %s: %s; no remote completion is implied", info.ID, info.State), info, nil
}

func (s *Server) mutateUpload(ctx context.Context, id, action string, confirm bool) (*mcp.CallToolResult, uploadMutationOutput, error) {
	err := s.checkWrite(ctx)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, uploadMutationOutput{}, nil
	}
	if (action == "resume" || action == "discard") && !confirm {
		r, _ := fail(errors.New(action + "_upload requires confirm=true"))
		return r, uploadMutationOutput{}, nil
	}
	info, err := s.authorizedUpload(ctx, id)
	if err != nil {
		r, _ := fail(uploadManagementError(err))
		return r, uploadMutationOutput{}, nil
	}
	out := uploadMutationOutput{ID: id, Action: action, Accepted: true}
	switch action {
	case "retry":
		err = s.opt.FS.RetryUploadAt(ctx, id, info.Path)
		out.State = journal.StatePending
	case "cancel":
		if s.unrestricted(ctx) {
			out.State, err = s.opt.FS.CancelUpload(ctx, id)
		} else {
			out.State, err = s.opt.FS.CancelUploadAt(ctx, id, info.Path)
		}
		out.Warning = "local content retained; cancellation does not undo or reconcile remote changes"
	case "resume":
		if s.unrestricted(ctx) {
			err = s.opt.FS.ResumeUpload(ctx, id, true)
		} else {
			err = s.opt.FS.ResumeUploadAt(ctx, id, info.Path, true)
		}
		out.State = journal.StatePending
		out.Warning = "a fresh upload attempt was accepted; remote data may be duplicated or overwritten"
	case "discard":
		if !s.unrestricted(ctx) {
			err = fmt.Errorf("%w because a cleanup retry may no longer have a namespace path", errUnrestrictedUploadManagement)
		} else {
			err = s.opt.FS.DiscardUpload(ctx, id, true)
		}
		out.Warning = "local version and private upload records discarded; remote changes were not undone or reconciled"
	default:
		err = errors.New("unknown upload management operation")
	}
	if err != nil {
		if errors.Is(err, errUnrestrictedUploadManagement) {
			r, _ := fail(err)
			return r, uploadMutationOutput{}, nil
		}
		r, _ := fail(uploadManagementError(err))
		return r, uploadMutationOutput{}, nil
	}
	return text("upload %s accepted; inspect state before any retry; no remote completion is implied", action), out, nil
}

func (s *Server) retryUpload(ctx context.Context, _ *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, uploadMutationOutput, error) {
	return s.mutateUpload(ctx, in.ID, "retry", false)
}

func (s *Server) cancelUpload(ctx context.Context, _ *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, uploadMutationOutput, error) {
	return s.mutateUpload(ctx, in.ID, "cancel", false)
}

func (s *Server) resumeUpload(ctx context.Context, _ *mcp.CallToolRequest, in confirmedUploadInput) (*mcp.CallToolResult, uploadMutationOutput, error) {
	return s.mutateUpload(ctx, in.ID, "resume", in.Confirm)
}

func (s *Server) discardUpload(ctx context.Context, _ *mcp.CallToolRequest, in confirmedUploadInput) (*mcp.CallToolResult, uploadMutationOutput, error) {
	return s.mutateUpload(ctx, in.ID, "discard", in.Confirm)
}

func (s *Server) flushUploads(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, uploadFlushOutput, error) {
	out := uploadFlushOutput{}
	err := s.checkWrite(ctx)
	if err == nil {
		err = s.requireOwner(ctx)
	}
	if err != nil {
		r, _ := fail(err)
		return r, out, nil
	}
	if !s.unrestricted(ctx) {
		r, _ := fail(fmt.Errorf("%w because flush_uploads processes the whole queue", errUnrestrictedUploadManagement))
		return r, out, nil
	}
	st, err := s.opt.FS.FlushUploads(ctx)
	out.Stats = uploadStats{Pending: st.Pending, Uploading: st.Uploading, Dead: st.Dead,
		Done: st.Done, Cancelling: st.Cancelling, Cancelled: st.Cancelled,
		Purging: st.Purging, RetainedBytes: st.RetainedBytes, QueuedBytes: st.Bytes}
	if err != nil {
		r, _ := fail(uploadManagementError(err))
		return r, out, nil
	}
	out.Completed = true
	out.Warning = "the queue snapshot drained locally; this is not independent remote reconciliation"
	return text("upload queue snapshot drained; no independent remote reconciliation is implied"), out, nil
}
