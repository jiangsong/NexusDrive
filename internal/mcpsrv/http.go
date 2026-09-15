package mcpsrv

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeHTTP serves the MCP server over Streamable HTTP on addr until ctx ends.
// It refuses to bind a non-loopback address without a bearer token, because
// the server hands out read and write access to the user's cloud storage.
func ServeHTTP(ctx context.Context, s *Server, addr string) error {
	return ServeHTTPWithToken(ctx, s, addr, "")
}

// ServeHTTPWithToken is ServeHTTP with bearer-token authentication. A token is
// required for any address that is not loopback.
func ServeHTTPWithToken(ctx context.Context, s *Server, addr, token string) error {
	return ServeHTTPWithAuth(ctx, s, addr, HTTPAuth{Token: token})
}

// HTTPAuth is how the HTTP transport decides who a request is. The legacy
// environment token keeps full access so an existing registration keeps
// working; issued tokens are looked up through Verify and carry their own
// scope; Open lets a loopback listener admit local callers until the first
// token exists.
type HTTPAuth struct {
	// Token is the legacy CLOUDFS_MCP_TOKEN: full access, kept for compatibility.
	Token string
	// Verify resolves an issued token to its principal. nil = no issued tokens.
	Verify func(ctx context.Context, plain string) (agent.Principal, error)
	// Open reports whether a request with no Authorization header may pass as
	// the local default principal. Only consulted on loopback listeners.
	Open func(ctx context.Context) bool
}

// enabled reports whether any bearer token can be accepted at all.
func (a HTTPAuth) enabled() bool { return a.Token != "" || a.Verify != nil }

// envPrincipalID is the TokenInfo.UserID a request authenticated with the
// legacy environment token carries. Issued tokens carry their principal's
// UUID, so the two can never collide.
const envPrincipalID = "env"

// NewHTTPHandler is the MCP HTTP handler behind the given authentication.
// With neither a legacy token nor a verifier the handler is open, which is
// only ever reached on a loopback listener.
func NewHTTPHandler(s *Server, a HTTPAuth) http.Handler {
	h := newMCPHTTPHandler(s)
	if !a.enabled() {
		return h
	}
	return requireAuth(h, a)
}

// ServeHTTPWithAuth serves the MCP server over Streamable HTTP on addr until
// ctx ends. A non-loopback address must have some authentication and never
// admits an anonymous caller, whatever Open says.
func ServeHTTPWithAuth(ctx context.Context, s *Server, addr string, a HTTPAuth) error {
	defer s.Close()
	// Also release the shutdown waiter if binding fails before ctx ends.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	loopback, err := isLoopback(addr)
	if err != nil {
		return err
	}
	if !loopback {
		a.Open = nil
		if !a.enabled() {
			return fmt.Errorf("mcpsrv: refusing to serve %s without a token: this server can read and write your cloud storage, so a non-loopback address needs authentication", addr)
		}
	}
	srv := &http.Server{Addr: addr, Handler: NewHTTPHandler(s, a), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Keep the session-based transport for older clients. The SDK's stateful
// transport does not support the 2026-07-28 protocol; selecting it for every
// request silently makes discovery negotiate an older protocol instead.
// Both handlers retain SDK header/body validation and share the same VFS and
// authorization boundary. A client-controlled version is routing, not trust.
func newMCPHTTPHandler(s *Server) http.Handler {
	getServer := func(*http.Request) *mcp.Server { return s.MCP() }
	legacy := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{SessionTimeout: 5 * time.Minute})
	modern := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true})
	protection := http.NewCrossOriginProtection()
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go's standard protection exempts safe methods; MCP also requires
		// validating Origin on the legacy GET notification stream.
		if r.Header.Get("Origin") != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions) {
			check := r.Clone(r.Context())
			check.Method = http.MethodPost
			if err := protection.Check(check); err != nil {
				http.Error(w, "cross-origin MCP request denied", http.StatusForbidden)
				return
			}
		}
		if r.Header.Get("MCP-Protocol-Version") >= "2026-07-28" {
			if acceptRedundantHTTPCancellation(w, r) {
				return
			}
			modern.ServeHTTP(w, r)
		} else {
			legacy.ServeHTTP(w, r)
		}
	})
	return protection.Handler(route)
}

// go-sdk v1.7.0 sends a redundant stdio-style cancellation POST after closing
// an HTTP subscription stream, without the new request metadata. Rejecting it
// makes that client fail its entire connection. Accept ONLY a bounded, valid
// cancellation notification as an inert acknowledgment. Never resolve its ID
// or cancel another POST: only the original stream's disconnect may do that.
// Authentication and cross-origin protection remain outside this adapter.
func acceptRedundantHTTPCancellation(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || r.Header.Get("MCP-Protocol-Version") != "2026-07-28" {
		return false
	}
	method := r.Header.Get("Mcp-Method")
	if method != "" && method != "notifications/cancelled" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.Body == nil {
		return false
	}
	// Valid modern calls have Mcp-Method; this small bound is only for the
	// metadata-less compatibility notification, not file upload requests.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	r.Body.Close()
	if err != nil {
		http.Error(w, "invalid cancellation body", http.StatusBadRequest)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason"`
			Meta      map[string]any  `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.JSONRPC != "2.0" || len(msg.ID) != 0 || msg.Method != "notifications/cancelled" {
		return false
	}
	var id any
	decoder := json.NewDecoder(bytes.NewReader(msg.Params.RequestID))
	decoder.UseNumber()
	if decoder.Decode(&id) != nil {
		return false
	}
	switch id := id.(type) {
	case string:
	case json.Number:
		if _, err := id.Int64(); err != nil {
			return false
		}
	default:
		return false
	}
	if v, exists := msg.Params.Meta[mcp.MetaKeyProtocolVersion]; exists && v != "2026-07-28" {
		return false
	}
	jsonOK, streamOK := false, false
	for _, accept := range r.Header.Values("Accept") {
		for _, value := range strings.Split(accept, ",") {
			typeName, _, err := mime.ParseMediaType(strings.TrimSpace(value))
			if err == nil {
				jsonOK = jsonOK || typeName == "application/json"
				streamOK = streamOK || typeName == "text/event-stream"
			}
		}
	}
	if !jsonOK || !streamOK {
		return false
	}
	w.WriteHeader(http.StatusAccepted)
	return true
}

// requireAuth admits a request that carries the legacy token or a live
// issued token, and records which as the request's TokenInfo so the session
// layer can name the principal. Anything else is 401; an unknown, expired
// and revoked token all look the same to the caller. A request with no
// Authorization header at all may still pass when Open says the listener is
// open.
func requireAuth(next http.Handler, a HTTPAuth) http.Handler {
	verifier := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		far := time.Now().Add(100 * 365 * 24 * time.Hour)
		if a.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) == 1 {
			return &auth.TokenInfo{UserID: envPrincipalID, Expiration: far}, nil
		}
		if a.Verify == nil {
			return nil, auth.ErrInvalidToken
		}
		p, err := a.Verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		exp := p.ExpiresAt
		if exp.IsZero() {
			exp = far
		}
		return &auth.TokenInfo{UserID: p.ID, Expiration: exp}, nil
	}
	guarded := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && a.Open != nil && a.Open(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(&challengeWriter{ResponseWriter: w}, r)
	})
}

// challengeWriter adds the WWW-Authenticate challenge to a 401 written by
// the SDK middleware, which only sets one when it has OAuth metadata to
// advertise. Unwrap keeps http.ResponseController, and with it SSE
// flushing, working through the wrapper.
type challengeWriter struct {
	http.ResponseWriter
}

func (w *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cloudfs"`)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requireBearer(next http.Handler, token string) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cloudfs"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("mcpsrv: bad address %q: %w", addr, err)
	}
	// An empty host binds all interfaces, not localhost.
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname we cannot classify: treat it as public and require a token.
		return false, nil
	}
	return ip.IsLoopback(), nil
}

// ClientConfig renders the registration snippet for an agent client.
func ClientConfig(client, binary string, allow []string, readOnly bool) (string, error) {
	args := []string{`"mcp"`, `"--stdio"`}
	for _, a := range allow {
		args = append(args, `"--allow"`, `"`+a+`"`)
	}
	if readOnly {
		args = append(args, `"--read-only"`)
	}
	joined := strings.Join(args, ", ")

	switch strings.ToLower(client) {
	case "claude", "claude-code":
		return fmt.Sprintf(`{
  "mcpServers": {
    "cloudfs": {
      "command": %q,
      "args": [%s]
    }
  }
}`, binary, joined), nil
	case "codex":
		tomlArgs := make([]string, 0, len(args))
		for _, a := range args {
			tomlArgs = append(tomlArgs, a)
		}
		return fmt.Sprintf(`[mcp_servers.cloudfs]
command = %q
args = [%s]
`, binary, strings.Join(tomlArgs, ", ")), nil
	default:
		return "", fmt.Errorf("mcpsrv: unknown client %q; use claude or codex", client)
	}
}
