// Command cloudfs mounts cloud storage as a local directory and serves it to
// agents over MCP. See docs/DESIGN.md for the architecture.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/bench"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/i18n"
	"cloudfs/internal/journal"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/provider"
	"cloudfs/internal/service"
	"cloudfs/internal/strmgen"
	"cloudfs/internal/webdavsrv"

	// Backends register themselves in init.
	_ "cloudfs/internal/pool"
	_ "cloudfs/internal/provider/aliyun"
	_ "cloudfs/internal/provider/baidu"
	_ "cloudfs/internal/provider/box"
	_ "cloudfs/internal/provider/dropbox"
	_ "cloudfs/internal/provider/gdrive"
	_ "cloudfs/internal/provider/onedrive"
	_ "cloudfs/internal/provider/pan115"
	_ "cloudfs/internal/provider/pan123"
	_ "cloudfs/internal/provider/quark"
	_ "cloudfs/internal/provider/s3"
	_ "cloudfs/internal/provider/sftp"
	_ "cloudfs/internal/provider/smb"
	_ "cloudfs/internal/provider/tianyi"
	_ "cloudfs/internal/provider/webdav"
	_ "cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cliLang is the language every message this command prints is rendered in.
// It is read once, from CLOUDFS_LANG or the shell's locale: a run answers in
// the language the shell is set to without a flag to remember. The daemon
// renders its own refusals, so the same value is handed to the control client
// here: settling it in two places is how they came to disagree.
var cliLang = resolveCLILanguage()

func resolveCLILanguage() i18n.Lang {
	lang := i18n.FromEnv()
	control.SetClientLanguage(lang)
	return lang
}

// version is a variable so release builds can inject the tag with
// -ldflags "-X main.version=<tag>". Local builds retain the development
// baseline instead of reporting an empty version.
var version = "0.1.0"

// errRestart signals from cmdMount up to main that the control plane asked for
// a restart. main runs it after cmdMount's defers have released everything.
var errRestart = errors.New("cloudfs: restart requested")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Printf("cloudfs %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	case "providers":
		for _, t := range provider.Types() {
			fmt.Println(t)
		}
	case "setup":
		err = cmdSetup(ctx, os.Args[2:])
	case "config":
		err = cmdConfig(ctx, os.Args[2:])
	case "proxy":
		err = cmdProxy(ctx, os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "mount":
		err = cmdMount(ctx, os.Args[2:])
	case "umount", "unmount":
		err = cmdUmount(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "pool":
		err = cmdPool(ctx, os.Args[2:])
	case "strm":
		err = cmdSTRM(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "uploads":
		err = cmdUploads(ctx, os.Args[2:])
	case "cache":
		err = cmdCache(ctx, os.Args[2:])
	case "pin":
		err = cmdPin(ctx, os.Args[2:])
	case "unpin":
		err = runCache(ctx, "unpin", os.Args[2:], os.Stdout)
	case "warm":
		err = cmdWarm(ctx, os.Args[2:])
	case "find":
		err = cmdFind(ctx, os.Args[2:])
	case "cp", "copy":
		err = runCopy(ctx, os.Args[2:], os.Stdout)
	case "copies":
		err = runCopies(ctx, os.Args[2:], os.Stdout)
	case "export":
		err = runExport(ctx, os.Args[2:], os.Stdout)
	case "exports":
		err = runExports(ctx, os.Args[2:], os.Stdout)
	case "audit":
		err = runAudit(ctx, os.Args[2:], os.Stdout)
	case "sessions":
		err = runSessions(ctx, os.Args[2:], os.Stdout)
	case "index":
		err = runIndex(ctx, os.Args[2:], os.Stdout)
	case "bench":
		err = cmdBench(ctx, os.Args[2:])
	case "ui", "open":
		err = cmdUI(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if errors.Is(err, errRestart) {
		stop() // stop catching signals before we hand the process over
		if xerr := reexecSelf(); xerr != nil {
			fmt.Fprintln(os.Stderr, "cloudfs: restart:", xerr)
			os.Exit(1)
		}
		os.Exit(0) // reached only where reexec spawns a child rather than execing
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloudfs:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: cloudfs <command> [args]

Getting started
  setup [--force] [--listen addr]
                            add or recover drives in the browser, even beside a running daemon

Mounting
  mount [path]              mount the configured remotes and serve until interrupted
  umount <path>             unmount a mount point
  service install|uninstall|status
                            manage the per-user systemd/launchd mount service

Agents
  mcp --stdio               serve MCP over stdin/stdout (for Claude Code, Codex)
  mcp --http [addr]         serve MCP over Streamable HTTP on a loopback address
  mcp install --client claude|codex [--write <file>]
                            print or write the client registration snippet
  mcp token create --name N [--read P,..] [--write P,..] [--read-only] [--ttl 720h]
                            issue a scoped HTTP access token; the token is printed once
  mcp token list            list issued tokens by fingerprint, never the token itself
  mcp token revoke <name|id> --confirm
                            revoke a token and close the sessions using it
  strm <virtual-path> --out <dir>
                            generate media .strm files through WebDAV; --prune removes verified stale outputs
  audit [--session ID] [--tool T] [--result ok|denied|error] [--since 1h] [--limit N] [--json]
                            list recorded MCP tool calls, newest first; reads agent.db when no daemon runs
  sessions list [--state active|finished|expired] | show <id> | finish <id> [--summary text]
                            inspect agent sessions and their scope; finish needs the running daemon
  index status [--path P] [--json]
                            content index health; reads index.db when no daemon runs
  index rules | add <path> [--include g1,g2] [--max-file-size 20MiB] | rm <path> --confirm
                            list, add or remove index rules; changes need the running daemon
  index rebuild --confirm | retry [<path>]
                            clear and rebuild the index, or requeue failed documents
  index search <query> [--path P] [--mode keyword|hybrid] [--limit N] [--json]
                            search extracted file contents

Inspection
  ui [--print]              open the dashboard in a browser (needs control.metrics)
  status [--json]           show cache, upload queue, proxy and remote state
  doctor [--fix] [--json]   diagnose the environment and the local state
  cache stats | gc | pins   inspect or trim the block cache; list pin rules
  uploads list | retry | cancel | resume | drop | flush
	                       drop <id> --confirm removes a stopped local version, not remote data
  find [query] [--ext go,md] [--size >1m] [--after 2026-09-01] [--type dir|file] [--sort name|size|mtime|path] [--all]
                            search indexed file names; --all lists the whole tree first
  cp <source> <destination> copy a file to an absent virtual path
  copies list | show <id>   inspect persistent copy preparations and handoffs
  copies retry|cancel <id>  retry or stop preparation while retaining its content
  copies forget <id> --confirm
                            discard terminal history and unreferenced private content
  export <vpath>... <dir> [--mirror --confirm] [--verify] [--wait] [--json]
                            copy virtual paths onto local storage; resumable, needs a running daemon
  exports list | show | pause | resume | cancel <id>
                            inspect and steer export jobs
  exports forget <id> --confirm
                            drop a finished job and its part files; finished copies are kept
  bench <dir> [--all|--tests a,b] [--cold] [--repeat 3] [--metrics addr] [--label L]
                            run the IO benchmark against any directory
  proxy test <host>         show which outbound a host routes to

Cache control
  pin <path>                download a path in full and keep it cached
  unpin <path>              remove a pin rule without deleting cached bytes
  warm <path> [depth]       pre-list a subtree so lookups are local
  warm --all                crawl every directory the name index does not hold yet

Other
  config check [path]       parse and validate the config file
  config add|auth|list      manage accounts, authorization and private credentials
  providers                 list registered backend types
  version

Global flags
  --config <path>           config file (default ~/.config/cloudfs/config.yaml)
`)
}

// flags is a tiny argument parser: it pulls --key value and --flag out of args
// and returns the rest as positional arguments.
type flags struct {
	values map[string]string
	bools  map[string]bool
	args   []string
}

func parseFlags(args []string, boolFlags ...string) *flags {
	isBool := map[string]bool{}
	for _, b := range boolFlags {
		isBool[b] = true
	}
	f := &flags{values: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			f.args = append(f.args, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			f.values[name[:eq]] = name[eq+1:]
			continue
		}
		if isBool[name] {
			f.bools[name] = true
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			f.values[name] = args[i+1]
			i++
			continue
		}
		f.bools[name] = true
	}
	return f
}

func (f *flags) str(name, def string) string {
	if v, ok := f.values[name]; ok {
		return v
	}
	return def
}

func (f *flags) bool(name string) bool { return f.bools[name] }

func (f *flags) arg(i int) string {
	if i < len(f.args) {
		return f.args[i]
	}
	return ""
}

func defaultConfigPath() string {
	if p := os.Getenv("CLOUDFS_CONFIG"); p != "" {
		return p
	}
	return config.ExpandHome("~/.config/cloudfs/config.yaml")
}

func loadConfig(f *flags) (*config.Config, string, error) {
	path := f.str("config", defaultConfigPath())
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, fmt.Errorf("no config file at %s. Write one first; docs/DESIGN.md section 6 has a complete example", path)
		}
		return nil, path, err
	}
	return cfg, path, nil
}

func cmdConfig(ctx context.Context, args []string) error {
	if parseFlags(args).arg(0) != "check" {
		return runConfig(ctx, args, configIO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	}
	f := parseFlags(args)
	if f.arg(0) != "check" {
		return errors.New("config: expected 'check [path]'")
	}
	path := f.arg(1)
	if path == "" {
		path = f.str("config", defaultConfigPath())
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s\n", path)
	fmt.Printf("  cache %s (max %s, min free %s, block %s)\n",
		cfg.Cache.Dir, cfg.Cache.MaxSize, cfg.Cache.MinFree, cfg.Cache.BlockSize)
	known := map[string]bool{}
	for _, t := range provider.Types() {
		known[t] = true
	}
	names := make([]string, 0, len(cfg.Remotes))
	for n := range cfg.Remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	var unknown []string
	for _, n := range names {
		r := cfg.Remotes[n]
		state := "ok"
		if !known[r.Type] {
			state = "UNKNOWN TYPE"
			unknown = append(unknown, r.Type)
		}
		fmt.Printf("  remote %-12s type=%-10s %s\n", n, r.Type, state)
	}
	for i, m := range cfg.Mounts {
		fmt.Printf("  mount %d at %s\n", i, m.Path)
		prefixes := make([]string, 0, len(m.Layout))
		for p := range m.Layout {
			prefixes = append(prefixes, p)
		}
		sort.Strings(prefixes)
		for _, p := range prefixes {
			l := m.Layout[p]
			mode := l.Mode
			if mode == "" {
				mode = config.ModeWriteback
			}
			fmt.Printf("    %-10s -> %s:%s (%s)\n", p, l.Remote, l.Root, mode)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%d remote(s) use a backend this build does not have: %s",
			len(unknown), strings.Join(unknown, ", "))
	}
	return nil
}

func cmdProxy(ctx context.Context, args []string) error {
	f := parseFlags(args)
	if f.arg(0) != "test" {
		return errors.New("proxy: expected 'test <host>'")
	}
	host := f.arg(1)
	if host == "" {
		return errors.New("proxy test: give a host, for example 'cloudfs proxy test www.googleapis.com'")
	}
	rules := proxy.DefaultRules
	if cfg, _, err := loadConfig(f); err == nil && len(cfg.Proxy.Rules) > 0 {
		rules = cfg.Proxy.Rules
	}
	r, err := proxy.NewRouter(rules)
	if err != nil {
		return err
	}
	tgt := proxy.Target{Host: host}
	if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
		tgt.IP = ips[0]
		fmt.Printf("%s resolves to %s\n", host, ips[0])
	}
	out := r.Outbound(tgt)
	if rule := r.Explain(tgt); rule != nil {
		fmt.Printf("%s -> %s  (matched %s,%s)\n", host, out, rule.Kind, rule.Match)
	} else {
		fmt.Printf("%s -> %s  (no rule matched; default)\n", host, out)
	}
	return nil
}

func cmdDoctor(ctx context.Context, args []string) error {
	f := parseFlags(args, "fix", "json")
	cfg, path, err := loadConfig(f)
	if err != nil {
		// Without a config we can still check the platform.
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
		d := &control.Doctor{FUSESupported: fusefs.Supported, Passthrough: fusefs.PassthroughEnabled}
		return reportChecks(control.LocalizeChecks(d.Run(ctx), cliLang), f.bool("json"))
	}
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version})
	if err != nil {
		return err
	}
	defer d.Close()
	doc := d.Doctor(nil, fusefs.Supported)
	doc.Passthrough = fusefs.PassthroughEnabled
	checks := control.LocalizeChecks(doc.Run(ctx), cliLang)
	if err := reportChecks(checks, f.bool("json")); err != nil {
		return err
	}
	if f.bool("fix") {
		fmt.Println("\n" + i18n.T(cliLang, "cli.applying_fixes"))
		for _, line := range doc.Fix(ctx, cliLang) {
			fmt.Println("  " + line)
		}
	}
	_ = path
	_, _, fail := control.Summary(checks)
	if fail > 0 && !f.bool("fix") {
		return errors.New(i18n.T(cliLang, "cli.checks_failed", fail))
	}
	return nil
}

func reportChecks(checks []control.Check, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(checks)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		mark := "ok  "
		switch c.Level {
		case control.LevelWarn:
			mark = "warn"
		case control.LevelFail:
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "\t\t-> %s\n", c.Fix)
		}
	}
	w.Flush()
	ok, warn, fail := control.Summary(checks)
	fmt.Printf("\n%d ok, %d warnings, %d failures\n", ok, warn, fail)
	return nil
}

func cmdMount(ctx context.Context, args []string) error {
	f := parseFlags(args, "allow-other", "debug", "read-only", "foreground")
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version})
	if err != nil {
		return err
	}
	defer d.Close()

	// A control-plane restart signals here; the wait below acts on it once the
	// mount is up. Buffered and non-blocking so the HTTP handler never blocks.
	restart := make(chan struct{}, 1)

	mountPath := f.arg(0)
	if mountPath == "" {
		mountPath = cfg.Mounts[0].Path
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("cannot create the mount point %s: %w", mountPath, err)
	}
	// The heap of a mounted daemon is small next to the bytes moving
	// through it; a lazier collector spends less of a cold read's CPU on
	// collections and adds less latency to a cache miss.
	debug.SetGCPercent(400)
	if err := fusefs.VerifyMountable(mountPath); err != nil {
		return err
	}
	m, err := fusefs.MountFS(fusefs.MountOptions{
		Options:    fusefs.Options{FS: d.FS},
		Path:       mountPath,
		AllowOther: f.bool("allow-other"),
		Debug:      f.bool("debug"),
		ReadOnly:   f.bool("read-only"),
	})
	if err != nil {
		return err
	}
	// A cold measurement must empty the kernel's caches as well as ours.
	unmounted := false
	defer func() {
		if !unmounted {
			_ = m.Unmount()
		}
	}()
	vfsDrop := d.DropCaches
	d.DropCaches = func(ctx context.Context) (int, error) {
		n, err := vfsDrop(ctx)
		m.DropKernelCaches()
		return n, err
	}
	fmt.Printf("mounted at %s\n", mountPath)
	for _, mnt := range d.FS.Mounts() {
		fmt.Printf("  %-10s -> %s (%s)\n", mnt.Prefix, mnt.Remote, mnt.Mode)
	}

	// Control endpoints, when configured. The desktop shell hands its spawned
	// daemon a loopback TCP address in CLOUDFS_CONTROL_UI so it has a URL a
	// WebView can load; it forces the UI on and overrides the configured TCP
	// endpoint for that one process. Start still refuses a non-loopback
	// address, so the env cannot open the control plane to the network.
	controlSocket, controlTCP, controlUI := cfg.Control.Socket, cfg.Control.Metrics, cfg.Control.UI
	if addr := os.Getenv("CLOUDFS_CONTROL_UI"); addr != "" {
		controlTCP, controlUI = addr, true
	}
	if controlTCP != "" || controlSocket != "" {
		col := d.Collector()
		col.FuseStats = func() control.FuseStatus {
			st := m.OpStats()
			return control.FuseStatus{Ops: st.Ops, ReadBytes: st.ReadBytes, ReadSizes: st.ReadSizes}
		}
		col.Doctor = d.Doctor(col.ConfigView, fusefs.Supported)
		col.Lifecycle = &control.Lifecycle{Restart: func() {
			select {
			case restart <- struct{}{}:
			default:
			}
		}}
		srv := control.NewServer(col)
		if controlUI {
			srv.EnableUI()
		}
		running, err := srv.Start(ctx, controlSocket, controlTCP)
		if err != nil {
			return err
		}
		defer running.Close()
		if controlSocket != "" {
			fmt.Printf("  control socket %s\n", controlSocket)
		}
		if controlTCP != "" {
			fmt.Printf("  metrics on http://%s/metrics\n", controlTCP)
			if controlUI {
				fmt.Printf("  dashboard on http://%s/  (or run: cloudfs ui)\n", controlTCP)
			}
		}
	}
	if cfg.WebDAV.HTTP != "" {
		running, err := startWebDAV(ctx, d, cfg)
		if err != nil {
			return err
		}
		defer running.Close()
		// Say which it is: an endpoint that accepts DELETE is worth naming.
		access := "read-only"
		if cfg.WebDAV.Writable {
			access = "writable"
		}
		fmt.Printf("  webdav on http://%s%s (root %s, %s, %s)\n",
			cfg.WebDAV.HTTP, cfg.WebDAV.Prefix, cfg.WebDAV.Root, access, cfg.WebDAV.Strategy)
	}
	// MCP over HTTP, when configured.
	if addr := cfg.MCP.HTTP; addr != "" {
		go func() {
			if err := serveMCPHTTP(ctx, d, cfg, addr); err != nil {
				fmt.Fprintf(os.Stderr, "mcp server: %v\n", err)
			}
		}()
		fmt.Printf("  mcp on http://%s\n", addr)
	}

	fmt.Println("press ctrl-c to unmount")
	restarting := false
	select {
	case <-ctx.Done():
	case <-restart:
		restarting = true
		fmt.Println("\nrestart requested")
	}
	if !restarting {
		fmt.Println("\nunmounting…")
	} else {
		fmt.Println("detaching mount for restart…")
	}
	if err := m.Unmount(); err != nil {
		return err
	}
	unmounted = true
	if restarting {
		// Return up to main, which re-execs after every deferred close in this
		// function has run — the journal lock and the listeners are released
		// before the successor starts, so the two never coexist.
		return errRestart
	}
	return nil
}

func cmdUmount(args []string) error {
	f := parseFlags(args)
	path := f.arg(0)
	if path == "" {
		return errors.New("umount: give the mount point")
	}
	// fusermount/umount is the reliable way to detach a mount this process
	// does not own.
	if err := service.Unmount(path); err != nil {
		return err
	}
	fmt.Printf("unmounted %s\n", path)
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	// http is an optional-value flag: `--http` selects the configured/default
	// address, while `--http 0.0.0.0:8765` overrides it. Leaving it in the
	// boolean-only set silently turned the address into a positional argument.
	f := parseFlags(args, "stdio", "read-only")
	cfg, _, err := loadConfig(f)
	if err != nil {
		// Installing the client registration does not need a working config.
		if f.arg(0) == "install" {
			var allow []string
			if v := f.str("allow", ""); v != "" {
				allow = strings.Split(v, ",")
			}
			return mcpInstall(f, allow, f.bool("read-only"), nil)
		}
		return err
	}
	allow := cfg.MCP.Allow
	if v := f.str("allow", ""); v != "" {
		allow = strings.Split(v, ",")
	}
	readOnly := cfg.MCP.ReadOnly || f.bool("read-only")

	if f.arg(0) == "install" {
		return mcpInstall(f, allow, readOnly, cfg)
	}
	if f.arg(0) == "token" {
		return runMCPToken(ctx, os.Stdout, mcpTokenArgs(args))
	}
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version})
	if err != nil {
		return err
	}
	defer d.Close()
	// An MCP-only owner needs the same native management surface as a
	// mounted owner. Do not emit control status on the MCP stdio transport.
	if d.Journal != nil && d.Journal.Owner() && (cfg.Control.Socket != "" || cfg.Control.Metrics != "") {
		srv := control.NewServer(d.Collector())
		if cfg.Control.UI {
			srv.EnableUI()
		}
		running, err := srv.Start(ctx, cfg.Control.Socket, cfg.Control.Metrics)
		if err != nil {
			return err
		}
		defer running.Close()
	}
	if cfg.WebDAV.HTTP != "" {
		running, err := startWebDAV(ctx, d, cfg)
		if err != nil {
			return err
		}
		defer running.Close()
	}
	if f.bool("http") || f.str("http", "") != "" {
		addr := f.str("http", cfg.MCP.HTTP)
		if addr == "" {
			addr = "127.0.0.1:8765"
		}
		return serveMCPHTTPWith(ctx, d, allow, readOnly, addr, cfg.MCP.ExportRoots)
	}
	// stdio is the default: it is how Claude Code and Codex launch servers.
	// Beside a running mount this process is not the cache owner: it has
	// its own VFS, does not see the kernel's writes, and shares neither
	// sessions nor the journal. Say so on stderr (stdout is the transport)
	// and let the tools that need the owner refuse with the same advice.
	nonOwner := d.Journal != nil && !d.Journal.Owner()
	if nonOwner {
		fmt.Fprintln(os.Stderr, nonOwnerWarning)
		// Announce the topology to the owner: doctor and the console's
		// connect panel read the heartbeat, since the lock alone cannot say
		// who else is here. Stopped and removed when this server ends.
		if d.Agent != nil {
			defer stdioHeartbeat(ctx, d.Agent.Dir())()
		}
	}
	srv, err := mcpsrv.New(mcpsrv.Options{
		FS: d.FS, Allow: allow, ReadOnly: readOnly, Version: version,
		Export: exportJobsOf(d), ExportRoots: cfg.MCP.ExportRoots,
		Sessions: d.Sessions, NonOwner: nonOwner, Workspace: cfg.MCP.Workspace,
		Index: indexOf(d),
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	err = srv.Run(ctx, &mcp.StdioTransport{})
	// The process was the session: when the client closes the transport
	// the run is over, and the console should say finished rather than
	// active. ctx may already be cancelled, so the update gets its own.
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.FinishStdioSessions(finishCtx)
	return err
}

// stdioHeartbeat writes this process's heartbeat under dir and refreshes it
// every agent.HeartbeatInterval until the returned stop is called or ctx
// ends; stop removes the file. A heartbeat that cannot be written is only
// logged: the server still works, doctor just cannot see it.
func stdioHeartbeat(ctx context.Context, dir string) (stop func()) {
	pid := os.Getpid()
	if err := agent.WriteHeartbeat(dir, pid); err != nil {
		fmt.Fprintf(os.Stderr, "cloudfs: %v\n", err)
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(agent.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = agent.WriteHeartbeat(dir, pid)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
			_ = agent.RemoveHeartbeat(dir, pid)
		})
	}
}

func serveMCPHTTP(ctx context.Context, d *daemon.Daemon, cfg *config.Config, addr string) error {
	return serveMCPHTTPWith(ctx, d, cfg.MCP.Allow, cfg.MCP.ReadOnly, addr, cfg.MCP.ExportRoots)
}

// exportJobsOf keeps a nil manager a nil interface: mcpsrv registers the
// export tools on the strength of that field alone.
func exportJobsOf(d *daemon.Daemon) mcpsrv.ExportJobs {
	if d.Export == nil {
		return nil
	}
	return d.Export
}

// indexOf keeps a nil indexer a nil interface: mcpsrv registers the index
// tools on that alone.
func indexOf(d *daemon.Daemon) mcpsrv.IndexService {
	if d.Index == nil {
		return nil
	}
	return d.Index
}

func serveMCPHTTPWith(ctx context.Context, d *daemon.Daemon, allow []string, readOnly bool, addr string, exportRoots []string) error {
	srv, err := mcpsrv.New(mcpsrv.Options{
		FS: d.FS, Allow: allow, ReadOnly: readOnly, Version: version,
		Export: exportJobsOf(d), ExportRoots: exportRoots,
		Sessions: d.Sessions, Workspace: d.Config.MCP.Workspace,
		Index: indexOf(d),
	})
	if err != nil {
		return err
	}
	// Tell the control plane where the transport is and how to register a
	// client against it; the snippets come from the adapter so the daemon
	// package does not have to import it. Cleared again when serving ends.
	d.SetMCPSnippets(mcpSnippets)
	d.SetMCPHTTP(addr, mcpHTTPToken() != "")
	defer d.SetMCPHTTP("", false)
	// Keep the legacy bearer token out of argv and YAML: argv is commonly
	// visible to other local users, while YAML is deliberately shareable as a
	// deployment template. Issued tokens live hashed in agent.db; until the
	// first one exists a loopback listener stays open to local callers, as it
	// always was. A non-loopback listener (for example inside a container)
	// still fails closed when neither kind of token can be accepted.
	auth := mcpsrv.HTTPAuth{Token: mcpHTTPToken()}
	if d.Agent != nil {
		auth.Verify = d.Agent.VerifyToken
		env := auth.Token
		auth.Open = func(ctx context.Context) bool {
			live, err := d.Agent.HasLiveTokens(ctx)
			return env == "" && err == nil && !live
		}
	}
	return mcpsrv.ServeHTTPWithAuth(ctx, srv, addr, auth)
}

func mcpHTTPToken() string { return os.Getenv("CLOUDFS_MCP_TOKEN") }

// nonOwnerWarning is what a stdio MCP server prints when another process
// owns the cache. The wording matches errRequiresOwner in mcpsrv so a person
// who sees the tool refusal and the startup line recognise the same advice.
const nonOwnerWarning = `cloudfs: another process owns this cache (is "cloudfs mount" running?). This stdio server has its own view; use the HTTP transport: cloudfs mcp install --transport http`

// mcpSnippets renders the HTTP registration snippets and add commands for
// every client, keyed by client name, with the <token> placeholder in the
// header. Injected into the daemon for /mcp/connect and /mcp/tokens.
func mcpSnippets(url string) (map[string]string, map[string]string) {
	snippets, adds := map[string]string{}, map[string]string{}
	for _, client := range []string{"claude", "codex"} {
		o := mcpsrv.ClientOptions{Client: client, Transport: "http", URL: url}
		if snippet, err := mcpsrv.ClientConfigFor(o); err == nil {
			snippets[client] = snippet
		}
		if cmd := mcpsrv.ClientAddCommand(o); cmd != "" {
			adds[client] = cmd
		}
	}
	return snippets, adds
}

// mcpHTTPURL is the URL a client registers for the HTTP transport, from
// --url, the configured mcp.http address or the default one.
func mcpHTTPURL(f *flags, cfg *config.Config) string {
	if u := f.str("url", ""); u != "" {
		return u
	}
	addr := "127.0.0.1:8765"
	if cfg != nil && cfg.MCP.HTTP != "" {
		addr = cfg.MCP.HTTP
	}
	return "http://" + addr + "/"
}

func startWebDAV(ctx context.Context, d *daemon.Daemon, cfg *config.Config) (*webdavsrv.Running, error) {
	return webdavsrv.Start(ctx, webdavsrv.Options{
		FS: d.FS, Addr: cfg.WebDAV.HTTP, Prefix: cfg.WebDAV.Prefix, Root: cfg.WebDAV.Root,
		Token: os.Getenv("CLOUDFS_WEBDAV_TOKEN"), Strategy: cfg.WebDAV.Strategy,
		Writable: cfg.WebDAV.Writable,
	})
}

func cmdSTRM(ctx context.Context, args []string) error {
	f := parseFlags(args, "embed-basic-auth", "prune")
	source := f.arg(0)
	if source == "" || f.str("out", "") == "" {
		return errors.New("strm: usage: cloudfs strm <virtual-path> --out <dir> [--base-url http://127.0.0.1:8080/dav]")
	}
	if !strings.HasPrefix(source, "/") || path.Clean(source) != source || strings.ContainsAny(source, "\x00\\") {
		return errors.New("strm: virtual path must be canonical and absolute")
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	startURL, err := strmStartURL(cfg, source, f.str("base-url", ""))
	if err != nil {
		return err
	}
	depth, err := strconv.Atoi(f.str("depth", "64"))
	if err != nil || depth < 1 || depth > 256 {
		return errors.New("strm: --depth must be an integer from 1 to 256")
	}
	maxFiles, err := strconv.Atoi(f.str("max-files", "100000"))
	if err != nil || maxFiles < 1 || maxFiles > 1000000 {
		return errors.New("strm: --max-files must be an integer from 1 to 1000000")
	}
	var extensions []string
	if raw := f.str("ext", ""); raw != "" {
		extensions = strings.Split(raw, ",")
	}
	stats, err := strmgen.Generate(ctx, strmgen.Options{
		StartURL: startURL, OutputDir: f.str("out", ""), Token: os.Getenv("CLOUDFS_WEBDAV_TOKEN"),
		EmbedBasicAuth: f.bool("embed-basic-auth"), Prune: f.bool("prune"),
		Extensions: extensions, MaxDepth: depth, MaxFiles: maxFiles,
	})
	if err != nil {
		return err
	}
	fmt.Printf("strm: %d media, %d written, %d unchanged, %d pruned, %d modified stale retained, %d non-media skipped, %d directories\n",
		stats.Media, stats.Written, stats.Unchanged, stats.Pruned, stats.Retained, stats.Skipped, stats.Directories)
	if f.bool("embed-basic-auth") {
		fmt.Fprintln(os.Stderr, "warning: generated .strm files contain WebDAV credentials; protect the output directory")
	}
	return nil
}

func strmStartURL(cfg *config.Config, source, override string) (string, error) {
	root := cfg.WebDAV.Root
	if root == "" {
		root = "/"
	}
	if source != root && !strings.HasPrefix(source, strings.TrimSuffix(root, "/")+"/") {
		return "", fmt.Errorf("strm: %s is outside configured webdav.root %s", source, root)
	}
	base := override
	if base == "" {
		if cfg.WebDAV.HTTP == "" {
			return "", errors.New("strm: configure webdav.http or pass --base-url")
		}
		host, port, err := net.SplitHostPort(cfg.WebDAV.HTTP)
		if err != nil {
			return "", fmt.Errorf("strm: invalid webdav.http: %w", err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		base = "http://" + net.JoinHostPort(host, port) + cfg.WebDAV.Prefix
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("strm: invalid base URL: %w", err)
	}
	rel := strings.TrimPrefix(source, root)
	for _, segment := range strings.Split(strings.Trim(rel, "/"), "/") {
		if segment != "" {
			u.Path = strings.TrimSuffix(u.Path, "/") + "/" + segment
		}
	}
	u.RawPath = ""
	return u.String(), nil
}

// mcpInstall prints (or writes) the registration snippet for an agent client,
// so the user does not have to hand-assemble JSON or TOML. --transport http
// registers the daemon's HTTP listener instead of a stdio launch; the token
// comes from --token or, when that is absent, stays the <token> placeholder
// with a hint on stderr so the snippet itself never invents one. cfg may be
// nil when the configuration could not be loaded.
func mcpInstall(f *flags, allow []string, readOnly bool, cfg *config.Config) error {
	return mcpInstallTo(os.Stdout, os.Stderr, f, allow, readOnly, cfg)
}

// mcpInstallTo is mcpInstall with its two streams named: the snippet goes
// to out, the advice to errOut, so a caller that captures stdout gets the
// snippet alone.
func mcpInstallTo(out, errOut io.Writer, f *flags, allow []string, readOnly bool, cfg *config.Config) error {
	client := f.str("client", "")
	if client == "" {
		return errors.New("mcp install: pass --client claude or --client codex")
	}
	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = "cloudfs"
	}
	o := mcpsrv.ClientOptions{Client: client, Binary: binary, Allow: allow, ReadOnly: readOnly, Transport: strings.ToLower(f.str("transport", "stdio"))}
	switch o.Transport {
	case "stdio":
	case "http":
		o.URL, o.Token = mcpHTTPURL(f, cfg), f.str("token", "")
	default:
		return fmt.Errorf("mcp install: unknown transport %q; use stdio or http", o.Transport)
	}
	snippet, err := mcpsrv.ClientConfigFor(o)
	if err != nil {
		return err
	}
	target := f.str("write", "")
	if target == "" {
		fmt.Fprintln(out, snippet)
		switch strings.ToLower(client) {
		case "claude", "claude-code":
			fmt.Fprintln(errOut, "\nAdd this to .mcp.json in your project, or run:")
			if cmd := mcpsrv.ClientAddCommand(o); cmd != "" {
				fmt.Fprintf(errOut, "  %s\n", cmd)
			} else {
				fmt.Fprintf(errOut, "  claude mcp add --transport stdio cloudfs -- %s mcp --stdio\n", binary)
			}
		case "codex":
			fmt.Fprintln(errOut, "\nAdd this to ~/.codex/config.toml")
		}
		if o.Transport == "http" && o.Token == "" {
			fmt.Fprintln(errOut, "\nReplace <token> with an access token; create one with: cloudfs mcp token create --name <client> --read <prefix>")
		}
		return nil
	}
	if err := os.WriteFile(target, []byte(snippet), 0o600); err != nil {
		return fmt.Errorf("mcp install: write %s: %w", target, err)
	}
	fmt.Fprintf(out, "wrote the %s registration to %s\n", client, target)
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	f := parseFlags(args, "json")
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	st, online, err := control.FetchStatusInLanguage(ctx, cfg.Control.Socket, cfg.Control.Metrics, cliLang)
	if err != nil {
		return err
	}
	if !online {
		d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: version, SkipWrite: true})
		if err != nil {
			return err
		}
		defer d.Close()
		col := d.Collector()
		j, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
		if err == nil {
			defer j.Close()
			col.Journal = j
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		st = col.Collect(ctx, cliLang)
		st.Durability = cfg.Journal.Durability
	}
	if f.bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(st)
	return nil
}

func printStatus(st control.Status) {
	fmt.Printf("cloudfs %s\n\n", st.Version)
	fmt.Println("Mounts")
	for _, m := range st.Mounts {
		fmt.Printf("  %-10s %-10s %s\n", m.Prefix, m.Remote, m.Mode)
	}
	fmt.Printf("\nCache\n  %s of %s used across %d blocks, %d hydrated files\n",
		st.Cache.BytesHuman, humanOrDash(st.Cache.MaxBytes), st.Cache.Blocks, st.Cache.HydratedFiles)
	fmt.Printf("  hit ratio %.1f%% (%d hits, %d misses, %d evictions)\n",
		st.Cache.HitRatio*100, st.Cache.Hits, st.Cache.Misses, st.Cache.Evictions)

	fmt.Printf("\nUploads\n  %d pending, %d in flight, %d failed\n",
		st.Uploads.Pending, st.Uploads.Uploading, st.Uploads.Dead)
	if st.Uploads.Cancelled+st.Uploads.Cancelling > 0 {
		fmt.Printf("  %d stopping, %d cancelled; local content retained, remote effects not reconciled\n", st.Uploads.Cancelling, st.Uploads.Cancelled)
	}
	if st.Uploads.Purging > 0 {
		fmt.Printf("  %d cleanup pending; do not retry or infer remote completion\n", st.Uploads.Purging)
	}
	if st.Uploads.OldestAge != "" {
		fmt.Printf("  oldest queued %s\n", st.Uploads.OldestAge)
	}

	fmt.Printf("\nMetadata\n  %d entries, %d directories fully listed, %d negative entries\n",
		st.Meta.Nodes, st.Meta.CompleteDirs, st.Meta.NegativeCache)

	if len(st.Remotes) > 0 {
		fmt.Println("\nRemotes")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  name\tmeta/s\tdown/s\tup/s\tstate")
		for _, r := range st.Remotes {
			state := "ok"
			if r.BreakerOpen {
				state = "PAUSED until " + r.BreakerUntil
			}
			fmt.Fprintf(w, "  %s\t%.1f\t%.1f\t%.1f\t%s\n", r.Remote, r.MetaRate, r.DownRate, r.UpRate, state)
		}
		w.Flush()
	}
	if len(st.Proxies) > 0 {
		fmt.Println("\nProxies")
		for _, p := range st.Proxies {
			state := "healthy"
			if !p.Healthy {
				state = "DOWN: " + p.Error
			}
			fmt.Printf("  %-12s %s (%dms)\n", p.Name, state, p.LatencyMS)
		}
	}
	if len(st.Warnings) > 0 {
		fmt.Println("\nNeeds attention")
		for _, warn := range st.Warnings {
			fmt.Println("  - " + warn)
		}
	}
}

func humanOrDash(b int64) string {
	if b <= 0 {
		return "unlimited"
	}
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

func cmdUploads(ctx context.Context, args []string) error {
	return runUploads(ctx, args, os.Stdout)
}

func cmdCache(ctx context.Context, args []string) error {
	return runCache(ctx, "cache", args, os.Stdout)
}

func cmdPin(ctx context.Context, args []string) error {
	return runCache(ctx, "pin", args, os.Stdout)
}

func cmdWarm(ctx context.Context, args []string) error {
	return runCache(ctx, "warm", args, os.Stdout)
}

func cmdFind(ctx context.Context, args []string) error {
	return runFind(ctx, args, os.Stdout)
}

// cmdBench runs the built-in IO benchmark. Pointed at a cloudfs mount with
// --metrics it also reports what each workload cost the backend; pointed at
// sshfs or a local disk it reports only time, which is what a comparison needs.
func cmdBench(ctx context.Context, args []string) error {
	f := parseFlags(args, "all", "cold", "prepare-only", "no-prepare")
	dir := f.arg(0)
	if dir == "" {
		return errors.New("bench: expected a directory")
	}
	num := func(name string, def int) (int, error) {
		v := f.str(name, "")
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("bench: --%s must be a number, got %q", name, v)
		}
		return n, nil
	}
	repeat, err := num("repeat", 1)
	if err != nil {
		return err
	}
	threads, err := num("threads", 4)
	if err != nil {
		return err
	}
	bigMiB, err := num("big", 256)
	if err != nil {
		return err
	}
	small, err := num("small", 500)
	if err != nil {
		return err
	}
	metaFiles, err := num("meta-files", 10000)
	if err != nil {
		return err
	}
	metrics := f.str("metrics", "")
	if metrics == "" {
		// The daemon's own control address, when a config is at hand.
		if cfg, _, err := loadConfig(f); err == nil && cfg.Control.Metrics != "" {
			metrics = cfg.Control.Metrics
		}
	}
	opt := bench.Options{
		Dir: dir, Label: f.str("label", "cloudfs"), Metrics: metrics,
		Repeat: repeat, Threads: threads, BigMiB: bigMiB, SmallCount: small, MetaFiles: metaFiles,
	}
	if f.bool("cold") {
		if metrics == "" {
			return errors.New("bench: --cold needs --metrics (or a config with control.metrics) to reach the daemon")
		}
		opt.Cold = func(context.Context) error { return bench.DropCaches(metrics) }
	}
	if !f.bool("no-prepare") {
		if err := bench.Prepare(dir, opt); err != nil {
			return fmt.Errorf("bench: prepare dataset: %w", err)
		}
	}
	if f.bool("prepare-only") {
		return nil
	}
	var names []string
	if t := f.str("tests", ""); t != "" && !f.bool("all") {
		names = strings.Split(t, ",")
	}
	results, err := bench.Run(ctx, opt, names)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Failed {
			return fmt.Errorf("bench: %s failed: %s", r.Test, r.Note)
		}
	}
	return nil
}
