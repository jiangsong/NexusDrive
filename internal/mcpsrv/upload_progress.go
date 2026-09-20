package mcpsrv

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// upload_progress (docs/mcp.md) answers the question the row-level upload
// tools could not: not "what is in the queue" but "how far along is it".
//
// An agent that writes a tree into the mount gets its calls back long before
// the bytes leave the machine — the write is durable locally at that point,
// not on the drive — and had no way to find out whether the drive had caught
// up. list_uploads showed rows, which is the wrong shape for the question: a
// thousand of them shrinking one a second is not an answer.
//
// It is read-only, costs one queue reading, and takes no path: the queue is
// the daemon's, not a subtree's. The batch figures come from the uploader's
// own sampler (FS.UploadProgress) and cost nothing at all.

type uploadProgressOutput struct {
	// Active says whether a batch is being worked through right now.
	Active bool `json:"active"`
	// Batch is the batch number. It changing between two calls means the
	// queue emptied and filled again, so the figures do not continue.
	Batch int64 `json:"batch"`
	// Resumed marks a batch that was already queued when the daemon
	// started: its done counts run from the restart, not from the copy.
	Resumed      bool    `json:"resumed,omitempty"`
	Percent      float64 `json:"percent"`
	FilesTotal   int64   `json:"files_total"`
	FilesDone    int64   `json:"files_done"`
	BytesTotal   int64   `json:"bytes_total"`
	BytesDone    int64   `json:"bytes_done"`
	Rate         float64 `json:"rate,omitempty"`
	ETASeconds   float64 `json:"eta_seconds,omitempty"`
	StartedAt    string  `json:"started_at,omitempty"`
	FinishedAt   string  `json:"finished_at,omitempty"`
	Queued       int     `json:"queued"`
	InFlight     int     `json:"in_flight"`
	Blocked      int     `json:"blocked"`
	Failed       int     `json:"failed"`
	QueuedBytes  int64   `json:"queued_bytes"`
	CleanupState string  `json:"cleanup_state,omitempty"`
}

func (s *Server) registerUploadProgressTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "upload_progress",
		Description: "Report how far along the upload queue is: percent, files and bytes done of the current batch, the live rate and an estimate, plus how many rows are queued, in flight, blocked or failed. " +
			"Writing to this mount returns as soon as the data is durable locally; the upload to the provider happens afterwards, and this is how to tell whether it has finished. " +
			"The queue is shared, so the batch covers everything being uploaded right now, not only what this session wrote.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, s.uploadProgress)
}

func (s *Server) uploadProgress(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, uploadProgressOutput, error) {
	p := s.opt.FS.UploadProgress()
	out := uploadProgressOutput{
		Active: p.Active, Batch: p.Seq, Resumed: p.Resumed, Percent: p.Percent(),
		FilesTotal: p.FilesTotal, FilesDone: p.FilesDone,
		BytesTotal: p.BytesTotal, BytesDone: p.BytesDone,
		Rate: p.Rate, ETASeconds: p.ETA.Seconds(),
	}
	if !p.StartedAt.IsZero() {
		out.StartedAt = p.StartedAt.UTC().Format(time.RFC3339)
	}
	if !p.FinishedAt.IsZero() {
		out.FinishedAt = p.FinishedAt.UTC().Format(time.RFC3339)
	}
	// The queue's own counts, read now. The daemon samples the batch once a
	// second, so a write made a moment ago is in these numbers before it is
	// in that one — and a tool that answered "idle" with rows still queued
	// would be worse than no tool.
	if j := s.opt.FS.Journal(); j != nil {
		if st, err := j.Stats(ctx); err == nil {
			out.Queued, out.InFlight = st.Pending, st.Uploading
			out.Blocked, out.Failed, out.QueuedBytes = st.Blocked, st.Dead, st.Bytes
			if st.Purging+st.Cancelling+st.Cancelled > 0 {
				out.CleanupState = "some uploads were stopped or are being cleaned up; see list_uploads"
			}
		}
	}
	return text("%s", uploadProgressSentence(out)), out, nil
}

// uploadProgressSentence is the text half: what an agent should do next,
// which is either nothing or wait and ask again.
func uploadProgressSentence(o uploadProgressOutput) string {
	switch {
	case o.Active:
		line := fmt.Sprintf("the upload queue is %.1f%% through batch %d: %d of %d files, %d of %d bytes",
			o.Percent, o.Batch, o.FilesDone, o.FilesTotal, o.BytesDone, o.BytesTotal)
		if o.Resumed {
			line += " (counted since the daemon restarted, not since the writes began)"
		}
		if o.ETASeconds > 0 {
			line += fmt.Sprintf("; about %s left at the current rate", time.Duration(o.ETASeconds*float64(time.Second)).Round(time.Second))
		}
		return line + ". Call again to follow it."
	case o.Blocked > 0:
		return fmt.Sprintf("%d queued uploads cannot run: they are waiting on a directory creation that failed or was cancelled. They will not move until someone retries it.", o.Blocked)
	case o.Queued+o.InFlight > 0:
		return fmt.Sprintf("%d uploads are queued and %d in flight; the current batch has not been sampled yet. Call again in a second.", o.Queued, o.InFlight)
	case o.Failed > 0:
		return fmt.Sprintf("the upload queue is empty, but %d uploads failed permanently and their data is still on this machine; list_uploads says why.", o.Failed)
	case o.Batch > 0:
		return fmt.Sprintf("the upload queue is empty: batch %d finished %d files (%d bytes). Everything written has reached the provider.", o.Batch, o.FilesDone, o.BytesDone)
	}
	return "the upload queue is empty and nothing has been uploaded since this daemon started."
}
