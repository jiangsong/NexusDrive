package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"cloudfs/internal/control"
)

// cmdUI points a browser at the running daemon's web app. The dashboard is
// served on the control TCP endpoint (control.metrics); the Unix socket serves
// the same routes but a browser cannot open a unix:// URL, so this needs the
// TCP endpoint configured. It reuses FetchStatus's endpoint selection to
// confirm a daemon is actually there before opening anything.
func cmdUI(ctx context.Context, args []string) error {
	f := parseFlags(args, "print", "no-open")
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	if !cfg.Control.UI {
		return errors.New("ui: control.ui is disabled in the configuration")
	}
	if cfg.Control.Metrics == "" {
		return errors.New("ui: set control.metrics to a loopback address (e.g. 127.0.0.1:9101) so a browser can reach the dashboard; the Unix socket cannot be opened from a browser")
	}
	// Confirm the daemon is up and owns the endpoint before sending anyone to
	// a dead URL.
	if _, online, err := control.FetchStatus(ctx, cfg.Control.Socket, cfg.Control.Metrics); err != nil {
		return err
	} else if !online {
		return fmt.Errorf("ui: no daemon is answering on %s; start one with `cloudfs mount`", cfg.Control.Metrics)
	}
	url := "http://" + cfg.Control.Metrics + "/"
	fmt.Println(url)
	if f.bool("print") || f.bool("no-open") {
		return nil
	}
	return openBrowser(ctx, url)
}

// openBrowser opens url in the platform's default browser. It is best effort:
// on a headless host it prints the URL (already printed above) and returns
// without treating the missing opener as a failure.
func openBrowser(ctx context.Context, url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	if _, err := exec.LookPath(name); err != nil {
		fmt.Fprintln(os.Stderr, "ui: open the URL above in a browser (no opener on this host)")
		return nil
	}
	if err := exec.CommandContext(ctx, name, append(args, url)...).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ui: could not launch a browser; open the URL above")
	}
	return nil
}
