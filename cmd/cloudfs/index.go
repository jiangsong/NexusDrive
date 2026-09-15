package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/index"
)

// indexSearchFromArgs turns `cloudfs index search <query> [--path P]
// [--mode M] [--limit N]` into a query. The query is every positional
// argument joined, so a phrase needs no quoting.
func indexSearchFromArgs(args []string) (index.SearchQuery, error) {
	f := parseFlags(args, "json")
	for k := range f.values {
		switch k {
		case "config", "path", "mode", "limit", "timeout":
		default:
			return index.SearchQuery{}, fmt.Errorf("index search: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" {
			return index.SearchQuery{}, fmt.Errorf("index search: --%s requires a value or is unknown", k)
		}
	}
	q := index.SearchQuery{Query: strings.TrimSpace(strings.Join(f.args, " ")), Mode: f.str("mode", "")}
	if q.Query == "" {
		return index.SearchQuery{}, errors.New("index search: give a query")
	}
	if p := f.str("path", ""); p != "" {
		if !strings.HasPrefix(p, "/") {
			return index.SearchQuery{}, errors.New("index search: --path must be an absolute virtual path")
		}
		q.Roots = []string{p}
	}
	switch q.Mode {
	case "", "keyword", "hybrid", "vector":
	default:
		return index.SearchQuery{}, errors.New("index search: --mode must be keyword, hybrid or vector")
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return index.SearchQuery{}, errors.New("index search: --limit must be between 1 and 100")
		}
		q.TopK = n
	}
	return q, nil
}

// runIndex is `cloudfs index <action>`. status, rules and search work
// without a daemon by opening index.db read-only; everything that changes
// the index goes through the running daemon, which owns the worker.
func runIndex(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json", "confirm")
	action := f.arg(0)
	if action == "" {
		return errors.New("index: usage: cloudfs index status|rules|add|rm|rebuild|retry|search ...")
	}
	rest := dropFirstPositional(args, "json", "confirm")
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("index: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	c := indexCLI{cfg: cfg, out: out, asJSON: f.bools["json"]}
	switch action {
	case "status":
		return c.status(ctx, rest)
	case "rules":
		return c.rules(ctx)
	case "add":
		return c.add(ctx, rest)
	case "rm", "remove":
		return c.remove(ctx, rest)
	case "rebuild":
		return c.rebuild(ctx, rest)
	case "retry":
		return c.retry(ctx, rest)
	case "search":
		return c.search(ctx, rest)
	}
	return fmt.Errorf("index: unknown action %q", action)
}

// dropFirstPositional removes the action word from the argument list,
// reading flags the way parseFlags does so a flag value that happens to
// spell the action is left alone.
func dropFirstPositional(args []string, boolFlags ...string) []string {
	isBool := map[string]bool{}
	for _, b := range boolFlags {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return append(append([]string{}, args[:i]...), args[i+1:]...)
		}
		name := strings.TrimPrefix(a, "--")
		if strings.Contains(name, "=") || isBool[name] {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
		}
	}
	return args
}

type indexCLI struct {
	cfg    *config.Config
	out    io.Writer
	asJSON bool
}

func (c *indexCLI) call(ctx context.Context, method, target string, body, out any) (bool, error) {
	return control.CallIndex(ctx, c.cfg.Control.Socket, c.cfg.Control.Metrics, method, target, body, out)
}

// offline opens index.db read-only. It answers os.ErrNotExist (wrapped)
// when no index was ever built, which the callers turn into a sentence
// about index.enabled rather than a stack of paths.
func (c *indexCLI) offline() (*index.Store, error) {
	st, err := index.OpenStoreReadOnly(c.cfg.Cache.Dir)
	if errors.Is(err, os.ErrNotExist) {
		if !c.cfg.Index.Enabled {
			return nil, errIndexDisabled
		}
		return nil, errors.New("index: no index.db yet; start `cloudfs mount` with index.enabled true to build it")
	}
	return st, err
}

var errIndexDisabled = errors.New("index: content indexing is off (index.enabled: false)")

func (c *indexCLI) needDaemon(online bool) error {
	if online {
		return nil
	}
	return errors.New("index: this action requires the running daemon; start `cloudfs mount` first")
}

func (c *indexCLI) status(ctx context.Context, args []string) error {
	f := parseFlags(args, "json")
	p := f.str("path", "")
	if len(f.args) > 0 {
		return errors.New("index status: unexpected argument; use --path to ask about one path")
	}
	st, online, err := control.CallIndexStatus(ctx, c.cfg.Control.Socket, c.cfg.Control.Metrics, p)
	if err != nil {
		return err
	}
	if !online {
		store, err := c.offline()
		if errors.Is(err, errIndexDisabled) {
			if c.asJSON {
				return json.NewEncoder(c.out).Encode(control.IndexStatusBrief{Enabled: false})
			}
			fmt.Fprintln(c.out, "content indexing is off (index.enabled: false)")
			return nil
		}
		if err != nil {
			return err
		}
		defer store.Close()
		if st, err = offlineIndexStatus(ctx, store); err != nil {
			return err
		}
		if !c.asJSON {
			fmt.Fprintln(c.out, "offline: read from index.db; progress and per-path state need the running daemon")
		}
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(st)
	}
	if !st.Enabled {
		fmt.Fprintln(c.out, "content indexing is off (index.enabled: false)")
		return nil
	}
	printIndexStatus(c.out, st, p)
	return nil
}

// offlineIndexStatus is the daemon's Status without the parts only the
// worker knows: the fetch budget and the progress counters stay zero.
func offlineIndexStatus(ctx context.Context, store *index.Store) (index.Status, error) {
	stats, err := store.Stats(ctx)
	if err != nil {
		return index.Status{}, err
	}
	failed, _, err := store.Failed(ctx, "", 20)
	if err != nil {
		return index.Status{}, err
	}
	return index.Status{
		Enabled: true, Docs: index.DocCounts{OK: stats.DocsOK, Dirty: stats.DocsDirty, Failed: stats.DocsFailed},
		ChunksTotal: stats.Chunks, Pending: stats.Pending, Failed: failed, TextBytes: stats.TextBytes,
	}, nil
}

func printIndexStatus(out io.Writer, st index.Status, p string) {
	if p != "" {
		fmt.Fprintf(out, "%s: %s", p, st.State)
		if st.Covered != "" {
			fmt.Fprintf(out, " (rule %s, %s)", st.Covered, st.RuleSource)
		}
		if st.Chunks > 0 {
			fmt.Fprintf(out, ", %d chunks", st.Chunks)
		}
		if st.Error != "" {
			fmt.Fprintf(out, ": %s", st.Error)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "documents: %d ok, %d dirty, %d failed; %d pending; %d chunks\n", st.Docs.OK, st.Docs.Dirty, st.Docs.Failed, st.Pending, st.ChunksTotal)
	if st.MaxTotalText > 0 {
		fmt.Fprintf(out, "text: %s of %s\n", humanBytes(st.TextBytes), humanBytes(st.MaxTotalText))
	} else {
		fmt.Fprintf(out, "text: %s\n", humanBytes(st.TextBytes))
	}
	if st.FetchBudget.Limit > 0 {
		fmt.Fprintf(out, "fetched this hour: %s of %s (%s since start)\n", humanBytes(st.FetchBudget.Used), humanBytes(st.FetchBudget.Limit), humanBytes(st.FetchBytesTotal))
	} else if st.FetchBytesTotal > 0 {
		fmt.Fprintf(out, "fetched since start: %s\n", humanBytes(st.FetchBytesTotal))
	}
	pr := st.Progress
	switch {
	case pr.Paused != "" && !pr.ResumeAt.IsZero():
		fmt.Fprintf(out, "worker: paused (%s) until %s\n", pr.Paused, pr.ResumeAt.Local().Format("15:04:05"))
	case pr.Paused != "":
		fmt.Fprintf(out, "worker: paused (%s)\n", pr.Paused)
	case pr.Running && pr.Extracting > 0:
		fmt.Fprintf(out, "worker: extracting, %d pending\n", pr.Pending)
	case pr.Running:
		fmt.Fprintln(out, "worker: idle")
	}
	if len(st.Failed) > 0 {
		fmt.Fprintln(out, "recent failures:")
		for _, d := range st.Failed {
			fmt.Fprintf(out, "  %s (%s): %s\n", d.Path, d.Kind, d.Error)
		}
	}
}

func (c *indexCLI) rules(ctx context.Context) error {
	var res control.IndexRulesResponse
	online, err := c.call(ctx, http.MethodGet, "/index/rules", nil, &res)
	if err != nil {
		return err
	}
	if !online {
		store, err := c.offline()
		if err != nil {
			return err
		}
		defer store.Close()
		rules, err := store.Rules(ctx)
		if err != nil {
			return err
		}
		res.Rules = make([]index.RuleView, 0, len(rules))
		for _, r := range rules {
			n, err := store.DocumentsUnder(ctx, r.Path)
			if err != nil {
				return err
			}
			res.Rules = append(res.Rules, index.RuleView{Rule: r, Documents: n})
		}
	}
	return c.printRules(res)
}

func (c *indexCLI) printRules(res control.IndexRulesResponse) error {
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Rules) == 0 {
		fmt.Fprintln(c.out, "no index rules")
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tSOURCE\tINCLUDE\tEXCLUDE\tMAX FILE\tDOCUMENTS")
	for _, r := range res.Rules {
		maxFile := "-"
		if r.MaxFileSize > 0 {
			maxFile = humanBytes(r.MaxFileSize)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", r.Path, r.Source, strings.Join(r.Include, ","), strings.Join(r.Exclude, ","), maxFile, r.Documents)
	}
	return w.Flush()
}

func (c *indexCLI) add(ctx context.Context, args []string) error {
	f := parseFlags(args, "json")
	p := f.arg(0)
	if len(f.args) != 1 || !strings.HasPrefix(p, "/") {
		return errors.New("index add: give one absolute virtual path")
	}
	req := control.IndexAddRequest{Path: p}
	if raw := f.str("include", ""); raw != "" {
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimSpace(g); g != "" {
				req.Include = append(req.Include, g)
			}
		}
	}
	if raw := f.str("max-file-size", ""); raw != "" {
		n, err := config.ParseSize(raw)
		if err != nil || n <= 0 {
			return errors.New("index add: --max-file-size takes a size such as 20MiB")
		}
		req.MaxFileSize = int64(n)
	}
	var res control.IndexRulesResponse
	online, err := c.call(ctx, http.MethodPost, "/index/add", req, &res)
	if err != nil {
		return err
	}
	if err := c.needDaemon(online); err != nil {
		return err
	}
	return c.printRules(res)
}

func (c *indexCLI) remove(ctx context.Context, args []string) error {
	f := parseFlags(args, "json", "confirm")
	p := f.arg(0)
	if len(f.args) != 1 || !strings.HasPrefix(p, "/") {
		return errors.New("index rm: give one absolute virtual path")
	}
	if !f.bools["confirm"] {
		return fmt.Errorf("index rm: removing the rule deletes the extracted text and chunks under %s; pass --confirm", p)
	}
	var res control.IndexRulesResponse
	online, err := c.call(ctx, http.MethodPost, "/index/remove", control.IndexRemoveRequest{Path: p, Confirm: true}, &res)
	if err != nil {
		return err
	}
	if err := c.needDaemon(online); err != nil {
		return err
	}
	return c.printRules(res)
}

func (c *indexCLI) rebuild(ctx context.Context, args []string) error {
	f := parseFlags(args, "json", "confirm")
	if len(f.args) != 0 {
		return errors.New("index rebuild: unexpected argument")
	}
	if !f.bools["confirm"] {
		return errors.New("index rebuild: clears the index and downloads every rule-covered file again; pass --confirm")
	}
	var st index.Status
	online, err := c.call(ctx, http.MethodPost, "/index/rebuild", control.IndexRebuildRequest{Confirm: true}, &st)
	if err != nil {
		return err
	}
	if err := c.needDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(st)
	}
	fmt.Fprintln(c.out, "index cleared; the worker rebuilds it from the rules")
	return nil
}

func (c *indexCLI) retry(ctx context.Context, args []string) error {
	f := parseFlags(args, "json")
	p := f.arg(0)
	if len(f.args) > 1 || (p != "" && !strings.HasPrefix(p, "/")) {
		return errors.New("index retry: give at most one absolute virtual path")
	}
	var res control.IndexRetryResponse
	online, err := c.call(ctx, http.MethodPost, "/index/retry", control.IndexRetryRequest{Path: p}, &res)
	if err != nil {
		return err
	}
	if err := c.needDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	fmt.Fprintf(c.out, "requeued %d failed documents\n", res.Requeued)
	return nil
}

func (c *indexCLI) search(ctx context.Context, args []string) error {
	q, err := indexSearchFromArgs(args)
	if err != nil {
		return err
	}
	res, online, err := control.CallIndexSearch(ctx, c.cfg.Control.Socket, c.cfg.Control.Metrics, q)
	if err != nil {
		return err
	}
	if !online {
		store, err := c.offline()
		if err != nil {
			return err
		}
		defer store.Close()
		// Without the daemon there is no live tree to compare versions
		// against: hits keep their indexed path and none is marked stale.
		if res, err = store.Search(ctx, q, nil); err != nil {
			return err
		}
		if !c.asJSON {
			fmt.Fprintln(c.out, "offline: stale marks unavailable")
		}
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Hits) == 0 {
		fmt.Fprintf(c.out, "no hits (%d documents searchable", res.Docs)
		if res.Pending > 0 {
			fmt.Fprintf(c.out, ", %d pending", res.Pending)
		}
		fmt.Fprintln(c.out, ")")
	}
	for _, h := range res.Hits {
		mark := ""
		if h.Stale {
			mark = " [stale]"
		}
		fmt.Fprintf(c.out, "%s%s", h.Path, mark)
		if h.Heading != "" {
			fmt.Fprintf(c.out, "  %s", h.Heading)
		}
		fmt.Fprintf(c.out, "  @%d\n", h.StartOff)
		if h.Snippet != "" {
			fmt.Fprintf(c.out, "    %s\n", strings.ReplaceAll(strings.TrimSpace(h.Snippet), "\n", " "))
		}
	}
	if res.Truncated {
		fmt.Fprintln(c.out, "truncated: narrow the query or the path for more")
	}
	if res.Degraded != "" {
		fmt.Fprintf(c.out, "degraded: %s\n", res.Degraded)
	}
	return nil
}
