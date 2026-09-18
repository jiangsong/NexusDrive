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
	"strings"
	"text/tabwriter"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// runTriggers is `cloudfs triggers <action>`. list reads the rules from
// the daemon or, offline, from the configuration file; deliveries and show
// read agent.db read-only when no daemon runs; test and retry change queue
// state only the engine may touch, so they need the daemon.
func runTriggers(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json", "confirm")
	for k := range f.values {
		switch k {
		case "config", "timeout", "limit", "cursor", "rule", "state":
		default:
			return fmt.Errorf("triggers: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" && k != "confirm" {
			return fmt.Errorf("triggers: --%s requires a value or is unknown", k)
		}
	}
	action := f.arg(0)
	switch action {
	case "":
		return errors.New("triggers: usage: cloudfs triggers list | deliveries [--rule R] [--state S] | show <id> | test <rule> <path> --confirm | retry <id>")
	case "list", "deliveries":
		if len(f.args) > 1 {
			return fmt.Errorf("triggers %s: unexpected argument", action)
		}
	case "show", "retry":
		if len(f.args) != 2 {
			return fmt.Errorf("triggers %s: give one delivery id", action)
		}
	case "test":
		if len(f.args) != 3 {
			return errors.New("triggers test: give the rule name and an absolute virtual path")
		}
	default:
		return fmt.Errorf("triggers: unknown action %q", action)
	}
	q := agent.DeliveryQuery{Cursor: f.str("cursor", ""), Rule: f.str("rule", ""), State: f.str("state", "")}
	switch q.State {
	case "", agent.DeliveryPending, agent.DeliveryRunning, agent.DeliveryDone, agent.DeliveryDead:
	default:
		return errors.New("triggers: --state must be pending, running, done or dead")
	}
	if raw := f.str("limit", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			return errors.New("triggers: --limit must be between 1 and 500")
		}
		q.Limit = n
	}
	var id int64
	if action == "show" || action == "retry" {
		n, err := strconv.ParseInt(f.arg(1), 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("triggers %s: the delivery id is a number from `cloudfs triggers deliveries`", action)
		}
		id = n
	}
	if action == "test" {
		if !strings.HasPrefix(f.arg(2), "/") {
			return errors.New("triggers test: the path must be an absolute virtual path such as /work/inbox/a.txt")
		}
		if !f.bools["confirm"] {
			return fmt.Errorf("triggers test: this runs the action of rule %s for real (its command executes or its webhook is sent); add --confirm", f.arg(1))
		}
	}
	timeout, err := time.ParseDuration(f.str("timeout", "30s"))
	if err != nil || timeout <= 0 {
		return errors.New("triggers: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	c := triggersCLI{cfg: cfg, out: out, asJSON: f.bools["json"]}
	switch action {
	case "list":
		return c.list(ctx)
	case "deliveries":
		return c.deliveries(ctx, q)
	case "show":
		return c.show(ctx, id)
	case "retry":
		return c.retry(ctx, id)
	default:
		return c.test(ctx, f.arg(1), f.arg(2))
	}
}

type triggersCLI struct {
	cfg    *config.Config
	out    io.Writer
	asJSON bool
}

func (c *triggersCLI) socket() (string, string) { return c.cfg.Control.Socket, c.cfg.Control.Metrics }

// offline opens agent.db read-only for the delivery table. os.ErrNotExist
// (wrapped) means no daemon ever wrote one.
func (c *triggersCLI) offline() (*agent.Store, error) {
	return agent.OpenReadOnly(filepath.Join(c.cfg.StateDir(), "agent"))
}

func needTriggerDaemon() error {
	return errors.New("triggers: this action requires the running daemon; start `cloudfs mount` first")
}

func (c *triggersCLI) list(ctx context.Context) error {
	socket, tcp := c.socket()
	res, online, err := control.CallTriggers(ctx, socket, tcp)
	if err != nil {
		return err
	}
	if !online {
		// The file is the source of truth either way; what the daemon adds
		// is whether the rules are actually running.
		res = control.TriggersResponseOf(false, c.cfg.Triggers, c.cfg.Agents)
		if !c.asJSON {
			fmt.Fprintln(c.out, "offline: rules read from the configuration file; whether they run needs the running daemon")
		}
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Rules) == 0 && len(res.Agents) == 0 {
		fmt.Fprintln(c.out, "no trigger rules or agents configured (triggers: / agents: in the configuration file)")
		return nil
	}
	if online && !res.Enabled {
		fmt.Fprintln(c.out, "the daemon runs no trigger engine (not the owner of agent.db, or no rules when it started)")
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATHS\tEVENTS\tORIGINS\tDEBOUNCE\tACTION")
	for _, r := range res.Rules {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, strings.Join(r.Paths, ","), strings.Join(r.Events, ","), strings.Join(r.Origins, ","), r.Debounce, actionSummary(r.Action))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(res.Agents) > 0 {
		names := make([]string, 0, len(res.Agents))
		for _, a := range res.Agents {
			names = append(names, a.Name)
		}
		fmt.Fprintf(c.out, "agents: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// actionSummary is one cell: the argv quoted element by element, so a
// space inside an argument is visible and nothing reads as a shell line.
func actionSummary(a control.TriggerActionView) string {
	switch a.Type {
	case "exec":
		parts := make([]string, 0, len(a.Exec.Command))
		for _, arg := range a.Exec.Command {
			parts = append(parts, strconv.Quote(arg))
		}
		return "exec " + strings.Join(parts, " ")
	case "webhook":
		s := "webhook " + a.Webhook.URL
		if a.Webhook.SecretConfigured {
			s += " (signed)"
		}
		return s
	}
	return a.Type
}

func (c *triggersCLI) deliveries(ctx context.Context, q agent.DeliveryQuery) error {
	socket, tcp := c.socket()
	res, online, err := control.CallDeliveries(ctx, socket, tcp, q)
	if err != nil {
		return err
	}
	if !online {
		st, err := c.offline()
		if errors.Is(err, os.ErrNotExist) {
			return c.printDeliveries(control.DeliveriesResponse{Deliveries: []agent.Delivery{}})
		}
		if err != nil {
			return err
		}
		defer st.Close()
		rows, next, err := st.Deliveries().List(ctx, q)
		if err != nil {
			return err
		}
		for i := range rows {
			rows[i].Output = ""
		}
		res = control.DeliveriesResponse{Deliveries: rows, NextCursor: next}
		if !c.asJSON {
			fmt.Fprintln(c.out, "offline: read from agent.db")
		}
	}
	return c.printDeliveries(res)
}

func (c *triggersCLI) printDeliveries(res control.DeliveriesResponse) error {
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	if len(res.Deliveries) == 0 {
		fmt.Fprintln(c.out, "no deliveries")
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTIME\tRULE\tPATH\tKIND\tORIGIN\tATTEMPTS\tSTATE\tERROR")
	for _, d := range res.Deliveries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.FirstSeen.Local().Format("2006-01-02 15:04:05"), d.Rule, d.Path, d.Kind, d.Origin, d.Attempts, d.State, oneLine(d.LastError))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if res.NextCursor != "" {
		fmt.Fprintf(c.out, "more deliveries: use triggers deliveries --cursor %s\n", res.NextCursor)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func (c *triggersCLI) show(ctx context.Context, id int64) error {
	socket, tcp := c.socket()
	d, online, err := control.CallDelivery(ctx, socket, tcp, id)
	if err != nil {
		return err
	}
	if !online {
		st, err := c.offline()
		if err != nil {
			return err
		}
		defer st.Close()
		if d, err = st.Deliveries().Get(ctx, id); err != nil {
			return err
		}
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(d)
	}
	fmt.Fprintf(c.out, "delivery %d\n  rule: %s\n  path: %s\n  kind: %s\n  origin: %s\n  state: %s\n  attempts: %d\n  first seen: %s\n  due: %s\n",
		d.ID, d.Rule, d.Path, d.Kind, d.Origin, d.State, d.Attempts, d.FirstSeen.Local().Format(time.RFC3339), d.DueAt.Local().Format(time.RFC3339))
	if !d.DoneAt.IsZero() {
		fmt.Fprintf(c.out, "  finished: %s\n", d.DoneAt.Local().Format(time.RFC3339))
	}
	if d.LastError != "" {
		fmt.Fprintf(c.out, "  error: %s\n", d.LastError)
	}
	if d.Output != "" {
		fmt.Fprintln(c.out, "output:")
		// The output is the action's own bytes; it is printed as it came,
		// indented so it cannot be mistaken for the fields above.
		for _, line := range strings.Split(strings.TrimRight(d.Output, "\n"), "\n") {
			fmt.Fprintf(c.out, "  %s\n", line)
		}
		if d.Truncated {
			fmt.Fprintln(c.out, "  (output was cut at the runner's size cap)")
		}
	}
	return nil
}

func (c *triggersCLI) retry(ctx context.Context, id int64) error {
	socket, tcp := c.socket()
	res, online, err := control.CallTriggerRetry(ctx, socket, tcp, id)
	if err != nil {
		return err
	}
	if !online {
		return needTriggerDaemon()
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	fmt.Fprintf(c.out, "delivery %d: pending again\n", res.ID)
	return nil
}

func (c *triggersCLI) test(ctx context.Context, name, path string) error {
	socket, tcp := c.socket()
	res, online, err := control.CallTriggerTest(ctx, socket, tcp, name, path, true)
	if err != nil {
		return err
	}
	if !online {
		return needTriggerDaemon()
	}
	if c.asJSON {
		return json.NewEncoder(c.out).Encode(res)
	}
	fmt.Fprintf(c.out, "queued delivery %d\n", res.ID)
	return nil
}
