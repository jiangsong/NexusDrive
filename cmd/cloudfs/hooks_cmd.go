package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"cloudfs/internal/integration"
)

// `cloudfs hooks install|uninstall|status [--client claude,codex,gemini]`
// registers CloudFS in an agent client's lifecycle hooks
// (docs/agent-first-design.md §7, TODO.md T-54); `cloudfs agent-hook
// prompt|read|stop --client <c>` is what those hooks run. The first is
// the only place cloudfs edits a file it does not own (the client's
// user-level settings), which is why it is marked, idempotent and has an
// uninstall; the second never fails a turn.

func cmdHooks(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var clients []string
	if v := f.str("client", ""); v != "" {
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" {
				clients = append(clients, c)
			}
		}
	}
	switch f.arg(0) {
	case "install":
		// The guard needs the mounts registry before the first mount runs;
		// the configuration says where the mounts will be.
		if cfg, _, err := loadConfig(f); err == nil {
			if err := writeMountsRegistry(home, cfg); err != nil {
				return err
			}
		}
		results, err := hooks.Install(home, clients)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Fprintln(out, "no agent client found (no ~/.claude, ~/.codex, ~/.gemini or ~/.hermes); pass --client to register anyway")
			return nil
		}
		return printHookResults(out, results, f.bool("json"), "installed")
	case "uninstall":
		results, err := hooks.Uninstall(home, clients)
		if err != nil {
			return err
		}
		return printHookResults(out, results, f.bool("json"), "removed")
	case "status", "":
		st := hooks.Statuses(home)
		if f.bool("json") {
			return json.NewEncoder(out).Encode(st)
		}
		for _, s := range st {
			state := "not installed"
			if s.Installed {
				state = "installed"
			}
			if !s.Present {
				state += " (client not found)"
			}
			fmt.Fprintf(out, "%-8s %-14s %s\n", s.Client, state, s.Path)
		}
		mounts, _ := hooks.ReadMounts(hooks.MountsPath(home))
		fmt.Fprintf(out, "mounts registry: %s (%d entries)\n", hooks.MountsPath(home), len(mounts))
		return nil
	}
	return fmt.Errorf("hooks: unknown subcommand %q; use install, uninstall or status", f.arg(0))
}

func printHookResults(out io.Writer, results []hooks.Result, asJSON bool, verb string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(results)
	}
	for _, r := range results {
		state := "already " + verb
		if r.Changed {
			state = verb
		}
		fmt.Fprintf(out, "%-8s %s: %s\n", r.Client, state, r.Path)
		if r.Note != "" {
			fmt.Fprintf(out, "         note: %s\n", r.Note)
		}
	}
	return nil
}

// writeMountsRegistry records the configured mount paths for the guard.
func writeMountsRegistry(home string, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	paths := make([]string, 0, len(cfg.Mounts))
	for _, m := range cfg.Mounts {
		paths = append(paths, config.ExpandHome(m.Path))
	}
	return hooks.WriteMounts(hooks.MountsPath(home), paths)
}

// cmdAgentHook is the hook's runtime. It exits 0 whatever happens: a
// hook must never fail the turn.
func cmdAgentHook(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	f := parseFlags(args)
	client := f.str("client", "claude")
	if f.str("config", "") == "" {
		if home, e := os.UserHomeDir(); e == nil {
			if p := integration.OwnerConfig(home, client); p != "" {
				f.values["config"] = p
			}
		}
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return nil
	}
	api := hooks.NewClient(config.ExpandHome(cfg.Control.Socket), cfg.Control.Metrics)
	switch f.arg(0) {
	case "prompt":
		return hooks.RunPrompt(ctx, in, out, client, api)
	case "read":
		return hooks.RunRead(ctx, in, client, api)
	case "write":
		return hooks.RunWrite(ctx, in, client, api)
	case "stop":
		return hooks.RunStop(ctx, in, client, api)
	}
	return errors.New("agent-hook: use prompt, read, write or stop")
}
