package mcpsrv

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionMiddleware gives every incoming call a CloudFS session, resolved
// from the connection it arrived on, so that path checks read the session's
// scope rather than one process-wide allowlist. It is a no-op when no session
// store is configured.
func (s *Server) sessionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if s.opt.Sessions == nil {
			return next(ctx, method, req)
		}
		sess, err := s.resolveSession(ctx, req)
		if err != nil {
			return nil, err
		}
		return next(agent.WithSession(ctx, sess), method, req)
	}
}

// onInitialized resolves the session as soon as a legacy-protocol client has
// finished its handshake, so a session is visible in the console before the
// first tool call rather than after it.
func (s *Server) onInitialized(ctx context.Context, req *mcp.InitializedRequest) {
	if s.opt.Sessions == nil {
		return
	}
	_, _ = s.resolveSession(ctx, req)
}

// resolveSession classifies the connection behind a request and asks the
// session store for the session that belongs to it.
func (s *Server) resolveSession(ctx context.Context, req mcp.Request) (agent.Session, error) {
	c := agent.ConnInfo{}
	ss, _ := req.GetSession().(*mcp.ServerSession)
	if info := clientInfoOf(req, ss); info != nil {
		c.ClientName, c.ClientVersion = info.Name, info.Version
	}
	extra := req.GetExtra()
	switch {
	case extra != nil && extra.TokenInfo != nil && extra.TokenInfo.UserID != "":
		// A bearer token verified upstream names its principal; every request
		// with that token is one connection, whatever SDK session carried it.
		c.Key, c.Transport, c.PrincipalID = "token:"+extra.TokenInfo.UserID, "http-token", extra.TokenInfo.UserID
	case ss != nil && ss.ID() != "":
		// The legacy stateful HTTP transport keeps one SDK session per client
		// and hands out its id in a header.
		c.Key, c.Transport, c.PrincipalID = "legacy:"+ss.ID(), "http-legacy", s.defaultPrincipal.ID
	case extra != nil && extra.Header != nil:
		// Stateless HTTP without a bearer token (loopback, no token issued
		// yet): every POST is a fresh SDK session, so the connection key is
		// the principal and the session rotates on idle like a token session.
		c.Key, c.Transport, c.PrincipalID = "loopback:"+s.defaultPrincipal.ID, "http-loopback", s.defaultPrincipal.ID
	default:
		// stdio (and in-memory transports): one SDK session per process.
		c.Key, c.Transport, c.PrincipalID = fmt.Sprintf("stdio:%p", ss), "stdio", s.defaultPrincipal.ID
	}
	return s.opt.Sessions.Resolve(ctx, c)
}

// clientInfoOf finds the client's identity wherever the protocol version put
// it: in the initialize request itself, in the per-request _meta of a
// 2026-07-28 client (whose first call, server/discover, precedes any session
// state), or in the session's stored initialize parameters.
func clientInfoOf(req mcp.Request, ss *mcp.ServerSession) *mcp.Implementation {
	params := req.GetParams()
	if v := reflect.ValueOf(params); params == nil || v.Kind() == reflect.Pointer && v.IsNil() {
		params = nil
	}
	if p, ok := params.(*mcp.InitializeParams); ok && p.ClientInfo != nil {
		return p.ClientInfo
	}
	if params != nil {
		switch v := params.GetMeta()[mcp.MetaKeyClientInfo].(type) {
		case *mcp.Implementation:
			return v
		case map[string]any:
			name, _ := v["name"].(string)
			version, _ := v["version"].(string)
			if name != "" {
				return &mcp.Implementation{Name: name, Version: version}
			}
		}
	}
	if ss != nil {
		if p := ss.InitializeParams(); p != nil {
			return p.ClientInfo
		}
	}
	return nil
}

// scopeOf returns the scope a call runs under. With a session store every
// call must have passed sessionMiddleware; a call that did not gets a scope
// that refuses everything rather than the process-wide fallback, so a
// missing middleware shows up as a denial instead of a silent widening.
func (s *Server) scopeOf(ctx context.Context) agent.Scope {
	if sess, ok := agent.FromContext(ctx); ok {
		return sess.Scope
	}
	if s.opt.Sessions != nil {
		return agent.Scope{ReadOnly: true, ExpiresAt: time.Unix(1, 0)}
	}
	return s.defaultScope
}

// checkPath cleans p and checks it against the caller's scope for reading
// or writing. Cleaning also defeats "..\" traversal attempts.
func (s *Server) checkPath(ctx context.Context, p string, write bool) (string, error) {
	return s.scopeOf(ctx).Check(p, write)
}

// checkWrite is the path-free half of a write check, for tools that mutate
// server state by id rather than by path.
func (s *Server) checkWrite(ctx context.Context) error {
	return s.scopeOf(ctx).CheckWriteAllowed(time.Now())
}

// unrestricted reports whether the caller may read the whole mount, which
// the whole-queue upload tools require.
func (s *Server) unrestricted(ctx context.Context) bool {
	for _, r := range s.scopeOf(ctx).EffectiveRead() {
		if r == "/" {
			return true
		}
	}
	return false
}

// readRoots intersects the caller's read scope with a requested subtree, so
// a search below root only visits directories the caller may read. A root
// inside the scope yields itself; a root above the scope yields the scope's
// own prefixes; a root beside it yields nothing.
func (s *Server) readRoots(ctx context.Context, root string) []string {
	roots := []string{}
	for _, allowed := range s.scopeOf(ctx).EffectiveRead() {
		switch {
		case agent.Under(root, allowed):
			roots = append(roots, root)
		case agent.Under(allowed, root):
			roots = append(roots, allowed)
		}
	}
	return roots
}
