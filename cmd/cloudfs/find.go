package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// runFind answers `cloudfs find` from a running daemon, or from the local
// index alone when none runs — the same offline rule as runCache: read the
// store, start no upload and no refresh. --all lists the whole tree first
// and is the one thing that touches the remote.
func runFind(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "all", "json")
	for k := range f.values {
		switch k {
		case "config", "timeout", "ext", "size", "after", "sort", "limit", "type", "path":
		default:
			return fmt.Errorf("find: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "all" && k != "json" {
			return fmt.Errorf("find: invalid flag --%s", k)
		}
	}
	if len(f.args) > 1 {
		return errors.New("find: quote a query that has spaces")
	}
	q := control.SearchQuery{
		Q: f.arg(0), Path: f.str("path", ""), Ext: f.str("ext", ""), Kind: f.str("type", ""),
		After: f.str("after", ""), Sort: f.str("sort", ""), Limit: 100,
	}
	if v := f.str("limit", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New("find: --limit must be a positive number")
		}
		q.Limit = n
	}
	// The flags are the grammar's key:value terms; folding them into the
	// query keeps one parser and one set of error messages.
	var terms []string
	if strings.TrimSpace(q.Q) != "" {
		terms = append(terms, q.Q)
	}
	if v := f.str("size", ""); v != "" {
		terms = append(terms, "size:"+v)
	}
	filter, err := meta.ParseQuery(strings.Join(terms, " "))
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	q.Q = strings.Join(terms, " ")
	if q.Kind != "" && q.Kind != "dir" && q.Kind != "file" {
		return errors.New("find: --type must be dir or file")
	}
	if q.After != "" {
		if _, err := time.Parse(time.RFC3339, q.After); err != nil {
			if _, err := time.ParseInLocation("2006-01-02", q.After, time.Local); err != nil {
				return errors.New("find: --after must be an RFC 3339 time or a YYYY-MM-DD date")
			}
		}
	}
	if filter.Empty() && q.Ext == "" && q.Kind == "" && q.After == "" {
		return errors.New("find: give a query or a filter (--ext, --size, --after, --type)")
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30m"))
	if err != nil || timeout <= 0 {
		return errors.New("find: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	if f.bools["all"] {
		if err := crawlBeforeFind(ctx, cfg); err != nil {
			return err
		}
	}
	res, online, err := control.CallSearch(ctx, cfg.Control.Socket, cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if !online {
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, SkipWrite: true, RequireOwner: true, NoBackground: true})
		if err != nil {
			return err
		}
		defer d.Close()
		res, err = findOffline(ctx, d, q, filter)
		if err != nil {
			return err
		}
	}
	if f.bools["json"] {
		return json.NewEncoder(out).Encode(res)
	}
	for _, r := range res.Results {
		fmt.Fprintln(out, r.Path)
	}
	if len(res.Results) == 0 {
		hint := "no matches in the local index"
		if res.Coverage.Listed < res.Coverage.Known {
			hint += fmt.Sprintf(" (%d of %d known directories listed)", res.Coverage.Listed, res.Coverage.Known)
		}
		fmt.Fprintln(os.Stderr, hint+"; run 'cloudfs find --all' to list the whole tree first")
	}
	if !res.Complete {
		// Saying nothing here would present a partial answer as the whole one.
		fmt.Fprintln(os.Stderr, "the index stopped at its work budget; there may be more matches — narrow the query or use --limit")
	}
	return nil
}

// crawlBeforeFind lists every directory the index does not hold. A running
// daemon's crawler takes the pass and this waits for it; without a daemon
// the pass runs here, on a store opened for that purpose only.
func crawlBeforeFind(ctx context.Context, cfg *config.Config) error {
	req := control.CacheRequest{Action: "warm", Path: "/", Depth: -1, All: true, Confirm: true}
	res, online, err := control.CallCache(ctx, cfg.Control.Socket, cfg.Control.Metrics, req)
	if err != nil {
		return err
	}
	if !online {
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, SkipWrite: true, RequireOwner: true, NoBackground: true})
		if err != nil {
			return err
		}
		defer d.Close()
		_, err = control.ManageCache(ctx, d.Collector(), req)
		return err
	}
	if !res.Queued {
		return nil
	}
	for {
		st, _, err := control.FetchStatus(ctx, cfg.Control.Socket, cfg.Control.Metrics)
		if err != nil {
			return err
		}
		if !st.Crawl.Running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// findOffline mirrors the /search route over a store opened read-only.
func findOffline(ctx context.Context, d *daemon.Daemon, q control.SearchQuery, filter meta.Filter) (control.SearchResponse, error) {
	var out control.SearchResponse
	for _, ext := range strings.Split(q.Ext, ",") {
		if ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), "."); ext != "" {
			filter.Ext = append(filter.Ext, ext)
		}
	}
	if q.Kind != "" {
		filter.Kind = q.Kind
	}
	if q.After != "" {
		t, err := time.Parse(time.RFC3339, q.After)
		if err != nil {
			t, _ = time.ParseInLocation("2006-01-02", q.After, time.Local)
		}
		filter.ModifiedAfter = t
	}
	roots := []string{"/"}
	if q.Path != "" {
		roots = []string{q.Path}
	}
	answer, err := d.FS.Search(ctx, meta.SearchQuery{Filter: filter, Roots: roots, Limit: q.Limit, Sort: q.Sort})
	if err != nil {
		return out, err
	}
	out.Complete = answer.Complete
	out.Coverage = control.SearchCoverage{Listed: answer.Coverage.Listed, Known: answer.Coverage.Known}
	out.Results = make([]control.SearchHit, 0, len(answer.Results))
	for _, r := range answer.Results {
		kind := "file"
		if r.Kind == provider.KindDir {
			kind = "dir"
		}
		out.Results = append(out.Results, control.SearchHit{Name: r.Name, Path: r.Path, Kind: kind, Size: r.Size, MTime: r.MTime, Cached: r.Cached})
	}
	return out, nil
}
