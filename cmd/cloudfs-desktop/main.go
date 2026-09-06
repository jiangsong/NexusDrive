//go:build desktop

// Command cloudfs-desktop is the native window shell for the CloudFS dashboard.
// It is deliberately thin: it does not embed a second copy of the daemon or
// serve the web assets itself. It finds the running daemon (or starts one),
// gives the embedded WebView a loopback URL for the daemon's own control UI,
// and manages the window. Every operation the user performs still goes through
// the same guarded control endpoints a browser would use, so the shell adds no
// privileged path of its own.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	webview "github.com/webview/webview_go"

	"cloudfs/internal/config"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "config file (default ~/.config/cloudfs/config.yaml)")
	flag.Parse()

	// One window per user. If another shell holds the lock, ask it to come
	// forward and exit rather than opening a duplicate.
	inst, held, err := acquireInstance()
	if err != nil || held {
		signalFocus()
		return
	}
	defer inst.release()

	cfg, _ := config.Load(*configPath) // a missing config surfaces below as a launch error

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sh := newShell(*configPath, cfg)
	defer sh.Close()

	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle("CloudFS")
	w.SetSize(1140, 760, webview.HintNone)
	w.SetSize(760, 520, webview.HintMin)
	w.SetHtml(placeholderHTML("正在连接 CloudFS 守护进程…", ""))

	// A second launch pings the focus socket; raise the window when it does.
	inst.onFocus(func() { w.Dispatch(func() { raiseWindow(w) }) })

	// Resolve the daemon off the UI thread; navigate (or show the failure) on it.
	go func() {
		target, err := sh.Resolve(ctx)
		w.Dispatch(func() {
			if err != nil {
				w.SetHtml(placeholderHTML("无法连接守护进程", err.Error()))
				return
			}
			w.Navigate(target)
		})
	}()

	// Ctrl-C in the launching terminal closes the window cleanly.
	go func() {
		<-ctx.Done()
		w.Dispatch(w.Terminate)
	}()

	w.Run()
}

func defaultConfigPath() string {
	if p := os.Getenv("CLOUDFS_CONFIG"); p != "" {
		return p
	}
	return config.ExpandHome("~/.config/cloudfs/config.yaml")
}
