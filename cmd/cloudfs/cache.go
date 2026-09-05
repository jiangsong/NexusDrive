package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
)

func runCache(ctx context.Context, action string, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	for k := range f.values {
		if k != "config" && k != "timeout" {
			return fmt.Errorf("cache: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" {
			return fmt.Errorf("cache: invalid flag --%s", k)
		}
	}
	q := control.CacheRequest{Action: action}
	if action == "cache" {
		q.Action = f.arg(0)
		if q.Action == "" {
			q.Action = "stats"
		}
		if len(f.args) > 1 {
			return errors.New("cache: unexpected positional argument")
		}
	} else {
		q.Path = f.arg(0)
		if q.Path == "" && action == "warm" {
			q.Path = "/"
		}
		if q.Path != "" {
			q.Path = path.Clean(q.Path)
		}
		limit := 1
		if action == "warm" {
			limit = 2
			q.Depth = -1
			if depth := f.arg(1); depth != "" {
				n, err := strconv.Atoi(depth)
				if err != nil {
					return errors.New("warm: depth must be a number")
				}
				q.Depth = n
			}
		}
		if len(f.args) > limit {
			return errors.New("cache: unexpected positional argument")
		}
	}
	if err := q.Validate(); err != nil {
		return err
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30m"))
	if err != nil || timeout <= 0 {
		return errors.New("cache: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	result, online, err := control.CallCache(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if !online {
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, SkipWrite: true, RequireOwner: true, NoBackground: true})
		if err != nil {
			return err
		}
		defer d.Close()
		result, err = control.ManageCache(ctx, d.Collector(), q)
		if err != nil {
			return err
		}
	}
	if f.bools["json"] {
		return json.NewEncoder(out).Encode(result)
	}
	switch q.Action {
	case "pin":
		fmt.Fprintf(out, "cached and pinned %s\n", q.Path)
	case "unpin":
		fmt.Fprintf(out, "removed pin rule %s; cached bytes are retained and overlapping rules still apply\n", q.Path)
	case "warm":
		fmt.Fprintf(out, "listed %d directories under %s\n", result.Directories, q.Path)
	case "gc":
		fmt.Fprintf(out, "freed %d bytes\n", result.FreedBytes)
	case "pins":
		for _, p := range result.Pins {
			fmt.Fprintf(out, "%s recursive=%t configured=%t\n", p.Path, p.Recursive, p.Configured)
		}
	case "stats":
		s := result.Stats
		fmt.Fprintf(out, "blocks       %d\nbytes        %d\nhit ratio    %.1f%% (%d hits / %d misses)\nevictions    %d\nhydrated     %d files\npinned       %d blocks\n", s.Blocks, s.Bytes, s.HitRatio()*100, s.Hits, s.Misses, s.Evictions, s.HydratedFiles, s.PinnedBlocks)
		fmt.Fprintf(out, "whole bytes  %d\nleased bytes %d\norphan bytes %d\ncache reserved   %d\nwrite reserved   %d\n", s.WholeBytes, s.LeasedBytes, s.OrphanBytes, s.ReservedBytes, s.WriteReservedBytes)
	}
	if result.Warning != "" {
		fmt.Fprintln(out, "warning:", result.Warning)
	}
	return nil
}
