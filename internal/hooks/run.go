package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The run-time side: `cloudfs agent-hook prompt|read|stop` reads the
// client's event JSON from stdin, asks the owner daemon's control plane
// for what the turn needs, and prints what the client's hook contract
// expects. Everything is best effort: a hook must never fail the turn, so
// every error path is a silent, successful exit, and the daemon being
// offline is the commonest of them.

// Event is the part of a client's hook payload the hooks read. Every
// supported client sends session_id and cwd; the tool fields ride on
// tool events.
type Event struct {
	SessionID string          `json:"session_id"`
	CWD       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// maxEventBytes bounds what a hook reads from stdin.
const maxEventBytes = 1 << 20

// ParseEvent reads one event; malformed input yields an empty event.
func ParseEvent(r io.Reader) Event {
	var e Event
	data, _ := io.ReadAll(io.LimitReader(r, maxEventBytes))
	_ = json.Unmarshal(data, &e)
	if e.CWD == "" {
		e.CWD, _ = os.Getwd()
	}
	return e
}

// ContextRequest is what the prompt hook asks the daemon.
type ContextRequest struct {
	Client    string `json:"client"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
}

// ContextResponse is the daemon's answer: the text to inject and, for
// the console and tests, its parts.
type ContextResponse struct {
	Context string `json:"context"`
	// Mount is the mount the working directory is in; empty means none,
	// and the hook prints nothing.
	Mount string `json:"mount,omitempty"`
	// Changed lists the paths (as the agent sees them from cwd) that
	// changed since its last turn; More counts what the list left out.
	Changed []string `json:"changed,omitempty"`
	More    int      `json:"more,omitempty"`
	Rescan  bool     `json:"rescan,omitempty"`
	Tokens  int      `json:"tokens"`
}

// ReadRequest is what the read hook reports: absolute paths the agent
// read, which the daemon maps into its mounts.
type ReadRequest struct {
	Client string   `json:"client"`
	Paths  []string `json:"paths"`
}

// StopRequest asks the daemon to finish the client's active session.
type StopRequest struct {
	Client    string `json:"client"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
}

// StopResponse says which session, if any, was finished. Candidates is
// how many active sessions looked like the client's; with more than one
// the daemon finishes none rather than guess.
type StopResponse struct {
	Finished   bool   `json:"finished"`
	SessionID  string `json:"session_id,omitempty"`
	Candidates int    `json:"candidates,omitempty"`
}

// Client talks to the daemon's control plane over its unix socket (or
// loopback TCP), the same way `cloudfs status` does.
type Client struct {
	http *http.Client
}

// hookTimeout bounds a hook's wait for the daemon; the prompt hook is
// on the turn's critical path.
const hookTimeout = 3 * time.Second

// NewClient dials socket first, then tcp; either may be empty.
func NewClient(socket, tcp string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: time.Second}
		if socket != "" {
			if c, err := d.DialContext(ctx, "unix", socket); err == nil {
				return c, nil
			}
		}
		if tcp != "" {
			return d.DialContext(ctx, "tcp", tcp)
		}
		return nil, errors.New("hooks: no control endpoint")
	}
	return &Client{http: &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: hookTimeout}}
}

func (c *Client) post(ctx context.Context, route string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cloudfs"+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The control plane's mutation header, as every native client sends.
	req.Header.Set("X-CloudFS-Control", "1")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hooks: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Context asks for the turn's context.
func (c *Client) Context(ctx context.Context, req ContextRequest) (ContextResponse, error) {
	var out ContextResponse
	err := c.post(ctx, "/agent/hook-context", req, &out)
	return out, err
}

// Read reports paths the agent read.
func (c *Client) Read(ctx context.Context, req ReadRequest) error {
	return c.post(ctx, "/agent/hook-read", req, nil)
}

// Stop asks the daemon to finish the client's session.
func (c *Client) Stop(ctx context.Context, req StopRequest) (StopResponse, error) {
	var out StopResponse
	err := c.post(ctx, "/agent/hook-stop", req, &out)
	return out, err
}

// promptEvent is the hookEventName a client expects back from its
// turn-start hook.
func promptEvent(client string) string {
	if p, ok := platforms[client]; ok {
		return p.prompt
	}
	return "UserPromptSubmit"
}

// RunPrompt is `cloudfs agent-hook prompt`: print the additionalContext
// JSON for the turn, or nothing when the daemon is offline or the
// directory is in no mount.
func RunPrompt(ctx context.Context, in io.Reader, out io.Writer, client string, api *Client) error {
	ev := ParseEvent(in)
	cwd, err := filepath.Abs(ev.CWD)
	if err != nil {
		return nil
	}
	resp, err := api.Context(ctx, ContextRequest{Client: client, SessionID: ev.SessionID, CWD: cwd})
	if err != nil || resp.Context == "" {
		return nil
	}
	payload := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     promptEvent(client),
		"additionalContext": resp.Context,
	}}
	enc, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	_, _ = fmt.Fprintln(out, string(enc))
	return nil
}

// RunRead is `cloudfs agent-hook read`: report the files the tool event
// names. Reads happen through more than the read tool — a grep consumes
// the files its matches came from, a shell command reads whatever files
// it names — so the tool's input is mined for existing regular files.
func RunRead(ctx context.Context, in io.Reader, client string, api *Client) error {
	ev := ParseEvent(in)
	paths := ReadPaths(ev)
	if len(paths) == 0 {
		return nil
	}
	_ = api.Read(ctx, ReadRequest{Client: client, Paths: paths})
	return nil
}

// maxReadPaths bounds what one tool event reports.
const maxReadPaths = 32

// ReadPaths lists the absolute regular files a tool event read: the
// file_path / path / notebook_path fields when they name a file, and for
// a shell command every token that is an existing file under cwd.
func ReadPaths(ev Event) []string {
	if len(ev.ToolInput) == 0 {
		return nil
	}
	var input map[string]any
	if err := json.Unmarshal(ev.ToolInput, &input); err != nil {
		return nil
	}
	var out []string
	add := func(raw string) {
		if raw == "" || len(out) >= maxReadPaths {
			return
		}
		p := raw
		if !filepath.IsAbs(p) {
			p = filepath.Join(ev.CWD, p)
		}
		p = filepath.Clean(p)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			for _, have := range out {
				if have == p {
					return
				}
			}
			out = append(out, p)
		}
	}
	for _, key := range []string{"file_path", "path", "notebook_path", "absolute_path"} {
		if v, ok := input[key].(string); ok {
			add(v)
		}
	}
	if cmd, ok := input["command"].(string); ok {
		for _, tok := range strings.Fields(cmd) {
			tok = strings.Trim(tok, `"'`+"`;|&()<>")
			if strings.HasPrefix(tok, "-") || tok == "" {
				continue
			}
			add(tok)
		}
	}
	return out
}

// RunStop is `cloudfs agent-hook stop`: finish the client's active
// session so the console shows the run as done.
func RunStop(ctx context.Context, in io.Reader, client string, api *Client) error {
	ev := ParseEvent(in)
	cwd, _ := filepath.Abs(ev.CWD)
	_, _ = api.Stop(ctx, StopRequest{Client: client, SessionID: ev.SessionID, CWD: cwd})
	return nil
}
