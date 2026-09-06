package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// cmdPool: cloudfs pool status|create|add|remove|drain|enable|disable|repair|scrub|divergences|join|rebuild.
// Configuration edits go to the file (and tell the daemon's next start);
// the rest asks the running daemon.
func cmdPool(ctx context.Context, args []string) error {
	f := parseFlags(args, "full")
	action := f.arg(0)
	cfg, configPath, err := loadConfig(f)
	if err != nil {
		return err
	}
	socket, tcp := cfg.Control.Socket, cfg.Control.Metrics
	switch action {
	case "status", "":
		var out control.PoolStatusResponse
		online, err := control.CallPool(ctx, socket, tcp, "status", nil, &out)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("pool: the daemon is not running; start it with `cloudfs mount`")
		}
		return printPoolStatus(out)
	case "create":
		// cloudfs pool create <name> --members a,b,c [--replicas 3] [--min-replicas 1] [--mount /mnt/cloud] [--prefix /]
		name := f.arg(1)
		if name == "" || f.str("members", "") == "" {
			return errors.New("pool create: usage: pool create <name> --members a,b,c [--replicas N] [--mount PATH]")
		}
		var members []config.PoolMember
		for _, m := range strings.Split(f.str("members", ""), ",") {
			if m = strings.TrimSpace(m); m != "" {
				members = append(members, config.PoolMember{Remote: m})
			}
		}
		replicas, _ := strconv.Atoi(f.str("replicas", ""))
		minReplicas, _ := strconv.Atoi(f.str("min-replicas", ""))
		if err := config.CreatePool(configPath, name, members, replicas, minReplicas, ""); err != nil {
			return err
		}
		if mp := f.str("mount", ""); mp != "" {
			prefix := f.str("prefix", "")
			if prefix == "" {
				prefix = "/"
			}
			if err := config.AddMount(configPath, mp, prefix, config.Layout{Remote: name, Mode: config.ModeWriteback}); err != nil {
				return err
			}
		}
		fmt.Printf("pool %s written to %s; restart the daemon for it to take effect\n", name, configPath)
		return nil
	case "add":
		// cloudfs pool add <pool> <remote> [--root /dir] [--weight 2] [--capacity 2TiB]
		pool, remote := f.arg(1), f.arg(2)
		if pool == "" || remote == "" {
			return errors.New("pool add: usage: pool add <pool> <remote> [--root /dir] [--weight N] [--capacity SIZE]")
		}
		m := config.PoolMember{Remote: remote, Root: f.str("root", "")}
		if w := f.str("weight", ""); w != "" {
			m.Weight, _ = strconv.ParseFloat(w, 64)
		}
		if c := f.str("capacity", ""); c != "" {
			size, err := config.ParseSize(c)
			if err != nil {
				return err
			}
			m.Capacity = size
		}
		if err := config.AddPoolMember(configPath, pool, m); err != nil {
			return err
		}
		fmt.Printf("%s added to pool %s; restart the daemon for it to take effect\n", remote, pool)
		return nil
	case "remove":
		pool, remote := f.arg(1), f.arg(2)
		if pool == "" || remote == "" {
			return errors.New("pool remove: usage: pool remove <pool> <remote> (drain it first)")
		}
		var out control.PoolMutationResponse
		online, err := control.CallPool(ctx, socket, tcp, "members/remove", control.PoolMemberRequest{Pool: pool, Remote: remote, Confirm: true}, &out)
		if err != nil {
			return err
		}
		if !online {
			if err := config.RemovePoolMember(configPath, pool, remote); err != nil {
				return err
			}
		}
		fmt.Printf("%s removed from pool %s; restart the daemon for it to take effect\n", remote, pool)
		return nil
	case "drain", "enable", "disable":
		pool, remote := f.arg(1), f.arg(2)
		if pool == "" || remote == "" {
			return fmt.Errorf("pool %s: usage: pool %s <pool> <remote>", action, action)
		}
		state := map[string]string{"drain": "draining", "enable": "enabled", "disable": "disabled"}[action]
		var out control.PoolMutationResponse
		online, err := control.CallPool(ctx, socket, tcp, "members/state", control.PoolMemberRequest{Pool: pool, Remote: remote, State: state, Confirm: true}, &out)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("pool: the daemon is not running")
		}
		fmt.Printf("%s is now %s in pool %s\n", remote, state, pool)
		return nil
	case "repair", "scrub", "rebuild":
		pool := f.arg(1)
		var out map[string]any
		online, err := control.CallPool(ctx, socket, tcp, action, control.PoolPathRequest{Pool: pool, Path: f.str("path", ""), Full: f.bool("full"), Confirm: true}, &out)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("pool: the daemon is not running")
		}
		for k, v := range out {
			fmt.Printf("%s: %v\n", k, v)
		}
		return nil
	case "divergences":
		var out struct {
			Divergences []control.PoolDivergenceView `json:"divergences"`
		}
		online, err := control.CallPool(ctx, socket, tcp, "divergences", nil, &out)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("pool: the daemon is not running")
		}
		if len(out.Divergences) == 0 {
			fmt.Println("no divergences")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "POOL\tPATH\tMEMBER\tKIND\tDETAIL")
		for _, d := range out.Divergences {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Pool, d.Path, d.Member, d.Kind, d.Detail)
		}
		return tw.Flush()
	case "join":
		remote := f.arg(1)
		if remote == "" {
			return errors.New("pool join: usage: pool join <remote> [--root /dir]")
		}
		var out map[string]any
		online, err := control.CallPool(ctx, socket, tcp, "join", control.PoolJoinRequest{Remote: remote, Root: f.str("root", ""), Confirm: true}, &out)
		if err != nil {
			return err
		}
		if !online {
			return errors.New("pool join needs the running daemon (it reads the marker through the remote's provider)")
		}
		fmt.Printf("joined pool %v; restart the daemon for it to take effect\n", out["pool"])
		if d, ok := out["detail"].(string); ok && d != "" {
			fmt.Println(d)
		}
		return nil
	}
	return fmt.Errorf("pool: unknown action %q (status, create, add, remove, drain, enable, disable, repair, scrub, divergences, join, rebuild)", action)
}

func printPoolStatus(out control.PoolStatusResponse) error {
	if len(out.Pools) == 0 {
		fmt.Println("no pool is configured")
		if len(out.Candidates) > 0 {
			fmt.Printf("remotes that could become members: %s\n", strings.Join(out.Candidates, ", "))
			fmt.Println("create one with: cloudfs pool create <name> --members a,b,c --mount <path>")
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, p := range out.Pools {
		target := fmt.Sprintf("%d", p.Target)
		if p.TargetCapped {
			target += " (capped: fewer members than replicas)"
		}
		fmt.Fprintf(tw, "pool %s\t%s\n", p.Name, p.PoolID)
		fmt.Fprintf(tw, "  replicas\t%d (min %d, current target %s)\n", p.Replicas, p.MinReplicas, target)
		fmt.Fprintf(tw, "  files\t%d (%d under-replicated, %d unavailable)\n", p.Files, p.UnderReplicated, p.Unavailable)
		fmt.Fprintf(tw, "  repair queue\t%d (%d waiting)\n", p.Repair.Queued, p.Repair.Blocked)
		if p.Total > 0 {
			fmt.Fprintf(tw, "  space\t%s used of %s (%s free)\n", humanBytes(p.Used), humanBytes(p.Total), humanBytes(p.Free))
		}
		fmt.Fprintf(tw, "  holds\t%s\n", humanBytes(p.HoldsBytes))
		if p.Divergences > 0 {
			fmt.Fprintf(tw, "  divergences\t%d (cloudfs pool divergences)\n", p.Divergences)
		}
		for _, n := range p.Notices {
			fmt.Fprintf(tw, "  notice\t%s\n", n)
		}
		fmt.Fprintln(tw, "  MEMBER\tSTATE\tFILES\tSPACE\tPENDING OPS\tLAST ERROR")
		for _, m := range p.Members {
			space := "-"
			if m.Total > 0 {
				space = fmt.Sprintf("%s/%s", humanBytes(m.Used), humanBytes(m.Total))
			}
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%d\t%s\n", m.Remote, m.State, m.Files, space, m.PendingOps, m.LastError)
		}
	}
	return tw.Flush()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
