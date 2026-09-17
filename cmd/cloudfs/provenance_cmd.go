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

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// `cloudfs history <path>` and `cloudfs heat [prefix]` print the change
// record and the read heat (docs/agent-first-design.md §6) the way
// `cloudfs audit` prints the audit trail: through the running daemon's
// control plane when there is one, else straight from agent.db, read
// only. Both are what the inspector and the Agent screen show, for a
// terminal.

func runHistory(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	p := f.arg(0)
	if p == "" {
		return errors.New("history: usage: cloudfs history <mount-relative path> [--limit N] [--cursor C] [--json]")
	}
	limit, err := flagInt(f, "limit", 50)
	if err != nil || limit <= 0 || limit > 500 {
		return errors.New("history: --limit must be 1..500")
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, online, err := control.CallChanges(ctx, cfg.Control.Socket, cfg.Control.Metrics, p, f.str("cursor", ""), limit)
	if err != nil {
		return err
	}
	if !online {
		st, err := agent.OpenReadOnly(agentDirOf(cfg.Cache.Dir))
		if errors.Is(err, os.ErrNotExist) {
			result = control.ChangesResponse{Path: p, Changes: []agent.Change{}}
		} else if err != nil {
			return err
		} else {
			defer st.Close()
			var before int64
			if c := f.str("cursor", ""); c != "" {
				if before, err = strconv.ParseInt(c, 10, 64); err != nil {
					return errors.New("history: --cursor must be a row id")
				}
			}
			rows, more, err := st.HistoryPage(ctx, agent.HistoryQuery{Path: p, Before: before, Limit: limit})
			if err != nil {
				return err
			}
			result = control.ChangesResponse{Enabled: true, Path: agent.Normalise(p), Changes: rows}
			if more && len(rows) > 0 {
				result.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
			}
		}
	}
	if f.bool("json") {
		return json.NewEncoder(out).Encode(result)
	}
	if !result.Enabled {
		fmt.Fprintln(out, "no change record (this daemon keeps no agent.db)")
		return nil
	}
	if len(result.Changes) == 0 {
		fmt.Fprintln(out, "no recorded change")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tKIND\tORIGIN\tBY\tPATH")
	for _, c := range result.Changes {
		by := c.Principal
		if c.SessionID != "" {
			by = shortID(c.SessionID)
		}
		path := c.Path
		if c.From != "" {
			path = c.From + " -> " + c.Path
		}
		kind := c.Kind
		if !c.Reliable {
			kind += " (may be incomplete)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.TS.Local().Format("2006-01-02 15:04:05"), kind, c.Origin, by, path)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if result.NextCursor != "" {
		fmt.Fprintf(out, "more: --cursor %s\n", result.NextCursor)
	}
	return nil
}

func runHeat(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	prefix := f.arg(0)
	if prefix == "" {
		prefix = "/"
	}
	days, err := flagInt(f, "days", 30)
	if err != nil || days <= 0 || days > 365 {
		return errors.New("heat: --days must be 1..365")
	}
	limit, err := flagInt(f, "limit", 50)
	if err != nil || limit <= 0 || limit > 1000 {
		return errors.New("heat: --limit must be 1..1000")
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, online, err := control.CallHeat(ctx, cfg.Control.Socket, cfg.Control.Metrics, prefix, days, limit)
	if err != nil {
		return err
	}
	if !online {
		st, err := agent.OpenReadOnly(agentDirOf(cfg.Cache.Dir))
		if errors.Is(err, os.ErrNotExist) {
			result = control.HeatResponse{Path: prefix, Days: days, Entries: []control.HeatEntry{}}
		} else if err != nil {
			return err
		} else {
			defer st.Close()
			hot, err := st.HotPaths(ctx, prefix, days, limit)
			if err != nil {
				return err
			}
			// Offline there is no meta store to say how old a file is, so
			// every entry is placed by reads alone.
			result = control.HeatResponse{Enabled: cfg.MCP.Heat.On(), Path: agent.Normalise(prefix), Days: days, Entries: make([]control.HeatEntry, 0, len(hot))}
			for _, h := range hot {
				result.Entries = append(result.Entries, control.HeatEntry{Path: h.Path, Reads: h.Reads, ByKind: h.ByKind, LastRead: h.LastRead})
			}
		}
	}
	if f.bool("json") {
		return json.NewEncoder(out).Encode(result)
	}
	if !result.Enabled {
		fmt.Fprintln(out, "read heat is off (mcp.heat.enabled: false, or no agent.db)")
		return nil
	}
	if len(result.Entries) == 0 {
		fmt.Fprintf(out, "no reads under %s in the last %d days\n", result.Path, result.Days)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "READS\tAGENT\tKERNEL\tCONSOLE\tWEBDAV\tLAST READ\tCHANGED\tPATH")
	for _, e := range result.Entries {
		changed := "?"
		if e.MTime != nil {
			changed = e.MTime.Local().Format("2006-01-02")
			if e.Quadrant == "hot_stale" {
				changed += " (stale)"
			}
		}
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", e.Reads, e.ByKind["agent"], e.ByKind["kernel"], e.ByKind["console"], e.ByKind["webdav"],
			e.LastRead.Local().Format("2006-01-02 15:04"), changed, e.Path)
	}
	return w.Flush()
}

// flagInt reads a numeric flag with a default.
func flagInt(f *flags, name string, def int) (int, error) {
	v := f.str(name, "")
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

// agentDirOf is where agent.db lives under the cache directory.
func agentDirOf(cacheDir string) string { return filepath.Join(config.ExpandHome(cacheDir), "agent") }
