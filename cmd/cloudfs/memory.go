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
	"text/tabwriter"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/memory"
)

// runMemory is `cloudfs memory <action>`: the terminal's view of the agent
// memory store, through the same routes the console uses and the same
// store the memory_* MCP tools write. Every action needs the running
// daemon, whose VFS the facts live in. --agent defaults to shared, the
// area every agent may read, which is what a person curating memory from
// a terminal most often means; an agent's own directory is named
// explicitly.
func runMemory(ctx context.Context, args []string, out io.Writer) error {
	return runMemoryWith(ctx, args, os.Stdin, out)
}

// runMemoryWith is runMemory with the standard input named, for `memory
// put`, which reads the body from it when there is no --file.
func runMemoryWith(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	f := parseFlags(args, "json", "confirm", "append", "no-shared")
	for k := range f.values {
		switch k {
		case "config", "timeout", "agent", "cursor", "limit", "file", "description", "type", "expected-version", "mode":
		default:
			return fmt.Errorf("memory: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		switch k {
		case "json", "confirm", "append", "no-shared":
		default:
			return fmt.Errorf("memory: --%s requires a value or is unknown", k)
		}
	}
	action := f.arg(0)
	if action == "" {
		return errors.New("memory: usage: cloudfs memory agents|list|get|put|delete|search ...")
	}
	agent := f.str("agent", memory.SharedAgent)
	if !memory.ValidName(agent) {
		return fmt.Errorf("memory: --agent %q: %v", agent, memory.ErrBadName)
	}
	name := f.arg(1)
	switch action {
	case "agents", "list":
		if len(f.args) > 1 {
			return fmt.Errorf("memory %s: unexpected argument", action)
		}
	case "get", "put", "delete":
		if len(f.args) != 2 {
			return fmt.Errorf("memory %s: give one fact name", action)
		}
		if !memory.ValidName(name) {
			return fmt.Errorf("memory %s: name %q: %v", action, name, memory.ErrBadName)
		}
	case "search":
		if len(f.args) < 2 {
			return errors.New("memory search: give a query")
		}
	default:
		return fmt.Errorf("memory: unknown action %q", action)
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("memory: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	c := memoryCLI{cfg: cfg, in: in, out: out, asJSON: f.bools["json"], agent: agent}
	switch action {
	case "agents":
		return c.agents(ctx)
	case "list":
		return c.list(ctx, f)
	case "get":
		return c.get(ctx, name)
	case "put":
		return c.put(ctx, name, f)
	case "delete":
		return c.delete(ctx, name, f.bools["confirm"])
	}
	return c.search(ctx, strings.TrimSpace(strings.Join(f.args[1:], " ")), f)
}

type memoryCLI struct {
	cfg    *config.Config
	in     io.Reader
	out    io.Writer
	asJSON bool
	agent  string
}

func (c *memoryCLI) endpoints() (socket, tcp string) {
	return c.cfg.Control.Socket, c.cfg.Control.Metrics
}

func needMemoryDaemon(online bool) error {
	if online {
		return nil
	}
	return errors.New("memory: this command requires the running daemon; start `cloudfs mount` first")
}

func (c *memoryCLI) agents(ctx context.Context) error {
	socket, tcp := c.endpoints()
	res, online, err := control.CallMemoryAgents(ctx, socket, tcp)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if !res.Enabled {
		if res.Reason == "no_root" {
			fmt.Fprintf(c.out, "memory.root is not configured; add to the configuration file:\n%s\n", res.Example)
			return nil
		}
		fmt.Fprintln(c.out, "the agent memory store is not available in this daemon")
		return nil
	}
	fmt.Fprintf(c.out, "root: %s (max %s per fact, %s per agent)\n", res.Root, humanBytes(res.MaxFactBytes), humanBytes(res.MaxAgentBytes))
	if len(res.Agents) == 0 {
		fmt.Fprintln(c.out, "no agent has written memory yet")
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tFACTS\tBYTES\tCONFLICTS")
	for _, a := range res.Agents {
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\n", a.Name, a.Facts, humanBytes(a.Bytes), a.Conflicts)
	}
	return w.Flush()
}

func (c *memoryCLI) list(ctx context.Context, f *flags) error {
	limit := 0
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return errors.New("memory list: --limit must be between 1 and 1000")
		}
		limit = n
	}
	socket, tcp := c.endpoints()
	res, online, err := control.CallMemoryList(ctx, socket, tcp, c.agent, f.str("cursor", ""), limit)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Facts) == 0 {
		fmt.Fprintf(c.out, "%s: no facts\n", c.agent)
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSIZE\tUPDATED\tCONFLICTS\tDESCRIPTION")
	for _, m := range res.Facts {
		updated := "-"
		if !m.Meta.UpdatedAt.IsZero() {
			updated = m.Meta.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", m.Name, orDash(m.Meta.Type), humanBytes(m.Size), updated, len(m.Conflicts), m.Meta.Description)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if res.NextCursor != "" {
		fmt.Fprintf(c.out, "more facts: use memory list --cursor %s\n", res.NextCursor)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// get prints the body verbatim so it can be piped into an editor or
// another command; --json carries the version and the conflict copies.
func (c *memoryCLI) get(ctx context.Context, name string) error {
	socket, tcp := c.endpoints()
	fact, online, err := control.CallMemoryGet(ctx, socket, tcp, c.agent, name)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(fact)
	}
	_, err = io.WriteString(c.out, fact.Content)
	return err
}

func (c *memoryCLI) put(ctx context.Context, name string, f *flags) error {
	q := control.MemoryPutRequest{Mode: f.str("mode", ""), ExpectedVersion: f.str("expected-version", ""), Description: f.str("description", ""), Type: f.str("type", "")}
	if f.bools["append"] {
		if q.Mode != "" && q.Mode != "append" {
			return errors.New("memory put: --append contradicts --mode")
		}
		q.Mode = "append"
	}
	switch q.Mode {
	case "", "replace", "append":
	default:
		return errors.New("memory put: --mode must be replace or append")
	}
	src := c.in
	if file := f.str("file", ""); file != "" {
		fh, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("memory put: %w", err)
		}
		defer fh.Close()
		src = fh
	}
	// One byte over the largest configurable fact is enough to refuse
	// locally rather than stream a file of any size to the daemon.
	body, err := io.ReadAll(io.LimitReader(src, int64(config.DefaultMemoryMaxAgentBytes)+1))
	if err != nil {
		return fmt.Errorf("memory put: %w", err)
	}
	if len(body) > int(config.DefaultMemoryMaxAgentBytes) {
		return fmt.Errorf("memory put: the body exceeds %s; a memory is a note, not a document", humanBytes(int64(config.DefaultMemoryMaxAgentBytes)))
	}
	q.Content = string(body)
	socket, tcp := c.endpoints()
	fact, online, err := control.CallMemoryPut(ctx, socket, tcp, c.agent, name, q)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(fact)
	}
	fmt.Fprintf(c.out, "wrote %s (%s, version %s)\n", fact.Path, humanBytes(fact.Size), fact.Version)
	if len(fact.Conflicts) > 0 {
		fmt.Fprintf(c.out, "%d conflict copies beside it:\n", len(fact.Conflicts))
		for _, p := range fact.Conflicts {
			fmt.Fprintf(c.out, "  %s\n", p)
		}
	}
	return nil
}

func (c *memoryCLI) delete(ctx context.Context, name string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("memory delete: removes %s/%s and its MEMORY.md line, on the remote too; pass --confirm", c.agent, name)
	}
	socket, tcp := c.endpoints()
	res, online, err := control.CallMemoryDelete(ctx, socket, tcp, c.agent, name, true)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	fmt.Fprintf(c.out, "deleted %s\n", res.Path)
	return nil
}

func (c *memoryCLI) search(ctx context.Context, query string, f *flags) error {
	if query == "" {
		return errors.New("memory search: give a query")
	}
	opt := memory.SearchOptions{Query: query, Agent: c.agent, IncludeShared: !f.bools["no-shared"], Mode: f.str("mode", "")}
	switch opt.Mode {
	case "", "keyword", "hybrid", "vector":
	default:
		return errors.New("memory search: --mode must be keyword, hybrid or vector")
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return errors.New("memory search: --limit must be between 1 and 100")
		}
		opt.TopK = n
	}
	socket, tcp := c.endpoints()
	res, online, err := control.CallMemorySearch(ctx, socket, tcp, opt)
	if err != nil {
		return err
	}
	if err := needMemoryDaemon(online); err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Hits) == 0 {
		fmt.Fprintf(c.out, "no hits (%s mode", res.ModeUsed)
		if res.Pending > 0 {
			fmt.Fprintf(c.out, ", %d documents pending", res.Pending)
		}
		fmt.Fprintln(c.out, ")")
	}
	for _, h := range res.Hits {
		where := h.Path
		if h.Name != "" {
			where = h.Agent + "/" + h.Name
		}
		fmt.Fprintf(c.out, "%s", where)
		if h.Heading != "" {
			fmt.Fprintf(c.out, "  %s", h.Heading)
		}
		fmt.Fprintln(c.out)
		if h.Snippet != "" {
			fmt.Fprintf(c.out, "    %s\n", strings.ReplaceAll(strings.TrimSpace(h.Snippet), "\n", " "))
		}
	}
	if res.Degraded != "" {
		fmt.Fprintf(c.out, "degraded to %s: %s\n", res.ModeUsed, res.Degraded)
	}
	if res.Truncated {
		fmt.Fprintln(c.out, "more hits than shown; raise --limit or narrow the query")
	}
	return nil
}
