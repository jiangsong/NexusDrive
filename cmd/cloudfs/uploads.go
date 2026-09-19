package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
)

func runUploads(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "all", "json", "confirm")
	for k := range f.values {
		if k != "config" && k != "timeout" && k != "limit" && k != "cursor" {
			return fmt.Errorf("uploads: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "all" && k != "json" && k != "confirm" {
			return fmt.Errorf("uploads: --%s requires a value or is unknown", k)
		}
	}
	action := f.arg(0)
	if action == "" {
		action = "list"
	}
	if action != "list" && action != "retry" && action != "flush" && action != "drop" && action != "cancel" && action != "resume" {
		return fmt.Errorf("uploads: unknown subcommand %q", action)
	}
	if len(f.args) > 2 || (len(f.args) > 1 && action != "retry" && action != "drop" && action != "cancel" && action != "resume") {
		return errors.New("uploads: unexpected positional argument")
	}
	q := control.UploadRequest{Action: action, ID: f.arg(1), Cursor: f.str("cursor", ""), Confirm: f.bools["confirm"]}
	if q.Confirm && action != "resume" && action != "drop" {
		return errors.New("uploads: --confirm is only valid for resume or drop")
	}
	if v := f.str("limit", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return errors.New("uploads: --limit must be between 1 and 1000")
		}
		q.Limit = n
	}
	if action == "retry" {
		q.All = f.bools["all"] || q.ID == ""
	}
	if f.bools["all"] && action != "retry" {
		return errors.New("uploads: --all is only valid for retry")
	}
	if err := q.Validate(); err != nil {
		return err
	}
	defaultTimeout := "30s"
	if action == "flush" || action == "resume" {
		defaultTimeout = "30m"
	}
	timeout, err := time.ParseDuration(f.str("timeout", defaultTimeout))
	if err != nil || timeout <= 0 {
		return errors.New("uploads: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}

	// Online management must not instantiate a second provider/cache
	// stack or touch the owner's journal through another process.
	result, online, err := control.CallUploads(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if online {
		return printUploads(out, q, result, f.bools["json"])
	}
	dir := filepath.Join(cfg.StateDir(), "journal")
	if action == "list" {
		j, err := journal.OpenReadOnly(dir)
		if errors.Is(err, os.ErrNotExist) {
			return printUploads(out, q, control.UploadResponse{}, f.bools["json"])
		}
		if err != nil {
			return err
		}
		defer j.Close()
		result, err = control.ManageUploads(ctx, j, nil, nil, q)
		if err != nil {
			return err
		}
	} else if action == "drop" {
		result, err = control.ManageUploadDiscard(ctx, func(ctx context.Context, id string, confirm bool) error {
			return daemon.DiscardUploadOffline(ctx, cfg, id, confirm)
		}, q)
		if err != nil {
			return err
		}
	} else if action == "flush" || action == "resume" {
		if action == "resume" {
			if _, err := os.Stat(filepath.Join(dir, "journal.db")); err != nil {
				return err
			}
		}
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, NoBackground: action == "resume"})
		if err != nil {
			return err
		}
		defer d.Close()
		if action == "resume" {
			result, err = control.ManageUploadResume(ctx, d.FS.ResumeUpload, q)
		} else {
			result, err = control.ManageUploads(ctx, d.Journal, d.Collector().FlushUploads, d.Collector().CancelUpload, q)
		}
		if err != nil {
			return err
		}
	} else {
		// Retrying or cancelling offline rows does not need credentials,
		// recovery, a VFS, or background uploads. Require the ownership lock.
		if _, err := os.Stat(filepath.Join(dir, "journal.db")); err != nil {
			return err
		}
		j, err := journal.Open(journal.Options{Dir: dir, Durability: journal.Durability(cfg.Journal.Durability)})
		if err != nil {
			return err
		}
		defer j.Close()
		if !j.Owner() {
			return errors.New("uploads: another process owns the queue but its control endpoint is unavailable")
		}
		cancelOffline := func(ctx context.Context, id string) (journal.State, error) {
			state, err := j.RequestCancel(ctx, id)
			if err == nil && state == journal.StateCancelling {
				// This process owns the queue and has started no workers.
				err = j.FinishCancel(ctx, id)
				if err == nil {
					state = journal.StateCancelled
				}
			}
			return state, err
		}
		result, err = control.ManageUploads(ctx, j, nil, cancelOffline, q)
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	return printUploads(out, q, result, f.bools["json"])
}

func printUploads(out io.Writer, q control.UploadRequest, result control.UploadResponse, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	switch q.Action {
	case "list":
		if len(result.Uploads) == 0 {
			fmt.Fprintln(out, "no queued or failed uploads")
			return nil
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tREMOTE\tNAME\tSIZE\tATTEMPTS\tLAST ERROR")
		for _, u := range result.Uploads {
			state := string(u.State)
			if u.NeedsPublish {
				state += " (local publication pending)"
			}
			name := u.Name
			if u.Kind == journal.KindMkdir {
				name += "/"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%q\t%d\t%d\t%q\n", u.ID, state, u.Remote, name, u.Size, u.Attempt, u.LastError)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if result.NextCursor != "" {
			fmt.Fprintf(out, "more uploads: use uploads list --cursor %s\n", result.NextCursor)
		}
	case "retry":
		fmt.Fprintf(out, "requeued %d upload(s)\n", result.Requeued)
	case "flush":
		fmt.Fprintln(out, "upload queue drained; no pending, in-flight or failed uploads")
	case "cancel", "resume":
		fmt.Fprintf(out, "upload %s: %s; %s\n", q.ID, result.State, result.Warning)
	case "drop":
		fmt.Fprintf(out, "discarded upload %s; %s\n", result.Discarded, result.Warning)
	}
	return nil
}
