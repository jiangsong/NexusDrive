package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"cloudfs/internal/agent"
)

// MCPView is what the control plane needs to hand a person an MCP
// connection: how the HTTP transport is reachable and the access tokens
// that open it. The daemon builds one over agent.Store with NewMCPView; a
// daemon without a store leaves Collector.MCP nil and the routes answer 503.
type MCPView interface {
	// Connect describes the HTTP transport with snippets that carry the
	// <token> placeholder, never a token.
	Connect(ctx context.Context) MCPConnect
	Tokens(ctx context.Context) ([]agent.Principal, error)
	// CreateToken returns the plain token exactly once, with its principal.
	CreateToken(ctx context.Context, spec agent.TokenSpec) (string, agent.Principal, error)
	RevokeToken(ctx context.Context, id string) (agent.Principal, error)
}

// MCPConnect is GET /mcp/connect. Snippets and AddCommands are keyed by
// client (claude, codex) and render the token as <token>.
type MCPConnect struct {
	HTTPListening bool   `json:"http_listening"`
	HTTPAddr      string `json:"http_addr,omitempty"`
	URL           string `json:"url,omitempty"`
	Owner         bool   `json:"owner"`
	// StdioNonOwner is always false in phase 1; the diagnostics item that
	// spots a stdio server beside a mount (T-43) fills it.
	StdioNonOwner bool              `json:"stdio_non_owner"`
	AuthRequired  bool              `json:"auth_required"`
	Snippets      map[string]string `json:"snippets"`
	AddCommands   map[string]string `json:"add_commands"`
}

// TokenView is one issued token as the console reads it: the fingerprint
// is the four characters after the prefix, never the token or its hash.
type TokenView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Read        []string   `json:"read"`
	Write       []string   `json:"write"`
	ReadOnly    bool       `json:"read_only"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	State       string     `json:"state"` // active | expired | revoked
}

// TokensResponse is GET /mcp/tokens.
type TokensResponse struct {
	Tokens []TokenView `json:"tokens"`
}

// TokenCreateRequest is POST /mcp/tokens. A nil Write means "the same as
// Read"; an empty list means no writes.
type TokenCreateRequest struct {
	Name       string   `json:"name"`
	Read       []string `json:"read"`
	Write      []string `json:"write"`
	ReadOnly   bool     `json:"read_only"`
	TTLSeconds int64    `json:"ttl_seconds"`
}

// TokenCreateResponse carries the plain token the one time it exists in
// the clear, with the snippets and add commands rendered against it.
type TokenCreateResponse struct {
	Token       string            `json:"token"`
	Principal   TokenView         `json:"principal"`
	Snippets    map[string]string `json:"snippets"`
	AddCommands map[string]string `json:"add_commands"`
}

// TokenRevokeRequest is POST /mcp/tokens/{id}/revoke.
type TokenRevokeRequest struct {
	Confirm bool `json:"confirm"`
}

// MCPHTTPState is what the process serving the HTTP transport knows about
// it, asked for on every request so a listener that comes up after the
// control plane is reported as soon as it does.
type MCPHTTPState struct {
	// Addr is the listening address; "" when the transport is not served.
	Addr string
	// EnvToken says CLOUDFS_MCP_TOKEN is set, so the listener demands a
	// bearer even before the first issued token exists.
	EnvToken bool
	// Owner says this process owns the cache, so an HTTP client sees the
	// same view as the mount.
	Owner bool
}

// MCPSnippetRenderer renders the registration snippets and add commands
// for url, keyed by client, with TokenPlaceholder in the Authorization
// header. The daemon has cmd/cloudfs inject one so it never imports the MCP
// adapter; the create route substitutes the real token itself.
type MCPSnippetRenderer func(url string) (snippets, addCommands map[string]string)

// TokenPlaceholder is the literal an HTTP snippet carries in place of a
// token; the create route substitutes the real one into its copy.
const TokenPlaceholder = "<token>"

// storeMCPView is MCPView over the store.
type storeMCPView struct {
	st     *agent.Store
	state  func() MCPHTTPState
	render MCPSnippetRenderer
}

// NewMCPView adapts an open agent.db to the connection routes. state is
// consulted per request; render may be nil, in which case Connect carries
// no snippets.
func NewMCPView(st *agent.Store, state func() MCPHTTPState, render MCPSnippetRenderer) MCPView {
	return &storeMCPView{st: st, state: state, render: render}
}

func (v *storeMCPView) Connect(ctx context.Context) MCPConnect {
	st := v.state()
	out := MCPConnect{HTTPListening: st.Addr != "", HTTPAddr: st.Addr, Owner: st.Owner, AuthRequired: st.EnvToken,
		Snippets: map[string]string{}, AddCommands: map[string]string{}}
	if !out.AuthRequired {
		live, err := v.st.HasLiveTokens(ctx)
		out.AuthRequired = err == nil && live
	}
	if out.HTTPListening {
		out.URL = "http://" + st.Addr + "/"
		if v.render != nil {
			s, a := v.render(out.URL)
			out.Snippets, out.AddCommands = orEmpty(s), orEmpty(a)
		}
	}
	return out
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (v *storeMCPView) Tokens(ctx context.Context) ([]agent.Principal, error) {
	return v.st.Tokens(ctx)
}

func (v *storeMCPView) CreateToken(ctx context.Context, spec agent.TokenSpec) (string, agent.Principal, error) {
	return v.st.CreateToken(ctx, spec)
}

func (v *storeMCPView) RevokeToken(ctx context.Context, id string) (agent.Principal, error) {
	return v.st.RevokeToken(ctx, id)
}

// TokenViewOf is one token as the console reads it.
func TokenViewOf(p agent.Principal, now time.Time) TokenView {
	v := TokenView{
		ID: p.ID, Name: p.Name, Fingerprint: p.TokenPrefix,
		Read: append([]string{}, p.Scope.EffectiveRead()...), Write: append([]string{}, p.Scope.EffectiveWrite()...),
		ReadOnly: p.Scope.ReadOnly, State: agent.TokenState(p, now),
	}
	if !p.ExpiresAt.IsZero() {
		t := p.ExpiresAt
		v.ExpiresAt = &t
	}
	if !p.LastUsedAt.IsZero() {
		t := p.LastUsedAt
		v.LastUsedAt = &t
	}
	return v
}

// now is the collector's clock, so a test can pin token states.
func (s *Server) now() time.Time {
	if s.collector.Now != nil {
		return s.collector.Now()
	}
	return time.Now()
}

// mcpView answers the request itself when no store is wired.
func (s *Server) mcpView(w http.ResponseWriter, r *http.Request) (MCPView, bool) {
	if s.collector.MCP == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.mcp_unavailable")
		return nil, false
	}
	return s.collector.MCP, true
}

// GET /mcp/connect
func (s *Server) mcpConnect(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	v, ok := s.mcpView(w, r)
	if !ok {
		return
	}
	writeJSON(w, v.Connect(r.Context()))
}

// GET /mcp/tokens lists tokens by fingerprint; POST /mcp/tokens issues one
// and answers with its plain text the one time it is available.
func (s *Server) mcpTokens(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	v, ok := s.mcpView(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.createToken(w, r, v)
		return
	}
	tokens, err := v.Tokens(r.Context())
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.tokens_list_failed")
		return
	}
	now := s.now()
	out := TokensResponse{Tokens: make([]TokenView, 0, len(tokens))}
	for _, p := range tokens {
		out.Tokens = append(out.Tokens, TokenViewOf(p, now))
	}
	writeJSON(w, out)
}

// createToken is the only handler that ever writes a plain token. The
// reply is marked no-store before it is written and nothing in here logs
// the request or the response.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, v MCPView) {
	var q TokenCreateRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	if q.TTLSeconds < 0 {
		httpErrorT(w, r, http.StatusBadRequest, "err.token_invalid", "ttl_seconds must not be negative")
		return
	}
	spec := agent.TokenSpec{Name: q.Name, Read: q.Read, Write: q.Write, ReadOnly: q.ReadOnly, TTL: time.Duration(q.TTLSeconds) * time.Second}
	plain, p, err := v.CreateToken(r.Context(), spec)
	if errors.Is(err, agent.ErrTokenName) || errors.Is(err, agent.ErrTokenScope) {
		httpErrorT(w, r, http.StatusBadRequest, "err.token_invalid", err.Error())
		return
	}
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.token_create_failed")
		return
	}
	conn := v.Connect(r.Context())
	out := TokenCreateResponse{Token: plain, Principal: TokenViewOf(p, s.now()),
		Snippets: substituteToken(conn.Snippets, plain), AddCommands: substituteToken(conn.AddCommands, plain)}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

// substituteToken renders the placeholder snippets against a real token.
func substituteToken(in map[string]string, token string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = strings.ReplaceAll(v, TokenPlaceholder, token)
	}
	return out
}

// POST /mcp/tokens/<id>/revoke
func (s *Server) mcpTokenByPath(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/mcp/tokens/")
	id, ok := strings.CutSuffix(tail, "/revoke")
	if !ok || id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	v, ok := s.mcpView(w, r)
	if !ok {
		return
	}
	var q TokenRevokeRequest
	if !decodeMutationLimit(w, r, &q, 1<<10) {
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.token_revoke", id) {
		return
	}
	p, err := v.RevokeToken(r.Context(), id)
	if errors.Is(err, agent.ErrPrincipalNotFound) {
		httpErrorT(w, r, http.StatusNotFound, "err.token_not_found")
		return
	}
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.token_revoke_failed")
		return
	}
	writeJSON(w, TokenViewOf(p, s.now()))
}
