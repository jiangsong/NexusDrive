package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// errExportFailedFiles reports a finished job that failed some of its files.
// The exit status is what a script reads, so a job that copied 998 of 1000
// files must not look like a success.
var errExportFailedFiles = errors.New("export: some files failed; `cloudfs exports show <id>` lists them")

// runExport: cloudfs export <vpath>... <dest-dir> [--mirror --confirm] [--verify]
// [--transfers N] [--streams N] [--range-size 32MiB] [--wait] [--json]
//
// An export is a long, durable job the daemon owns, so there is no offline
// mode: the command hands the request to a running daemon and returns an ID.
func runExport(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "mirror", "confirm", "verify", "wait", "json")
	for key := range f.values {
		switch key {
		case "config", "timeout", "transfers", "streams", "range-size":
		default:
			return fmt.Errorf("export: unknown flag --%s", key)
		}
	}
	for key := range f.bools {
		switch key {
		case "mirror", "confirm", "verify", "wait", "json":
		default:
			return fmt.Errorf("export: --%s requires a value or is unknown", key)
		}
	}
	if len(f.args) < 2 {
		return errors.New("export: usage: export <virtual-path>... <destination-directory> [--mirror --confirm] [--verify] [--wait]")
	}
	q := control.ExportRequest{
		Sources: append([]string(nil), f.args[:len(f.args)-1]...),
		Dest:    f.args[len(f.args)-1],
		Mirror:  f.bool("mirror"),
		Verify:  f.bool("verify"),
		Confirm: f.bool("confirm"),
	}
	if q.Mirror && !q.Confirm {
		return errors.New("export: --mirror deletes everything in the destination that this export does not put there; pass --confirm")
	}
	if !q.Mirror && q.Confirm {
		return errors.New("export: --confirm belongs to --mirror; no other part of an export deletes anything")
	}
	var err error
	if q.Transfers, err = exportCount(f, "transfers"); err != nil {
		return err
	}
	if q.Streams, err = exportCount(f, "streams"); err != nil {
		return err
	}
	if raw := f.str("range-size", ""); raw != "" {
		size, err := config.ParseSize(raw)
		if err != nil {
			return err
		}
		q.RangeSize = int64(size)
	}
	if err := q.Validate(); err != nil {
		return err
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("export: --timeout must be a positive duration")
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	socket, tcp := cfg.Control.Socket, cfg.Control.Metrics
	start, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started, online, err := control.CallExport(start, socket, tcp, q)
	if err != nil {
		return err
	}
	if !online {
		return errExportNeedsDaemon
	}
	if !f.bool("wait") {
		if f.bool("json") {
			return json.NewEncoder(out).Encode(started)
		}
		fmt.Fprintf(out, "export %s queued to %s\n", started.ID, q.Dest)
		fmt.Fprintln(out, "follow it with `cloudfs exports show "+started.ID+"`")
		return nil
	}
	return waitForExport(ctx, out, socket, tcp, started.ID, f.bool("json"), timeout)
}

// errExportNeedsDaemon is the one thing every export command says when the
// daemon is down: there is no offline path for a job the daemon has to run.
var errExportNeedsDaemon = errors.New("export: the daemon is not running; start it with `cloudfs mount` (an export is a long job and has no offline mode)")

func exportCount(f *flags, name string) (int, error) {
	raw := f.str(name, "")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("export: --%s must be a positive whole number", name)
	}
	return n, nil
}

// waitForExport polls the job and keeps one progress line up to date, the way
// a copy tool does. It polls rather than following /events because the CLI
// wants one number, and a dropped SSE stream would have to be reconnected to
// learn that the job had already finished.
func waitForExport(ctx context.Context, out io.Writer, socket, tcp, id string, asJSON bool, timeout time.Duration) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		poll, cancel := context.WithTimeout(ctx, timeout)
		r, online, err := control.CallExports(poll, socket, tcp, control.ExportsRequest{ID: id})
		cancel()
		if err != nil {
			return err
		}
		if !online {
			return errExportNeedsDaemon
		}
		if len(r.Jobs) == 0 {
			return errors.New("export: the job disappeared from the queue")
		}
		job := r.Jobs[0]
		if !asJSON {
			fmt.Fprintf(out, "\r%s", exportProgressLine(job, r.Progress))
		}
		if !exportRunning(job.State) {
			if !asJSON {
				fmt.Fprintln(out)
			} else if err := json.NewEncoder(out).Encode(r); err != nil {
				return err
			}
			return exportOutcome(job)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func exportRunning(state string) bool {
	switch state {
	case "done", "failed", "cancelled":
		return false
	}
	return true
}

// exportOutcome turns a finished job into an exit status.
func exportOutcome(job control.ExportJobView) error {
	switch job.State {
	case "failed":
		return errors.New("export: the job failed; `cloudfs exports show " + job.ID + "` has the reason")
	case "cancelled":
		return errors.New("export: the job was cancelled")
	}
	if job.FilesFailed > 0 {
		return errExportFailedFiles
	}
	return nil
}

func exportProgressLine(job control.ExportJobView, p *control.ExportProgressView) string {
	pct := 0.0
	if job.BytesTotal > 0 {
		pct = float64(job.BytesDone) / float64(job.BytesTotal) * 100
	}
	line := fmt.Sprintf("%s %5.1f%%  %s/%s  %d/%d files",
		exportStateLabel(job), pct, humanBytes(job.BytesDone), humanBytes(job.BytesTotal),
		job.FilesDone+job.FilesSkipped, job.FilesTotal)
	if p != nil && p.Rate > 0 {
		line += fmt.Sprintf("  %s/s", humanBytes(int64(p.Rate)))
		if p.ETASeconds > 0 {
			line += "  ETA " + time.Duration(p.ETASeconds*float64(time.Second)).Round(time.Second).String()
		}
	}
	if job.FilesFailed > 0 {
		line += fmt.Sprintf("  %d failed", job.FilesFailed)
	}
	// Pad over whatever the previous, longer line left on the terminal.
	return line + "        "
}

func exportStateLabel(job control.ExportJobView) string {
	if job.State == "paused" && job.PauseReason != "" {
		return job.State + "(" + job.PauseReason + ")"
	}
	return job.State
}
