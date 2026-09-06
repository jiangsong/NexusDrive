//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// shell resolves and owns the daemon the WebView talks to. It never hands the
// WebView a unix-socket URL — a WebView cannot open one — so a socket-only
// daemon is reached through a small loopback reverse proxy this process runs.
type shell struct {
	configPath string
	cfg        *config.Config
	proxy      *http.Server
	daemon     *exec.Cmd // set only when this shell launched the daemon
}

func newShell(configPath string, cfg *config.Config) *shell {
	return &shell{configPath: configPath, cfg: cfg}
}

// Resolve returns a loopback HTTP URL for the WebView, bringing a daemon up if
// none is running.
func (s *shell) Resolve(ctx context.Context) (string, error) {
	if s.cfg != nil {
		if _, online, err := control.FetchStatus(ctx, s.cfg.Control.Socket, s.cfg.Control.Metrics); err == nil && online {
			// A daemon already owns the journal; a second cannot start, so
			// attach to this one rather than launch.
			if s.cfg.Control.UI && s.cfg.Control.Metrics != "" {
				return "http://" + s.cfg.Control.Metrics + "/", nil
			}
			return s.attach(ctx)
		}
	}
	return s.launch(ctx)
}

// attach serves the running daemon's UI to the WebView over a loopback TCP
// front door that reverse-proxies to its control socket (or TCP). The incoming
// Host is preserved so the daemon's same-origin guard still sees a loopback
// origin.
func (s *shell) attach(ctx context.Context) (string, error) {
	dial, where := s.targetDial()
	transport := &http.Transport{DialContext: dial}
	if err := probeUI(ctx, transport); err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			// Keep the loopback Host the browser sent: the control guard
			// checks that Origin matches Host, and rewriting it to the
			// daemon's own name would fail every mutation.
			r.Out.URL.Host = r.In.Host
			r.Out.Host = r.In.Host
		},
		Transport: transport,
	}
	s.proxy = &http.Server{Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	go s.proxy.Serve(ln)
	_ = where
	return "http://" + ln.Addr().String() + "/", nil
}

// targetDial dials the running daemon's control interface, preferring its unix
// socket. The returned dialer ignores the address it is handed and always
// reaches the one daemon this shell attached to.
func (s *shell) targetDial() (func(context.Context, string, string) (net.Conn, error), string) {
	if s.cfg != nil && s.cfg.Control.Socket != "" {
		sock := s.cfg.Control.Socket
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}, "socket"
	}
	tcp := s.cfg.Control.Metrics
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", tcp)
	}, "tcp"
}

// probeUI confirms the daemon actually serves the web app before the WebView is
// pointed at it. A running daemon with control.ui off answers /status but 404s
// on /, which would show the user a blank error page instead of the app.
func probeUI(ctx context.Context, transport http.RoundTripper) error {
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://cloudfs/", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return errors.New("a daemon is running but its dashboard is off; set `control.ui: true` (and a loopback `control.metrics`) in the config and restart it")
}

// launch starts a fresh daemon with a loopback TCP dashboard on a free port,
// handed to it in CLOUDFS_CONTROL_UI, and waits for it to answer.
func (s *shell) launch(ctx context.Context) (string, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return "", err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	bin, err := cloudfsBinary()
	if err != nil {
		return "", fmt.Errorf("cannot find the cloudfs binary next to this shell or on PATH: %w", err)
	}
	args := []string{"mount"}
	if s.configPath != "" {
		args = append(args, "--config", s.configPath)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CLOUDFS_CONTROL_UI="+addr)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	s.daemon = cmd
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, online, _ := control.FetchStatus(ctx, "", addr); online {
			return "http://" + addr + "/", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return "", errors.New("the daemon did not come up in time; check its output in this terminal")
}

// Close shuts down the front door and, if this shell launched the daemon, takes
// it down too. A daemon this shell only attached to is left running.
func (s *shell) Close() {
	if s.proxy != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.proxy.Shutdown(ctx)
		cancel()
	}
	if s.daemon != nil && s.daemon.Process != nil {
		_ = s.daemon.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = s.daemon.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = s.daemon.Process.Kill()
		}
	}
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func cloudfsBinary() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "cloudfs")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return exec.LookPath("cloudfs")
}
