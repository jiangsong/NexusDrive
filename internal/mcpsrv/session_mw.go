package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionMiddleware gives every incoming call a CloudFS session, resolved
// from the connection it arrived on, so that path checks read the session's
// scope rather than one process-wide allowlist. Session resolution is a
// no-op when no session store is configured; the origin tag is not, so a
// change made by any tool call is reported as the agent's (vfs.OriginAPI)
// and a trigger rule can leave the agent's own writes out.
func (s *Server) sessionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		ctx = vfs.WithOrigin(ctx, "mcp")
		if s.opt.Sessions == nil {
			return next(ctx, method, req)
		}
		sess, err := s.resolveSession(ctx, req)
		if err != nil {
			return nil, err
		}
		// Changes this call makes are attributed to the session, so the
		// record of who changed a file needs no join with the audit trail.
		ctx = vfs.WithActor(ctx, sess.ID, sess.PrincipalID)
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
		pid := extra.TokenInfo.UserID
		if pid == bridgePrincipalID {
			// A stdio server beside this owner, forwarding for its agent:
			// the default principal, under a transport name of its own so
			// the console can tell a bridged call from a token's. The
			// connection is the stdio process (its header names it), so two
			// agents beside one mount never share a session; the scope is the
			// stdio server's own, so the owner grants nothing that server
			// would have refused.
			conn, narrow, err := bridgeConnOf(extra.Header)
			if err != nil {
				return agent.Session{}, err
			}
			c.Key, c.Transport, c.PrincipalID, c.Narrow = "bridge:"+conn, "http-bridge", s.defaultPrincipal.ID, &narrow
			sess, err := s.opt.Sessions.Resolve(ctx, c)
			if err != nil {
				return agent.Session{}, err
			}
			sess.Scope = sess.Scope.Narrow(narrow)
			return sess, nil
		}
		if pid == envPrincipalID {
			// The legacy environment token has no row of its own until it is
			// first used; it runs under the process-wide scope like stdio.
			p, err := s.envPrincipal(ctx)
			if err != nil {
				return agent.Session{}, err
			}
			pid = p.ID
		}
		c.Key, c.Transport, c.PrincipalID = "token:"+pid, "http-token", pid
		if ss != nil && ss.ID() != "" {
			// A stateful SDK session outlives the request that authenticated
			// it, so remember which principal it belongs to: revoking that
			// principal's token must close it, not just refuse its next call.
			s.rememberLegacySession(pid, ss)
		}
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
		s.rememberStdioKey(c.Key)
	}
	return s.opt.Sessions.Resolve(ctx, c)
}

// bridgeConnOf reads the stdio process's connection id and scope from a
// bridged request's headers. Both are required: a bridge that names no
// connection would fold every stdio process into one session, and one
// that sends no scope would run under the owner's full scope.
func bridgeConnOf(h http.Header) (conn string, narrow agent.Scope, err error) {
	if h == nil {
		return "", agent.Scope{}, errors.New("bridge: request carries no headers")
	}
	conn = h.Get(bridgeConnHeader)
	if conn == "" || len(conn) > 64 {
		return "", agent.Scope{}, errors.New("bridge: " + bridgeConnHeader + " header is required")
	}
	raw := h.Get(bridgeScopeHeader)
	if raw == "" {
		return "", agent.Scope{}, errors.New("bridge: " + bridgeScopeHeader + " header is required")
	}
	if err := json.Unmarshal([]byte(raw), &narrow); err != nil {
		return "", agent.Scope{}, fmt.Errorf("bridge: %s: %w", bridgeScopeHeader, err)
	}
	return conn, narrow, nil
}

func (s *Server) rememberStdioKey(key string) {
	s.principalsMu.Lock()
	defer s.principalsMu.Unlock()
	if s.stdioKeys == nil {
		s.stdioKeys = map[string]struct{}{}
	}
	s.stdioKeys[key] = struct{}{}
}

// FinishStdioSessions finishes the CloudFS session of every stdio
// connection this server resolved one for, and reports how many it closed.
// A stdio process is its session: `cloudfs mcp` calls this when its
// transport ends, so the console does not show the run as active until an
// idle sweep that stdio sessions, which never rotate, would not get.
func (s *Server) FinishStdioSessions(ctx context.Context) int {
	if s.opt.Sessions == nil {
		return 0
	}
	s.principalsMu.Lock()
	keys := make([]string, 0, len(s.stdioKeys))
	for k := range s.stdioKeys {
		keys = append(keys, k)
	}
	s.principalsMu.Unlock()
	n := 0
	for _, k := range keys {
		if _, ok, err := s.opt.Sessions.FinishConn(ctx, k, ""); err == nil && ok {
			n++
		}
	}
	return n
}

// envPrincipal returns the principal behind the legacy environment token,
// created on first use with the process-wide scope and cached for the life
// of the server.
func (s *Server) envPrincipal(ctx context.Context) (agent.Principal, error) {
	s.principalsMu.Lock()
	defer s.principalsMu.Unlock()
	if s.envPrincipalCached != nil {
		return *s.envPrincipalCached, nil
	}
	p, err := s.opt.Sessions.EnsurePrincipal(ctx, "env", "env", s.defaultScope)
	if err != nil {
		return agent.Principal{}, fmt.Errorf("mcpsrv: env principal: %w", err)
	}
	s.envPrincipalCached = &p
	return p, nil
}

// rememberLegacySession records that a stateful SDK session was
// authenticated as principalID.
func (s *Server) rememberLegacySession(principalID string, ss *mcp.ServerSession) {
	s.principalsMu.Lock()
	defer s.principalsMu.Unlock()
	if s.legacyByPrincipal == nil {
		s.legacyByPrincipal = map[string]map[*mcp.ServerSession]struct{}{}
	}
	if s.legacyByPrincipal[principalID] == nil {
		s.legacyByPrincipal[principalID] = map[*mcp.ServerSession]struct{}{}
	}
	s.legacyByPrincipal[principalID][ss] = struct{}{}
}

// CloseSessionsOf closes every stateful SDK session that authenticated as
// the given principal and reports how many it closed. Stateless requests
// need nothing here: each one re-verifies its token.
func (s *Server) CloseSessionsOf(principalID string) int {
	s.principalsMu.Lock()
	sessions := s.legacyByPrincipal[principalID]
	delete(s.legacyByPrincipal, principalID)
	s.principalsMu.Unlock()
	for ss := range sessions {
		_ = ss.Close()
	}
	return len(sessions)
}

// pruneLegacySessions forgets SDK sessions that have already ended, so the
// map only ever holds what the SDK still lists.
func (s *Server) pruneLegacySessions() {
	live := map[*mcp.ServerSession]struct{}{}
	for ss := range s.mcp.Sessions() {
		live[ss] = struct{}{}
	}
	s.principalsMu.Lock()
	defer s.principalsMu.Unlock()
	for pid, sessions := range s.legacyByPrincipal {
		for ss := range sessions {
			if _, ok := live[ss]; !ok {
				delete(sessions, ss)
			}
		}
		if len(sessions) == 0 {
			delete(s.legacyByPrincipal, pid)
		}
	}
}

// watchRevocations polls the store for principals revoked since the last
// look and closes their sessions, so a token revoked by the CLI or the
// console in another process stops working within one poll interval even
// on a stateful connection.
func (s *Server) watchRevocations(interval time.Duration) {
	defer s.revokeWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	since := time.Now()
	for {
		select {
		case <-s.revokeStop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		ids, err := s.opt.Sessions.Store().RevokedSince(context.Background(), since)
		if err != nil {
			continue
		}
		since = now
		for _, id := range ids {
			s.CloseSessionsOf(id)
		}
		s.pruneLegacySessions()
	}
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
// or writing. Cleaning also defeats "..\" traversal attempts. The outcome
// is noted for the audit row: the path either way, a refusal as denied.
func (s *Server) checkPath(ctx context.Context, p string, write bool) (string, error) {
	clean, err := s.scopeOf(ctx).Check(p, write)
	if err != nil {
		recordCheck(ctx, agent.Normalise(p), err)
		return "", err
	}
	recordCheck(ctx, clean, nil)
	return clean, nil
}

// checkWrite is the path-free half of a write check, for tools that mutate
// server state by id rather than by path.
func (s *Server) checkWrite(ctx context.Context) error {
	err := s.scopeOf(ctx).CheckWriteAllowed(time.Now())
	recordCheck(ctx, "", err)
	return err
}

// visible reports whether the caller may read p. It is for tools that
// filter a listing by scope rather than act on a path the caller named, so
// it leaves nothing on the audit row: an entry hidden from a search result
// or a directory page is not a refusal.
func (s *Server) visible(ctx context.Context, p string) bool {
	_, err := s.scopeOf(ctx).Check(p, false)
	return err == nil
}

// visibleNow is visible against the session as the store holds it now,
// not as the context captured it: a subscription is reserved once and
// delivered for as long as the client stays, and in between the session
// may have been finished, expired or rolled back. Without a session layer
// the scope is the process's and cannot change.
func (s *Server) visibleNow(ctx context.Context, p string) bool {
	sess, ok := agent.FromContext(ctx)
	if !ok || s.opt.Sessions == nil {
		return s.visible(ctx, p)
	}
	current, err := s.opt.Sessions.Get(ctx, sess.ID)
	if err != nil || current.State != "active" {
		return false
	}
	_, err = current.Scope.Check(p, false)
	return err == nil
}

// visibleFrom is the source path of a rename as the caller may see it:
// itself when readable, "" when it lies outside the caller's scope, so a
// change feed shows that a file arrived without naming where from.
func (s *Server) visibleFrom(ctx context.Context, from string) string {
	if from == "" || !s.visible(ctx, from) {
		return ""
	}
	return from
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
