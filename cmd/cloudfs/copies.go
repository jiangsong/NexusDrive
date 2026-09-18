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

func runCopies(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json", "confirm")
	for key := range f.values {
		if key != "config" && key != "timeout" && key != "limit" && key != "cursor" {
			return fmt.Errorf("copies: unknown flag --%s", key)
		}
	}
	for key := range f.bools {
		if key != "json" && key != "confirm" {
			return fmt.Errorf("copies: --%s requires a value or is unknown", key)
		}
	}
	q := control.CopiesRequest{Cursor: f.str("cursor", "")}
	action := f.arg(0)
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		if len(f.args) > 1 {
			return errors.New("copies list: unexpected argument")
		}
	case "show", "retry", "cancel", "forget":
		if len(f.args) != 2 {
			return fmt.Errorf("copies %s: give one copy ID", action)
		}
		q.ID = f.arg(1)
	default:
		return fmt.Errorf("copies: unknown action %q", action)
	}
	if raw := f.str("limit", ""); raw != "" {
		var err error
		q.Limit, err = strconv.Atoi(raw)
		if err != nil || q.Limit < 1 {
			return errors.New("copies: --limit must be between 1 and 1000")
		}
	}
	if err := q.Validate(); err != nil {
		return err
	}
	if (action == "forget") != f.bools["confirm"] {
		return errors.New("copies: forget requires --confirm; other actions do not accept it")
	}
	defaultTimeout := "30s"
	if action == "retry" {
		defaultTimeout = "30m"
	}
	timeout, err := time.ParseDuration(f.str("timeout", defaultTimeout))
	if err != nil || timeout <= 0 {
		return errors.New("copies: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	var result control.CopiesResponse
	var online bool
	mutation := action == "retry" || action == "cancel" || action == "forget"
	if mutation {
		result, online, err = control.CallCopyMutation(ctx, cfg.Control.Socket, cfg.Control.Metrics, control.CopyMutationRequest{Action: action, ID: q.ID, Confirm: f.bools["confirm"]})
	} else {
		result, online, err = control.CallCopies(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	}
	if err != nil {
		return err
	}
	if !online && mutation {
		dir := filepath.Join(cfg.StateDir(), "journal")
		if _, err := os.Stat(filepath.Join(dir, "journal.db")); err != nil {
			return err
		}
		var j *journal.Journal
		if action == "cancel" {
			// No active local IO exists when the offline process owns the
			// queue. Cancellation need not construct or authenticate a provider.
			j, err = journal.Open(journal.Options{Dir: dir, Durability: journal.Durability(cfg.Journal.Durability)})
			if err != nil {
				return err
			}
			defer j.Close()
			if !j.Owner() {
				return errors.New("copies: queue owner is running but its control endpoint is unavailable")
			}
			err = j.CancelCopy(ctx, q.ID)
		} else {
			d, openErr := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, RequireOwner: true, NoBackground: true})
			if openErr != nil {
				return openErr
			}
			defer d.Close()
			j = d.Journal
			if action == "forget" {
				err = d.FS.ForgetCopy(ctx, q.ID)
			} else {
				err = d.FS.RetryCopy(ctx, q.ID)
			}
		}
		if err != nil {
			return err
		}
		if action == "forget" {
			result = control.CopiesResponse{Copies: []control.CopyItem{}, Forgotten: q.ID}
		} else {
			result, err = control.InspectCopies(ctx, j, q)
		}
		if err != nil {
			return err
		}
	} else if !online {
		j, err := journal.OpenReadOnly(filepath.Join(cfg.StateDir(), "journal"))
		if errors.Is(err, os.ErrNotExist) {
			if q.ID != "" {
				return journal.ErrNotFound
			}
			result = control.CopiesResponse{Copies: []control.CopyItem{}}
		} else {
			if err != nil {
				return err
			}
			defer j.Close()
			result, err = control.InspectCopies(ctx, j, q)
			if err != nil {
				return err
			}
		}
	}
	if f.bools["json"] {
		return json.NewEncoder(out).Encode(result)
	}
	if action == "forget" {
		_, err := fmt.Fprintf(out, "forgotten copy %s; unreferenced private content removed, no local or remote file deleted (other links/open leases may retain bytes)\n", result.Forgotten)
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tCHECKPOINT/SIZE\tSOURCE\tTARGET\tUPLOAD")
	for _, job := range result.Copies {
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%q\t%q\t%s\n", job.ID, job.State, job.Checkpoint, job.Size, job.Source, job.Target, job.UploadID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if result.NextCursor != "" {
		fmt.Fprintf(out, "next_cursor: %s\n", result.NextCursor)
	}
	if action == "retry" {
		fmt.Fprintln(out, "retry queued, not completed; a running daemon resumes preparation")
	}
	if action == "cancel" {
		fmt.Fprintln(out, "preparation cancelled; retained content was not deleted")
	}
	return nil
}
