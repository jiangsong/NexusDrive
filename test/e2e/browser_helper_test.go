// test/e2e/browser_helper_test.go
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stackOptions lets a test extend the shared e2e stack without editing
// newStack: extra top-level configuration, and a hook on the MCP options once
// the daemon exists.
type stackOptions struct {
	extraYAML string
	mcp       func(d *daemon.Daemon, o *mcpsrv.Options)
}

func newStackWith(t *testing.T, mode string, o stackOptions) *stack {
	t.Helper()
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	return buildStack(t, mode, o, true)
}

func newUnmountedStack(t *testing.T, o stackOptions) *stack {
	t.Helper()
	return buildStack(t, "writeback", o, false)
}

func buildStack(t *testing.T, mode string, o stackOptions, mount bool) *stack {
	t.Helper()
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.yaml")
	body := fmt.Sprintf(configTemplate, cacheDir, mountDir, mode) + o.extraYAML
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		cancel()
		d.Close()
		t.Fatal("the demo remote did not resolve to the fake provider")
	}
	var m *fusefs.Mount
	if mount {
		m, err = fusefs.MountFS(fusefs.MountOptions{
			Options: fusefs.Options{FS: d.FS, AttrTimeout: time.Second, EntryTimeout: time.Second},
			Path:    mountDir,
		})
		if err != nil {
			cancel()
			d.Close()
			t.Fatalf("mount: %v", err)
		}
	}
	opts := mcpsrv.Options{FS: d.FS, Version: "e2e"}
	if o.mcp != nil {
		o.mcp(d, &opts)
	}
	srv, err := mcpsrv.New(opts)
	if err != nil {
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
		t.Fatal(err)
	}
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
		t.Fatal(err)
	}
	s := &stack{d: d, mount: m, dir: mountDir, fake: fake, session: session, cfg: cfg}
	t.Cleanup(func() {
		session.Close()
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
	})
	return s
}

// requireBrowser skips unless CLOUDFS_BROWSER=1 and a Chrome or Chromium
// binary can be found. CLOUDFS_CHROME names one explicitly.
func requireBrowser(t *testing.T) string {
	t.Helper()
	if os.Getenv("CLOUDFS_BROWSER") != "1" {
		t.Skip("set CLOUDFS_BROWSER=1 to run the headless browser smoke")
	}
	candidates := []string{
		os.Getenv("CLOUDFS_CHROME"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
	}
	// Playwright keeps a Chromium under the user cache; a dev box without a
	// system browser usually has one. Its headless shell starts and exits in
	// well under a second where the full browser spends tens of seconds on
	// first-run work, so it comes first.
	if home, err := os.UserHomeDir(); err == nil {
		for _, pattern := range []string{
			filepath.Join("chromium_headless_shell-*", "chrome-headless-shell-linux64", "chrome-headless-shell"),
			filepath.Join("chromium-*", "chrome-linux64", "chrome"),
		} {
			matches, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", pattern))
			candidates = append(candidates, matches...)
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	t.Skip("CLOUDFS_BROWSER=1 but no Chrome or Chromium was found; set CLOUDFS_CHROME")
	return ""
}

// startControlUI serves the embedded console on a loopback port, the same
// handler the daemon mounts.
func startControlUI(t *testing.T, col *control.Collector) string {
	t.Helper()
	srv := control.NewServer(col)
	srv.EnableUI()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// renderedDOM loads url in headless Chrome, waits for the page's modules and
// first API calls to settle, and returns the serialized DOM.
//
// The console keeps an EventSource open on /events for as long as the page
// lives, and Chrome's --dump-dom cannot see past that: --virtual-time-budget
// never elapses while a fetch is pending, and without it the dump happens on
// the load event, before any API response has arrived. So the page is driven
// over the DevTools protocol on --remote-debugging-pipe instead (NUL-delimited
// JSON on fds 3 and 4, no WebSocket and no extra dependency): navigate, wait
// for the load event, then poll the DOM until every string in want is
// present or the deadline passes. The DOM is returned either way, so a
// caller's own assertion produces the failure message. With no want strings
// the DOM is taken once two samples a beat apart agree, which on a loopback
// server is after the first API responses have been rendered.
func renderedDOM(t *testing.T, chrome, url string, want ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, err := startBrowser(ctx, chrome)
	if err != nil {
		t.Fatalf("headless chrome: %v", err)
	}
	defer b.close()
	dom, err := b.render(ctx, url, want, 30*time.Second)
	if err != nil {
		t.Fatalf("headless chrome %s: %v", url, err)
	}
	return dom
}

// browser is one headless Chrome process spoken to over the DevTools pipe.
type browser struct {
	cmd    *exec.Cmd
	in     *os.File // our end of the pipe Chrome reads (its fd 3)
	out    *bufio.Reader
	outF   *os.File
	msgs   chan cdpMessage
	nextID int
}

// cdpMessage is one DevTools message in either direction: a response
// (ID set), or an event (Method set).
type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startBrowser(ctx context.Context, chrome string) (*browser, error) {
	// Chrome reads commands from its fd 3 and writes responses to its fd 4;
	// ExtraFiles hands them over in that order.
	cmdIn, ourIn, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ourOut, cmdOut, err := os.Pipe()
	if err != nil {
		cmdIn.Close()
		ourIn.Close()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-pipe", "about:blank")
	cmd.ExtraFiles = []*os.File{cmdIn, cmdOut}
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		cmdIn.Close()
		ourIn.Close()
		ourOut.Close()
		cmdOut.Close()
		return nil, err
	}
	// The child holds its own copies now.
	cmdIn.Close()
	cmdOut.Close()
	b := &browser{cmd: cmd, in: ourIn, out: bufio.NewReaderSize(ourOut, 1<<20), outF: ourOut, msgs: make(chan cdpMessage, 256)}
	go b.readLoop()
	return b, nil
}

// readLoop turns the NUL-delimited stream from Chrome into messages. It ends
// when Chrome closes the pipe.
func (b *browser) readLoop() {
	defer close(b.msgs)
	for {
		raw, err := b.out.ReadBytes(0)
		if len(raw) > 1 {
			var m cdpMessage
			if json.Unmarshal(raw[:len(raw)-1], &m) == nil {
				b.msgs <- m
			}
		}
		if err != nil {
			return
		}
	}
}

// call sends one command and waits for its response, discarding events
// that arrive in between unless events is non-nil, in which case they are
// forwarded there.
func (b *browser) call(ctx context.Context, sessionID, method string, params any, events chan<- cdpMessage) (json.RawMessage, error) {
	b.nextID++
	id := b.nextID
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if sessionID != "" {
		req["sessionId"] = sessionID
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := b.in.Write(append(body, 0)); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: %w", method, ctx.Err())
		case m, ok := <-b.msgs:
			if !ok {
				return nil, fmt.Errorf("%s: chrome closed the debugging pipe", method)
			}
			if m.ID == id {
				if m.Error != nil {
					return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
				}
				return m.Result, nil
			}
			if m.Method != "" && events != nil {
				select {
				case events <- m:
				default:
				}
			}
		}
	}
}

// render navigates a fresh tab to url and returns its DOM once every string
// in want is present (or, with no want, once it stops changing), or once
// wait has passed since the load event.
func (b *browser) render(ctx context.Context, url string, want []string, wait time.Duration) (string, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	res, err := b.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(res, &created); err != nil {
		return "", err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	res, err = b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(res, &attached); err != nil {
		return "", err
	}
	sid := attached.SessionID
	if _, err := b.call(ctx, sid, "Page.enable", nil, nil); err != nil {
		return "", err
	}
	// Events that arrive while later commands are in flight are collected
	// here so the load event is not lost between calls.
	events := make(chan cdpMessage, 64)
	if _, err := b.call(ctx, sid, "Page.navigate", map[string]any{"url": url}, events); err != nil {
		return "", err
	}
	loadCtx, cancelLoad := context.WithTimeout(ctx, wait)
	defer cancelLoad()
	if err := b.waitEvent(loadCtx, events, sid, "Page.loadEventFired"); err != nil {
		return "", fmt.Errorf("waiting for the load event: %w", err)
	}
	deadline := time.Now().Add(wait)
	var last string
	for {
		dom, err := b.outerHTML(ctx, sid)
		if err != nil {
			return "", err
		}
		settled := dom == last
		if len(want) > 0 {
			settled = hasAll(dom, want)
		}
		if settled || time.Now().After(deadline) {
			return dom, nil
		}
		last = dom
		select {
		case <-ctx.Done():
			return dom, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (b *browser) waitEvent(ctx context.Context, events <-chan cdpMessage, sessionID, method string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-events:
			if m.Method == method && m.SessionID == sessionID {
				return nil
			}
		case m, ok := <-b.msgs:
			if !ok {
				return errors.New("chrome closed the debugging pipe")
			}
			if m.Method == method && m.SessionID == sessionID {
				return nil
			}
		}
	}
}

func (b *browser) outerHTML(ctx context.Context, sessionID string) (string, error) {
	res, err := b.call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression": "document.documentElement.outerHTML", "returnByValue": true,
	}, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Result.Value, nil
}

func hasAll(dom string, want []string) bool {
	for _, w := range want {
		if !strings.Contains(dom, w) {
			return false
		}
	}
	return true
}

// close asks Chrome to exit and gives it a moment before killing it: the
// full browser can spend a long time on background work at shutdown, and a
// test has nothing to wait for.
func (b *browser) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.call(ctx, "", "Browser.close", nil, nil)
	b.in.Close()
	done := make(chan struct{})
	go func() { _ = b.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = b.cmd.Process.Kill()
		<-done
	}
	b.outF.Close()
}
