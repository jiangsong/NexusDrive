package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"cloudfs/internal/control"
)

// runExports: cloudfs exports list|show|pause|resume|cancel|forget <id> [--confirm].
//
// Every action needs the running daemon: it owns exports.db, and an export is
// a job rather than a state a second process could safely edit underneath it.
func runExports(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json", "confirm")
	for key := range f.values {
		switch key {
		case "config", "timeout", "limit", "cursor":
		default:
			return fmt.Errorf("exports: unknown flag --%s", key)
		}
	}
	for key := range f.bools {
		switch key {
		case "json", "confirm":
		default:
			return fmt.Errorf("exports: --%s requires a value or is unknown", key)
		}
	}
	action := f.arg(0)
	if action == "" {
		action = "list"
	}
	q := control.ExportsRequest{Cursor: f.str("cursor", "")}
	switch action {
	case "list":
		if len(f.args) > 1 {
			return errors.New("exports list: unexpected argument")
		}
	case "show", "pause", "resume", "cancel", "forget":
		if len(f.args) != 2 {
			return fmt.Errorf("exports %s: give one export job ID", action)
		}
		q.ID = f.arg(1)
	default:
		return fmt.Errorf("exports: unknown action %q", action)
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return errors.New("exports: --limit must be a positive whole number")
		}
		q.Limit = n
	}
	if err := q.Validate(); err != nil {
		return err
	}
	confirm := f.bool("confirm")
	if (action == "forget") != confirm {
		return errors.New("exports: forget requires --confirm; the other actions do not accept it")
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("exports: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	socket, tcp := cfg.Control.Socket, cfg.Control.Metrics
	var result control.ExportsResponse
	var online bool
	if action == "list" || action == "show" {
		result, online, err = control.CallExports(ctx, socket, tcp, q)
	} else {
		result, online, err = control.CallExportMutation(ctx, socket, tcp,
			control.ExportMutationRequest{Action: action, ID: q.ID, Confirm: confirm})
	}
	if err != nil {
		return err
	}
	if !online {
		return errExportNeedsDaemon
	}
	if f.bool("json") {
		return json.NewEncoder(out).Encode(result)
	}
	if action == "forget" {
		_, err := fmt.Fprintf(out, "forgotten export %s; its part files were removed and the finished copies were kept\n", result.Forgotten)
		return err
	}
	if action == "list" {
		return printExportList(out, result)
	}
	// show, and every mutation but forget, answer with the job as it stands
	// now: the point of pausing something is seeing that it paused.
	return printExportJob(out, result)
}

func printExportList(out io.Writer, r control.ExportsResponse) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tDONE/TOTAL\tFILES\tDESTINATION")
	for _, job := range r.Jobs {
		fmt.Fprintf(w, "%s\t%s\t%s/%s\t%d/%d\t%q\n", job.ID, exportStateLabel(job),
			humanBytes(job.BytesDone), humanBytes(job.BytesTotal),
			job.FilesDone+job.FilesSkipped, job.FilesTotal, job.Dest)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(r.Jobs) == 0 {
		fmt.Fprintln(out, "no export jobs")
	}
	if r.NextCursor != "" {
		fmt.Fprintf(out, "next_cursor: %s\n", r.NextCursor)
	}
	return nil
}

func printExportJob(out io.Writer, r control.ExportsResponse) error {
	if len(r.Jobs) == 0 {
		return errors.New("exports: no such export job")
	}
	job := r.Jobs[0]
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "id\t%s\n", job.ID)
	fmt.Fprintf(w, "state\t%s\n", exportStateLabel(job))
	for _, src := range job.Sources {
		fmt.Fprintf(w, "source\t%s\n", src)
	}
	fmt.Fprintf(w, "destination\t%s\n", job.Dest)
	fmt.Fprintf(w, "mirror\t%t\n", job.Mirror)
	fmt.Fprintf(w, "verify\t%t\n", job.Verify)
	fmt.Fprintf(w, "bytes\t%s/%s\n", humanBytes(job.BytesDone), humanBytes(job.BytesTotal))
	fmt.Fprintf(w, "files\tdone %d, skipped %d, failed %d, planned %d\n",
		job.FilesDone, job.FilesSkipped, job.FilesFailed, job.FilesTotal)
	if r.Progress != nil && r.Progress.Rate > 0 {
		fmt.Fprintf(w, "rate\t%s/s\n", humanBytes(int64(r.Progress.Rate)))
		if r.Progress.ETASeconds > 0 {
			fmt.Fprintf(w, "eta\t%s\n", time.Duration(r.Progress.ETASeconds*float64(time.Second)).Round(time.Second))
		}
	}
	if job.LastError != "" {
		fmt.Fprintf(w, "last error\t%s\n", job.LastError)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Only the items that still need attention: a plan is thousands of rows,
	// and the finished ones are on the disk where the person can see them.
	items := make([]control.ExportItemView, 0, 16)
	for _, it := range r.Items {
		if it.State == "failed" || it.State == "active" || it.LastError != "" {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	iw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(iw, "FILE\tSTATE\tDONE/SIZE\tATTEMPTS\tERROR")
	for _, it := range items {
		fmt.Fprintf(iw, "%s\t%s\t%s/%s\t%d\t%s\n", it.Rel, it.State,
			humanBytes(it.DoneBytes), humanBytes(it.Size), it.Attempts, it.LastError)
	}
	return iw.Flush()
}
