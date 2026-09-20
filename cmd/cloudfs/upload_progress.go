package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/upload"
)

// A copy into the mount is two phases: the write lands in the local journal
// and close(2) returns, then the upload queue sends it. Measured on a real
// tree, 3434 files took 37 seconds in the foreground and about twenty
// minutes in the background. The console has drawn a bar for the second
// phase all along; nothing in a terminal, a script or an agent could see it.
//
// This file is that bar for a terminal. It is modelled on the export
// progress line (export.go), which already solved the same problem for a
// different queue, and it reads the daemon's /status — the batch arithmetic
// belongs to internal/upload and is not repeated here.

// uploadWatchInterval is how often a watch re-reads /status. It matches the
// rate at which the daemon samples its own queue. A variable so tests need
// not wait a second per reading.
var uploadWatchInterval = time.Second

// errUploadsNeedDaemon is what every progress reader says when there is
// nothing to read from.
var errUploadsNeedDaemon = errors.New("uploads: the daemon is not running, so there is no queue to follow; start it with `cloudfs mount`")

// uploadBatchPercent renders the batch the way the console does: it prefers
// the percentage the daemon computed and sent, exactly as the page does.
//
// This binary is not necessarily the daemon's: a CLI left over from an
// earlier install talks to a newer daemon quite happily, and then two copies
// of one formula are two versions of it. Taking the served number leaves one
// answer per daemon. The local computation stays as the fallback for a
// daemon too old to send one, where a zero is indistinguishable from an
// absent field — and it is the same call, so both paths agree anyway.
func uploadBatchPercent(b control.UploadBatch) float64 {
	if b.Percent > 0 {
		return b.Percent
	}
	return upload.PercentOf(b.Active, b.FilesTotal, b.FilesDone, b.BytesTotal, b.BytesDone)
}

// uploadProgressLine renders one reading, without the padding a redraw needs.
func uploadProgressLine(st control.Status) string {
	b := st.Uploads.Batch
	line := fmt.Sprintf("%s %5.1f%%  %s/%s  %d/%d files",
		uploadStateLabel(b), uploadBatchPercent(b),
		humanBytes(b.BytesDone), humanBytes(b.BytesTotal), b.FilesDone, b.FilesTotal)
	if b.Active && b.Rate > 0 {
		line += fmt.Sprintf("  %s/s", humanBytes(int64(b.Rate)))
		if b.ETASeconds > 0 {
			line += "  ETA " + time.Duration(b.ETASeconds*float64(time.Second)).Round(time.Second).String()
		}
	}
	if st.Uploads.Blocked > 0 {
		line += fmt.Sprintf("  %d blocked", st.Uploads.Blocked)
	}
	if st.Uploads.Dead > 0 {
		line += fmt.Sprintf("  %d failed", st.Uploads.Dead)
	}
	return line
}

// uploadStateLabel says what is happening, and — for a batch a restart
// inherited — that the figures beside it start at the restart rather than at
// the copy. It never says "your copy": two copies at once are one batch here.
func uploadStateLabel(b control.UploadBatch) string {
	switch {
	case b.Active && b.Resumed:
		return "uploading (queued before the restart)"
	case b.Active:
		return "uploading"
	case b.FilesTotal == 0:
		return "idle"
	}
	return "uploaded"
}

// redrawUpload writes the line over the previous one, padded past whatever a
// longer predecessor left on the terminal.
func redrawUpload(out io.Writer, st control.Status) {
	fmt.Fprintf(out, "\r%s        ", uploadProgressLine(st))
}

// uploadQueueOutcome turns a drained queue into an exit status. Without it
// `cp -r ... && cloudfs uploads watch` reports success while files sit in the
// dead letter queue, which is the one case where being quiet is dangerous.
func uploadQueueOutcome(st control.Status) error {
	if st.Uploads.Dead > 0 {
		return fmt.Errorf("uploads: %d uploads failed permanently and their data is still on disk; `cloudfs uploads list` says why", st.Uploads.Dead)
	}
	return nil
}

// uploadQueueStuck reports a queue that is busy and cannot move: every
// pending row waits on a directory creation that is dead, cancelled or gone.
// Waiting for a batch like that to close is waiting forever, so a watch says
// so and stops — the same judgement the flush path makes.
func uploadQueueStuck(st control.Status) bool {
	u := st.Uploads
	return u.Blocked > 0 && u.Uploading == 0 && u.Pending == u.Blocked
}

// watchUploadQueue follows the queue until it drains. It is a plain reader of
// /status: no control-plane action, nothing to cancel, and a second watcher
// costs the daemon one more status render a second.
//
// It polls rather than following /events because a dropped stream would have
// to be reconnected to learn that the queue had already drained, which is
// exactly the moment this command exists to catch.
func watchUploadQueue(ctx context.Context, out io.Writer, socket, tcp string, pollTimeout time.Duration, asJSON bool) error {
	t := time.NewTicker(uploadWatchInterval)
	defer t.Stop()
	drawn := false
	var seq int64
	for {
		poll, cancel := context.WithTimeout(ctx, pollTimeout)
		st, online, err := control.FetchStatus(poll, socket, tcp)
		cancel()
		if err != nil {
			return err
		}
		if !online {
			return errUploadsNeedDaemon
		}
		b := st.Uploads.Batch
		// The queue's own counts, not the batch: the daemon samples once a
		// second, so a copy that finished a moment ago may not have opened a
		// batch yet, and a watch started right after `cp` must not mistake
		// that for an idle queue.
		busy := st.Uploads.Pending+st.Uploads.Uploading > 0
		if !asJSON {
			// A new batch is a new bar. Continuing the old line would draw a
			// percentage that falls and a total that shrinks.
			if drawn && b.Seq != seq {
				fmt.Fprintln(out)
				drawn = false
			}
			if b.Active || busy || b.FilesTotal > 0 {
				redrawUpload(out, st)
				drawn, seq = true, b.Seq
			}
		}
		switch {
		case uploadQueueStuck(st):
			if drawn {
				fmt.Fprintln(out)
			}
			if asJSON {
				if err := json.NewEncoder(out).Encode(st); err != nil {
					return err
				}
			}
			return fmt.Errorf("uploads: %d uploads are blocked behind a directory creation that failed or was cancelled and cannot proceed; `cloudfs uploads retry` requeues it", st.Uploads.Blocked)
		case !b.Active && !busy:
			if asJSON {
				if err := json.NewEncoder(out).Encode(st); err != nil {
					return err
				}
			} else if drawn {
				fmt.Fprintln(out)
			} else {
				fmt.Fprintln(out, "no uploads in progress")
			}
			return uploadQueueOutcome(st)
		}
		select {
		case <-ctx.Done():
			if drawn {
				fmt.Fprintln(out)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// runUploadsWatch is `cloudfs uploads watch`: follow the queue until it is
// empty, then exit — zero if everything went up, non-zero if anything did
// not. That exit status is the whole point of putting it in a script.
func runUploadsWatch(ctx context.Context, f *flags, out io.Writer) error {
	for _, k := range []string{"limit", "cursor"} {
		if _, ok := f.values[k]; ok {
			return fmt.Errorf("uploads: watch takes no --%s", k)
		}
	}
	for _, k := range []string{"all", "confirm"} {
		if f.bools[k] {
			return fmt.Errorf("uploads: watch takes no --%s", k)
		}
	}
	if len(f.args) > 1 {
		return errors.New("uploads: watch takes no arguments")
	}
	// The timeout bounds one reading, not the wait: a watch runs until the
	// queue drains, however long that is. Interrupting it stops the watch
	// and never the uploads.
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("uploads: --timeout must be a positive duration")
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	return watchUploadQueue(ctx, out, cfg.Control.Socket, cfg.Control.Metrics, timeout, f.bools["json"])
}

// flushWithProgress runs a flush while drawing the queue's progress. The
// flush call itself blocks until the queue is empty, so the readings come
// from /status beside it.
func flushWithProgress(ctx context.Context, socket, tcp string, q control.UploadRequest, out io.Writer) (control.UploadResponse, bool, error) {
	type result struct {
		response control.UploadResponse
		online   bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		r, online, err := control.CallUploads(ctx, socket, tcp, q)
		done <- result{r, online, err}
	}()
	t := time.NewTicker(uploadWatchInterval)
	defer t.Stop()
	drawn := false
	for {
		select {
		case r := <-done:
			if drawn {
				fmt.Fprintln(out)
			}
			return r.response, r.online, r.err
		case <-t.C:
			poll, cancel := context.WithTimeout(ctx, 5*time.Second)
			st, online, err := control.FetchStatus(poll, socket, tcp)
			cancel()
			// A reading that did not arrive is not worth failing a flush
			// over: the flush is the operation, this is only its picture.
			if err != nil || !online {
				continue
			}
			redrawUpload(out, st)
			drawn = true
		}
	}
}

// uploadBatchSummary is the batch as one line of `cloudfs status`, or "" when
// this daemon has never had one.
func uploadBatchSummary(st control.Status) string {
	if st.Uploads.Batch.Seq == 0 && st.Uploads.Batch.FilesTotal == 0 {
		return ""
	}
	line := uploadProgressLine(st)
	if st.Uploads.Batch.Resumed {
		line += " (counted since the daemon restarted, not since the copy began)"
	}
	return line
}
