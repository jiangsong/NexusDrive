package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
)

func runCopy(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	for k := range f.values {
		if k != "config" && k != "timeout" {
			return fmt.Errorf("cp: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" {
			return fmt.Errorf("cp: invalid flag --%s", k)
		}
	}
	if len(f.args) != 2 {
		return errors.New("cp: give source and destination virtual file paths")
	}
	q := control.CopyRequest{From: path.Clean(f.args[0]), To: path.Clean(f.args[1])}
	if err := q.Validate(); err != nil {
		return err
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30m"))
	if err != nil || timeout <= 0 {
		return errors.New("cp: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	result, online, err := control.CallCopy(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if !online {
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, RequireOwner: true, NoBackground: true})
		if err != nil {
			return err
		}
		defer d.Close()
		result.File, err = d.FS.Copy(ctx, q.From, q.To)
		if err != nil {
			return err
		}
	}
	if f.bools["json"] {
		return json.NewEncoder(out).Encode(result)
	}
	fmt.Fprintf(out, "copied %s to %s (%d bytes)\n", q.From, q.To, result.File.Size)
	if !online {
		fmt.Fprintln(out, "writeback copies are journaled locally; start the daemon or use uploads flush to finish queued uploads")
	}
	return nil
}
