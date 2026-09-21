package mcpsrv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio→HTTP bridge (docs/agent-first-design.md §5.2, TODO.md T-50,
// closing T-43). A stdio MCP server started beside `cloudfs mount` does
// not own the cache: it has its own VFS view, no uploader and no shared
// session state, so every tool that changes something used to refuse
// with errRequiresOwner and a shell command the agent cannot run. With a
// bridge the refusal becomes a forward: the call goes, arguments intact,
// to the owner's HTTP transport, which runs it under its own scope, and
// the answer comes back as this server's. Reads stay local, where the
// shared meta and block cache already serve them.
//
// The owner accepts the bridge with a secret it writes to
// <cache.dir>/agent/bridge.token (0600): the same OS user reads it, so no
// token has to be issued or pasted, and the request must arrive from a
// loopback address. Calls forwarded this way run as the owner's default
// principal under transport http-bridge, narrowed to the stdio server's
// own scope (its --allow / --read-only), which travels in a header the
// stdio process sets and the agent behind it cannot: what the person
// restricted for that client stays restricted on the owner. Each stdio
// process names its connection in another header, so two agents beside
// one mount get two owner sessions and neither can finish or roll back
// the other's. The stdio side's audit row says forwarded, so the two
// rows of one call are told apart.

// bridgePrincipalID is the TokenInfo.UserID a request carrying the
// bridge secret is given; resolveSession maps it to the default
// principal under its own transport name.
const bridgePrincipalID = "bridge"

// BridgeTokenPath is where the owner keeps the bridge secret; the name
// is agent.BridgeTokenPath's so the control plane can see the file too.
func BridgeTokenPath(agentDir string) string { return agent.BridgeTokenPath(agentDir) }

// WriteBridgeToken creates (or reuses) the owner's bridge secret and
// returns it. The file is 0600 in a 0700 directory: what the OS user can
// read, the OS user's own stdio server may present.
func WriteBridgeToken(agentDir string) (string, error) {
	p := BridgeTokenPath(agentDir)
	if data, err := os.ReadFile(p); err == nil && len(data) >= 32 {
		return string(data), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := "cfsb_" + hex.EncodeToString(buf)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadBridgeToken reads the owner's bridge secret, "" when there is none
// (no owner serving HTTP, or an older owner).
func ReadBridgeToken(agentDir string) string {
	data, err := os.ReadFile(BridgeTokenPath(agentDir))
	if err != nil || len(data) < 32 {
		return ""
	}
	return string(data)
}

// Headers a bridged connection carries. They are set by the stdio
// process's HTTP transport, never by the agent (which speaks stdio to
// that process and sets no headers), so the owner may trust them as much
// as the bridge secret beside them.
const (
	// bridgeConnHeader names the stdio process: the owner's connection key.
	bridgeConnHeader = "X-Cloudfs-Bridge-Conn"
	// bridgeScopeHeader is the stdio server's own scope as JSON; the owner
	// narrows the bridged session to it.
	bridgeScopeHeader = "X-Cloudfs-Bridge-Scope"
)

// BridgeOptions is how a NonOwner server reaches the owner.
type BridgeOptions struct {
	// URL is the owner's Streamable HTTP endpoint.
	URL string
	// Token is the bridge secret (or any token the owner accepts).
	Token string
	// HTTPClient overrides the client used to reach the owner; nil uses
	// a default with the token as bearer.
	HTTPClient *http.Client
}

// bridgedTools is every tool the owner fence covers: the calls a
// NonOwner server forwards instead of refusing. Kept as a table beside
// the fence sites; TestBridgeForwardsEveryFencedTool keeps them equal.
var bridgedTools = map[string]bool{
	"write_file": true, "edit_file": true, "create_directory": true, "copy": true, "move": true, "delete": true,
	"pin": true, "unpin": true, "export": true, "cancel_export_job": true,
	"retry_upload": true, "cancel_upload": true, "resume_upload": true, "discard_upload": true, "flush_uploads": true,
	"retry_copy_job": true, "cancel_copy_job": true, "forget_copy_job": true,
	"index": true, "unindex": true,
	"begin_session": true, "finish_session": true, "list_sessions": true, "rollback_session": true,
	"memory_put": true, "memory_delete": true, "memory_propose": true, "memory_review": true,
}

// bridgedReadTools are the fenced tools that change nothing, so a
// read-only stdio server still forwards them.
var bridgedReadTools = map[string]bool{"list_sessions": true}

// bridge is the lazily connected client to the owner.
type bridge struct {
	opt BridgeOptions
	// connID names this process on the owner; scopeJSON is the stdio
	// server's scope, sent with every request.
	connID    string
	scopeJSON string

	mu      sync.Mutex
	agent   mcp.Implementation // the client behind the stdio server, once known
	client  *mcp.Client
	session *mcp.ClientSession
}

// bridgeTransport adds the bridge secret, the connection id and the
// scope to every request.
type bridgeTransport struct {
	b    *bridge
	next http.RoundTripper
}

func (t *bridgeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.b.opt.Token)
	r.Header.Set(bridgeConnHeader, t.b.connID)
	r.Header.Set(bridgeScopeHeader, t.b.scopeJSON)
	return t.next.RoundTrip(r)
}

// newBridge prepares the client to the owner for a stdio server whose
// own scope is scope. It fails only when the scope cannot be encoded.
func newBridge(opt BridgeOptions, scope agent.Scope) (*bridge, error) {
	b := &bridge{opt: opt}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	b.connID = hex.EncodeToString(buf)
	sj, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	b.scopeJSON = string(sj)
	base := opt.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}
	client := *base
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = &bridgeTransport{b: b, next: next}
	b.opt.HTTPClient = &client
	return b, nil
}

// setAgent records the client behind the stdio server, so the owner's
// session for this bridge is named after the agent rather than the bridge.
// It matters until the first dial; a later change is not resent.
func (b *bridge) setAgent(info *mcp.Implementation) {
	if info == nil || info.Name == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		b.agent = *info
	}
}

// connect returns the owner session, dialling on first use and after a
// dropped connection.
func (b *bridge) connect(ctx context.Context) (*mcp.ClientSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		return b.session, nil
	}
	if b.client == nil {
		impl := b.agent
		if impl.Name == "" {
			impl = mcp.Implementation{Name: "cloudfs-bridge", Version: "1"}
		}
		b.client = mcp.NewClient(&impl, nil)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	session, err := b.client.Connect(dialCtx, &mcp.StreamableClientTransport{Endpoint: b.opt.URL, HTTPClient: b.opt.HTTPClient}, nil)
	if err != nil {
		return nil, err
	}
	b.session = session
	return session, nil
}

// drop forgets a session that failed, so the next call redials.
func (b *bridge) drop(session *mcp.ClientSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == session {
		b.session = nil
		_ = session.Close()
	}
}

// close ends the owner session. The stdio process was the session: when
// it goes, its owner-side CloudFS session is finished first (the same
// finish_session an agent would call, with no arguments so it names the
// bridged session itself), so the console shows the run as finished
// rather than active until the idle sweep.
func (b *bridge) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.session.CallTool(ctx, &mcp.CallToolParams{Name: "finish_session", Arguments: map[string]any{}})
	_ = b.session.Close()
	b.session = nil
}

// forward runs one tool call on the owner.
func (b *bridge) forward(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	session, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		b.drop(session)
		return nil, err
	}
	return res, nil
}

// errBridgeDown is what a forwarded call reports when the owner cannot
// be reached: the fence's own advice, with the reason.
func errBridgeDown(err error) error {
	return fmt.Errorf("the owner could not be reached through the bridge (%v); %w", err, errRequiresOwner)
}

// forwardedResult marks the stdio side's audit row: the call ran, but
// elsewhere. The middleware reads it back through the note.
const resultForwarded = "forwarded"

// bridgeMiddleware forwards fenced tool calls on a NonOwner server with
// a bridge; every other call, and every call on an owner, passes through.
// It sits inside the audit middleware, so the stdio row records the call
// with result forwarded. A forwarded call skips this server's handlers
// and with them its path checks, so the write permission is checked here
// against this server's scope first; the owner then narrows its own
// session to the same scope (bridgeScopeHeader) and checks the paths.
func (s *Server) bridgeMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if s.bridge == nil || method != "tools/call" {
			return next(ctx, method, req)
		}
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || call.Params == nil || !bridgedTools[call.Params.Name] {
			return next(ctx, method, req)
		}
		if !bridgedReadTools[call.Params.Name] {
			if err := s.checkWrite(ctx); err != nil {
				r, _ := fail(err)
				return r, nil
			}
		}
		ss, _ := req.GetSession().(*mcp.ServerSession)
		s.bridge.setAgent(clientInfoOf(req, ss))
		res, err := s.bridge.forward(ctx, &mcp.CallToolParams{Name: call.Params.Name, Arguments: call.Params.Arguments, Meta: call.Params.Meta})
		if err != nil {
			r, _ := fail(errBridgeDown(err))
			recordCheck(ctx, "", errRequiresOwner)
			return r, nil
		}
		if n := noteFrom(ctx); n != nil {
			n.mu.Lock()
			n.forwarded = true
			n.mu.Unlock()
		}
		return res, nil
	}
}

// Bridged reports whether this server forwards fenced calls.
func (s *Server) Bridged() bool { return s.bridge != nil }

// BridgeOptionsFor decides whether a stdio server beside the owner gets a
// bridge: only when the owner's HTTP transport listens on a loopback
// address — the only place the secret is honoured (HTTPAuth.Bridge reads
// the peer's address) — and the owner has published one. Anything else
// returns nil, and the fenced tools refuse as they do without an owner
// serving HTTP; `cloudfs mcp` says why on stderr.
func BridgeOptionsFor(listen, token string) *BridgeOptions {
	if token == "" || !loopbackListen(listen) {
		return nil
	}
	return &BridgeOptions{URL: "http://" + listen + "/", Token: token}
}

// loopbackListen says a listen address binds a loopback interface;
// ":8765" binds every interface and is not one.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
