package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"cloudfs/internal/integration"
)

func cmdAgent(ctx context.Context, args []string, out io.Writer) error {
	f := parseFlags(args, "json")
	for k := range f.values {
		if k != "client" && k != "config" {
			return fmt.Errorf("agent: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		if k != "json" {
			return fmt.Errorf("agent: unknown flag --%s", k)
		}
	}
	if len(f.args) > 1 {
		return errors.New("agent: unexpected argument")
	}
	action := f.arg(0)
	if action == "" {
		action = "status"
	}
	if action != "install" && action != "uninstall" && action != "status" {
		return errors.New("agent: use install, status or uninstall")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	clients := []string{}
	if raw := f.str("client", ""); raw != "" {
		clients = strings.Split(raw, ",")
	} else {
		for _, c := range []string{"codex", "claude"} {
			_, e := exec.LookPath(c)
			_, d := os.Stat(filepath.Join(home, "."+c))
			if action != "install" || e == nil || d == nil {
				clients = append(clients, c)
			}
		}
	}
	seen := map[string]bool{}
	for i, c := range clients {
		c = strings.TrimSpace(c)
		clients[i] = c
		if (c != "codex" && c != "claude") || seen[c] {
			return fmt.Errorf("agent: invalid or repeated client %q", c)
		}
		seen[c] = true
	}
	if f.str("config", "") == "" {
		ownerConfig := ""
		for _, client := range clients {
			p := integration.OwnerConfig(home, client)
			if p == "" {
				continue
			}
			if ownerConfig != "" && p != ownerConfig {
				return fmt.Errorf("agent: selected clients belong to different CloudFS owners (%s and %s)", ownerConfig, p)
			}
			ownerConfig = p
		}
		if ownerConfig != "" {
			f.values["config"] = ownerConfig
		}
	}
	cfg, _, cfgErr := loadConfig(f)
	if action != "status" && cfgErr != nil {
		return cfgErr
	}
	for _, c := range clients {
		switch action {
		case "install":
			if cfg.MCP.HTTP == "" {
				return errors.New("agent install: configure mcp.http on the mount owner first")
			}
			st, e := agent.Open(filepath.Join(cfg.StateDir(), "agent"))
			if e != nil {
				return e
			}
			e = integration.InstallClient(ctx, home, c, version, "", mcpHTTPURL(f, cfg), cfg, st)
			st.Close()
			if e != nil {
				return e
			}
		case "uninstall":
			st, e := agent.Open(filepath.Join(cfg.StateDir(), "agent"))
			if e != nil {
				return e
			}
			e = integration.UninstallClient(ctx, home, c, version, cfg, st)
			st.Close()
			if e != nil {
				return e
			}
		}
	}
	if action == "install" {
		if err = writeMountsRegistry(home, cfg); err != nil {
			return err
		}
	}
	return printAgentStatus(ctx, out, home, clients, cfg, f.bool("json"))
}

func printAgentStatus(ctx context.Context, out io.Writer, home string, clients []string, cfg *config.Config, asJSON bool) error {
	type clientState struct {
		integration.Status
		ClientVersion string `json:"client_version"`
		MCPConnection string `json:"mcp_connection"`
	}
	result := struct {
		Clients     []clientState         `json:"clients"`
		OwnerOnline bool                  `json:"owner_online"`
		Directory   hooks.ContextResponse `json:"directory"`
		Memory      string                `json:"memory"`
		Index       string                `json:"index"`
	}{Memory: "unavailable", Index: "unavailable", Clients: []clientState{}}
	if cfg != nil {
		cwd, _ := os.Getwd()
		short, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		r, e := hooks.NewClient(config.ExpandHome(cfg.Control.Socket), cfg.Control.Metrics).Context(short, hooks.ContextRequest{CWD: cwd, Inspect: true})
		result.OwnerOnline = e == nil
		result.Directory = r
		if e == nil {
			result.Memory = r.Memory
			result.Index = r.Index
		}
	}
	for _, c := range clients {
		s := clientState{Status: integration.Inspect(home, c), ClientVersion: "not found", MCPConnection: "unverified"}
		short, cancel := context.WithTimeout(ctx, time.Second)
		b, e := exec.CommandContext(short, c, "--version").Output()
		cancel()
		if e == nil {
			s.ClientVersion = strings.TrimSpace(string(b))
		}
		if !result.OwnerOnline {
			s.MCPConnection = "owner offline"
		} else if s.MCPConfigured && cfg != nil {
			s.MCPConnection, result.Memory = integration.ProbeClient(ctx, home, c, mcpHTTPURL(parseFlags(nil), cfg))
		}
		result.Clients = append(result.Clients, s)
	}
	if asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	for _, c := range result.Clients {
		fmt.Fprintf(out, "%s (%s): skill=%t MCP configured=%t connection=%s hooks=%t automatic injection=%s\n", c.Client, c.ClientVersion, c.SkillInstalled, c.MCPConfigured, c.MCPConnection, c.HooksInstalled, c.AutoInjection)
		if c.Problem != "" {
			fmt.Fprintln(out, "  "+c.Problem)
		}
	}
	fmt.Fprintf(out, "owner online=%t directory=%s scope=%s memory=%s index=%s\n", result.OwnerOnline, result.Directory.VirtualPath, result.Directory.Scope, result.Memory, result.Index)
	return nil
}
