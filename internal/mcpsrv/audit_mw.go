package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AuditWriter is where the server records calls. agent.Store satisfies it;
// tests substitute a failing one.
type AuditWriter interface {
	AppendAudit(ctx context.Context, row agent.AuditRow) (int64, error)
}

// maxAuditPaths bounds the paths one row lists; stat_many takes at most 100.
const maxAuditPaths = 128

// maxAuditError bounds the error text kept with a row.
const maxAuditError = 512

// callNote collects, while a call runs, what the path checks decided, so
// the audit row can name the paths and say whether the call was refused
// without every tool reporting back by hand.
type callNote struct {
	mu     sync.Mutex
	paths  []string
	denied bool
}

type callNoteKey struct{}

func noteFrom(ctx context.Context) *callNote {
	n, _ := ctx.Value(callNoteKey{}).(*callNote)
	return n
}

// recordCheck is called by checkPath and checkWrite with what they decided.
// A refusal marks the call denied; the path is kept either way, deduplicated
// and capped, so a denied row names what was asked for.
func recordCheck(ctx context.Context, p string, err error) {
	n := noteFrom(ctx)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if errors.Is(err, agent.ErrDenied) || errors.Is(err, agent.ErrReadOnly) || errors.Is(err, agent.ErrExpired) {
		n.denied = true
	}
	if p == "" || len(n.paths) >= maxAuditPaths {
		return
	}
	for _, have := range n.paths {
		if have == p {
			return
		}
	}
	n.paths = append(n.paths, p)
}

// audited reports whether a method leaves an audit row: every tool call,
// both handshakes, and both ways of registering a resource subscription.
func audited(method string) bool {
	switch method {
	case "tools/call", "initialize", "server/discover", "resources/subscribe", "subscriptions/listen":
		return true
	}
	return false
}

// auditMiddleware records one row per audited call. It sits inside
// sessionMiddleware, so the session is already in the context, and outside
// the subscription reservation, so the paths a subscription names are
// checked with the note in place. A subscriptions/listen stream is one call
// that lasts as long as the stream: its row lands when the stream ends (a
// refused one at once), and a catalog-only listen that names no resource
// leaves no row.
func (s *Server) auditMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if s.audit == nil || !audited(method) {
			return next(ctx, method, req)
		}
		row, skip := s.auditRowFor(method, req)
		if skip {
			return next(ctx, method, req)
		}
		note := &callNote{}
		start := time.Now()
		res, err := next(context.WithValue(ctx, callNoteKey{}, note), method, req)
		row.TS, row.DurationMS = start, time.Since(start).Milliseconds()
		if sess, ok := agent.FromContext(ctx); ok {
			row.SessionID, row.PrincipalID, row.Transport = sess.ID, sess.PrincipalID, sess.Transport
		}
		note.mu.Lock()
		row.Paths, row.Result = append([]string{}, note.paths...), "ok"
		denied := note.denied
		note.mu.Unlock()
		call, _ := res.(*mcp.CallToolResult)
		switch {
		case denied:
			row.Result = "denied"
			row.Error = auditError(err, call)
		case err != nil:
			row.Result, row.Error = "error", truncateError(err.Error())
		case call != nil && call.IsError:
			row.Result, row.Error = "error", truncateError(firstText(call))
		}
		if call != nil {
			row.BytesOut = contentBytes(call)
		}
		if _, werr := s.audit.AppendAudit(context.WithoutCancel(ctx), row); werr != nil {
			slog.Warn("mcp audit write failed", "tool", row.Tool, "err", werr)
		}
		return res, err
	}
}

// auditRowFor builds the part of a row known before the call runs: the
// tool name, the redacted arguments and the bytes of content received.
func (s *Server) auditRowFor(method string, req mcp.Request) (agent.AuditRow, bool) {
	row := agent.AuditRow{Tool: method, Args: json.RawMessage(`{}`)}
	switch q := req.(type) {
	case *mcp.CallToolRequest:
		if q.Params == nil {
			return row, false
		}
		row.Tool = q.Params.Name
		row.Args, row.BytesIn = agent.RedactArgs(q.Params.Arguments)
	case *mcp.SubscribeRequest:
		if q.Params != nil {
			row.Args = urisJSON([]string{q.Params.URI})
		}
	case *mcp.SubscriptionsListenRequest:
		if q.Params == nil || q.Params.Notifications == nil || len(q.Params.Notifications.ResourceSubscriptions) == 0 {
			return row, true
		}
		row.Args = urisJSON(q.Params.Notifications.ResourceSubscriptions)
	default:
		// initialize and server/discover: keep the client's identity, which
		// is what a console wants to see, not the capability handshake.
		ss, _ := req.GetSession().(*mcp.ServerSession)
		if info := clientInfoOf(req, ss); info != nil {
			row.Args, _ = json.Marshal(map[string]string{"client": info.Name, "version": info.Version})
		}
	}
	return row, false
}

// urisJSON is the argument object of a subscription row. cloudfs:// URIs
// are the mount's own names, not download links, so they are kept as they
// are rather than passed through RedactArgs.
func urisJSON(uris []string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"uris": uris})
	return b
}

// auditError is the text kept with a denied row: the tool's own message
// when it returned one, otherwise the transport error.
func auditError(err error, call *mcp.CallToolResult) string {
	if call != nil {
		return truncateError(firstText(call))
	}
	if err != nil {
		return truncateError(err.Error())
	}
	return ""
}

func truncateError(s string) string {
	if len(s) > maxAuditError {
		return s[:maxAuditError]
	}
	return s
}

// firstText is the first text block of a result, which is where fail puts
// the message.
func firstText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// contentBytes is what a result cost the client's context: every text and
// blob block plus the structured output, both of which go over the wire.
func contentBytes(r *mcp.CallToolResult) int64 {
	var n int64
	for _, c := range r.Content {
		switch x := c.(type) {
		case *mcp.TextContent:
			n += int64(len(x.Text))
		case *mcp.ImageContent:
			n += int64(len(x.Data))
		case *mcp.AudioContent:
			n += int64(len(x.Data))
		case *mcp.EmbeddedResource:
			if x.Resource != nil {
				n += int64(len(x.Resource.Text) + len(x.Resource.Blob))
			}
		}
	}
	if r.StructuredContent != nil {
		if b, err := json.Marshal(r.StructuredContent); err == nil {
			n += int64(len(b))
		}
	}
	return n
}
