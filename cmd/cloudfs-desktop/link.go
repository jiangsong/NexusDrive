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
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// shell resolves and owns the daemon the WebView talks to. It never hands the
// WebView a unix-socket URL — a WebView cannot open one — so a socket-only
// daemon is reached through a small loopback reverse proxy this process runs.
//
// mu guards the fields Resolve writes from a background goroutine and Close
// reads from the main goroutine: without it, closing the window while a slow
// launch is still in flight could read daemon==nil, miss it, and leave the
// just-started daemon orphaned.
type shell struct {
	mu         sync.Mutex
	configPath string
	cfg        *config.Config
	proxy      *http.Server
	daemon     *exec.Cmd     // set only when this shell launched the daemon
	exited     chan struct{} // closed once cmd.Wait() has returned
	exitErr    error         // what it returned; read under mu, after exited
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
//
// Note the threat-model shift this makes on a shared host: the daemon's control
// plane, otherwise reachable only over a 0700 unix socket, is now also on a
// loopback TCP port. privateRequest still blocks browser-driven cross-site
// requests, but it cannot stop another local process (not bound by same-origin)
// from forging the Host/Origin headers. The port is random and lives only for
// the session, but on a multi-user machine this is a wider surface than the
// socket alone; it is documented in docs/distribution.md.
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
	configureDaemonCmd(cmd) // platform hook: a new process group on Windows
	if err := cmd.Start(); err != nil {
		return "", err
	}
	exited := s.own(cmd)

	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, online, _ := control.FetchStatus(ctx, "", addr); online {
			return "http://" + addr + "/", nil
		}
		select {
		case <-exited:
			return "", fmt.Errorf("the daemon exited before it was ready: %w", s.exitError())
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", errors.New("the daemon did not come up in time; check its output in this terminal")
		case <-tick.C:
		}
	}
}

// own takes responsibility for a started child: one Wait goroutine reaps it.
// Both the readiness loop (to fail fast if the daemon dies early) and Close
// (to reap it without a second Wait) watch for it to finish, so the signal
// is the channel closing rather than a value one of them takes from the
// other — a value sent once leaves whichever arrives second waiting for a
// process that has already been reaped.
func (s *shell) own(cmd *exec.Cmd) chan struct{} {
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.exitErr = err
		s.mu.Unlock()
		close(exited)
	}()
	s.mu.Lock()
	s.daemon, s.exited = cmd, exited
	s.mu.Unlock()
	return exited
}

// Close shuts down the front door and, if this shell launched the daemon, takes
// it down too. A daemon this shell only attached to is left running. It is safe
// to call after Resolve has finished (main waits for the resolve goroutine
// before Close), so the fields it reads are stable.
func (s *shell) Close() {
	s.mu.Lock()
	proxy, cmd, exited := s.proxy, s.daemon, s.exited
	s.mu.Unlock()
	if proxy != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = proxy.Shutdown(ctx)
		cancel()
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Ask for a graceful stop (SIGINT on unix, a console break on Windows) so
	// the daemon runs cmdMount's unmount/close path; fall back to a hard kill.
	terminateDaemon(cmd.Process)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited // reap; the Wait goroutine returns once the kill lands
	}
}

// exitError reports what the daemon's Wait returned. Callers read it after
// the exited channel is closed.
func (s *shell) exitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
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
