package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	netproxy "cloudfs/internal/net/proxy"
	"cloudfs/internal/provider"
)

// `cloudfs setup` is the way in.
//
// Adding a drive means a browser authorization, and the screen that drives one
// lives in the control plane — which only runs inside a daemon, which refuses
// to start without a configuration that already names a mount and a remote.
// So the product's own onboarding was unreachable until someone had written
// the YAML by hand. This command breaks that circle: it writes the smallest
// configuration that can be served, starts the control plane with nothing
// behind it — no FUSE, no journal lock, no providers — and opens the browser.
//
// When the setup screen asks for a restart, the process re-execs as
// `cloudfs mount`. reexecSelf keeps the PID and the control address, so the
// page the person is looking at reconnects to the same origin and finishes.
type setupStage int

const (
	// setupWriteStarter: there is no configuration at all.
	setupWriteStarter setupStage = iota
	// setupServe: a configuration exists but nothing is mountable yet.
	setupServe
	// setupAlreadyUsable: a mount names a remote that exists, so the daemon
	// can serve it and setup has nothing to add.
	setupAlreadyUsable
)

func (s setupStage) String() string {
	switch s {
	case setupWriteStarter:
		return "write-starter"
	case setupServe:
		return "serve"
	default:
		return "already-usable"
	}
}

// stageFor decides what setup should do with the configuration at path. A
// configuration counts as usable only when some mount's layout names a remote
// that really exists: a mount pointing at nothing is what the daemon refuses
// to start on, and handing the person to that refusal is the failure this
// command exists to prevent.
func stageFor(path string) setupStage {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return setupWriteStarter
		}
		// A file that exists but does not parse is not something to overwrite.
		// Serving the setup screen is wrong too, but the caller reports the
		// load error itself; this only says setup has work to do.
		return setupServe
	}
	for _, m := range cfg.Mounts {
		for _, l := range m.Layout {
			if _, ok := cfg.Remotes[l.Remote]; ok {
				return setupAlreadyUsable
			}
		}
	}
	return setupServe
}

// setupRestartArgv is the command line the wizard re-execs into once the
// person presses finish.
//
// It names the configuration explicitly. The wizard is usually pointed at a
// file that is not the default one — that is most of what it is for — and
// `cloudfs mount` with no --config resolves the default path, so leaving the
// flag out mounts a different file from the one just configured, with no error
// anywhere to say so.
func setupRestartArgv(current []string, configPath string) []string {
	executable := "cloudfs"
	if len(current) > 0 && current[0] != "" {
		executable = current[0]
	}
	return []string{executable, "mount", "--config", configPath}
}

func setupControlEndpoints(cfg *config.Config, listenOverride string) (socket, tcp string) {
	if listenOverride != "" {
		// An explicit recovery address runs beside the configured daemon, whose
		// Unix socket may already be owned by the live process.
		return "", listenOverride
	}
	return cfg.Control.Socket, cfg.Control.Metrics
}

func cmdSetup(ctx context.Context, args []string) error {
	f := parseFlags(args)
	path := f.str("config", defaultConfigPath())
	if err := migrateLegacyLayout(path, os.Stdout); err != nil {
		return err
	}

	switch stageFor(path) {
	case setupWriteStarter:
		if err := config.WriteStarter(path); err != nil {
			return err
		}
		fmt.Printf("wrote a starter configuration at %s\n", path)
	case setupAlreadyUsable:
		if !f.bools["force"] {
			fmt.Printf("%s already describes a mountable filesystem; run cloudfs to mount it\n", path)
			return nil
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	proxyManager, err := netproxy.NewManager(control.ProxyManagerOptions(cfg.Proxy))
	if err != nil {
		return fmt.Errorf("setup: proxy configuration: %w", err)
	}
	proxyManager.StartHealthChecks(ctx)
	defer proxyManager.Stop()

	controlSocket, controlTCP := setupControlEndpoints(cfg, f.str("listen", ""))
	if controlTCP == "" {
		return errors.New("setup: the configuration names no control address; set control.metrics")
	}

	restart := make(chan struct{}, 1)
	col := &control.Collector{
		Config:  cfg,
		Version: version,
		Started: time.Now(),
		Proxy:   proxyManager,
		ReloadProxy: func(p config.Proxy) error {
			return proxyManager.Reload(control.ProxyManagerOptions(p))
		},

		Lifecycle: &control.Lifecycle{
			Restart: func() {
				select {
				case restart <- struct{}{}:
				default:
				}
			},
		},
	}
	// Every hook reads the configuration through the collector's published
	// view. This flow adds accounts one after another and republishes after
	// each, so a hook holding the pointer this function loaded would not see
	// the drive that was just added — the next thing it is asked about.
	view := func() *config.Config { return col.ConfigView() }
	col.Auth = daemon.AuthStarterFor(view)
	col.CheckAccount = func(ctx context.Context, name string) error {
		return daemon.SanitizeAccountError(daemon.CheckAccount(ctx, view(), name))
	}
	col.AccountQuota = func(ctx context.Context, name string) (provider.Quota, bool, error) {
		return daemon.AccountQuota(ctx, view(), name)
	}

	srv := control.NewServer(col)
	srv.EnableUI()

	running, err := srv.Start(ctx, controlSocket, controlTCP)
	if err != nil {
		return err
	}
	defer running.Close()

	// The wizard's own route, not the console root: the root lands on the
	// connections screen, which offers the wizard only while it lists no
	// drive, so someone who added their first drive from that screen's own
	// button was out of the flow with three steps still to go.
	url := "http://" + controlTCP + "/#/setup"
	fmt.Printf("setup is open at %s\n", url)
	fmt.Println("  add your drives there, then let it restart the daemon to mount them")
	if !f.bools["no-open"] {
		_ = openBrowser(ctx, url)
	}

	select {
	case <-restart:
		// Come back as a real daemon serving what was just configured.
		os.Args = setupRestartArgv(os.Args, path)
		return errRestart
	case <-ctx.Done():
		return nil
	}
}
