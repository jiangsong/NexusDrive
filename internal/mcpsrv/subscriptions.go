package mcpsrv

import (
	"context"
	"errors"
	"sync"
	"time"

	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxResourceSessionSubscriptions = 128
const maxResourceSubscriptionSessions = 64
const maxResourceSubscriptions = 4096

type subscriptionToken struct {
	session *mcp.ServerSession
	modern  bool
	cancel  context.CancelFunc
}

type subscriptionContextKey struct{}

type resourceWatch struct {
	path  string
	token *subscriptionToken
	ready bool
	dirty bool
}

type resourceSubscriptions struct {
	server *Server
	// Serializes legacy subscribe/unsubscribe through the SDK's map update.
	// Modern streams reserve their URIs until all SDK cleanup has completed.
	registrationMu sync.Mutex
	mu             sync.Mutex
	sessions       map[*mcp.ServerSession]map[string]*resourceWatch
	streams        map[*subscriptionToken]struct{}
	count          int
	closed         bool
	running        bool
	unwatch        func()
	done           chan struct{}
	wake           chan struct{}
	wg             sync.WaitGroup
}

func (r *resourceSubscriptions) init(server *Server) {
	r.server = server
	r.sessions = make(map[*mcp.ServerSession]map[string]*resourceWatch)
	r.streams = make(map[*subscriptionToken]struct{})
	r.done, r.wake = make(chan struct{}), make(chan struct{}, 1)
}

func subscriptionError(message string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: message}
}

// Subscribe does not stat or download. A permitted path can be watched before
// creation and after deletion; namespace creation will invalidate that URI.
func (r *resourceSubscriptions) reserve(token *subscriptionToken, uris []string) (bool, error) {
	if token.session == nil || len(uris) == 0 || len(uris) > maxResourceSessionSubscriptions {
		return false, subscriptionError("resource subscription requires a session and at most 128 URIs")
	}
	paths := make(map[string]string, len(uris))
	for _, uri := range uris {
		q, err := r.server.parseResource(uri)
		if err != nil {
			return false, subscriptionError("invalid or inaccessible resource subscription")
		}
		if _, duplicate := paths[uri]; duplicate {
			return false, subscriptionError("duplicate resource URI in subscription stream")
		}
		paths[uri] = q.path
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, errors.New("resource subscriptions are closed")
	}
	watches, tracked := r.sessions[token.session]
	for uri := range paths {
		if previous := watches[uri]; previous != nil {
			if !token.modern && !previous.token.modern && len(paths) == 1 {
				return false, nil // Legacy repeat subscribe is idempotent.
			}
			// The SDK indexes one request ID per session/URI. Overlapping
			// listen streams would otherwise cancel one another's delivery.
			return false, subscriptionError("resource already subscribed; cancel the existing stream first")
		}
	}
	if len(watches)+len(paths) > maxResourceSessionSubscriptions || r.count+len(paths) > maxResourceSubscriptions || !tracked && len(r.sessions) >= maxResourceSubscriptionSessions {
		return false, subscriptionError("resource subscription limit reached")
	}
	if !tracked {
		watches = make(map[string]*resourceWatch)
		r.sessions[token.session] = watches
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			_ = token.session.Wait()
			r.mu.Lock()
			for _, w := range r.sessions[token.session] {
				w.token.cancel()
				r.count--
			}
			delete(r.sessions, token.session)
			r.mu.Unlock()
		}()
	}
	if !r.running {
		changes, cancel := r.server.opt.FS.WatchChanges()
		r.unwatch, r.running = cancel, true
		r.wg.Add(1)
		go r.run(changes)
	}
	for uri, p := range paths {
		watches[uri] = &resourceWatch{path: p, token: token}
		r.count++
	}
	return true, nil
}

func (r *resourceSubscriptions) release(token *subscriptionToken) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, token)
	for uri, w := range r.sessions[token.session] {
		if w.token == token {
			delete(r.sessions[token.session], uri)
			r.count--
		}
	}
}

func (r *resourceSubscriptions) activate(token *subscriptionToken) {
	if token == nil {
		return
	}
	r.mu.Lock()
	for _, w := range r.sessions[token.session] {
		if w.token == token {
			w.ready = true
		}
	}
	r.mu.Unlock()
	r.signal()
}

func (r *resourceSubscriptions) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *resourceSubscriptions) subscribeHook(ctx context.Context, req *mcp.SubscribeRequest) error {
	token, _ := ctx.Value(subscriptionContextKey{}).(*subscriptionToken)
	if token == nil || req.Params == nil {
		return subscriptionError("subscription must pass the resource authorization boundary")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.sessions[req.Session][req.Params.URI]
	if r.closed || w == nil || w.token != token && (token.modern || w.token.modern) {
		return subscriptionError("resource subscription reservation changed")
	}
	return nil
}

// SDK cleanup calls this with a cancelled context. The reservation is removed
// by receive only AFTER the SDK has removed its own subscription mapping.
func (r *resourceSubscriptions) unsubscribeHook(context.Context, *mcp.UnsubscribeRequest) error {
	return nil
}

func (r *resourceSubscriptions) receive(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "subscriptions/listen":
			q := req.(*mcp.SubscriptionsListenRequest)
			if q.Params == nil || q.Params.Notifications == nil {
				return nil, subscriptionError("missing notification subscriptions")
			}
			streamCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			token := &subscriptionToken{session: q.Session, modern: true, cancel: cancel}
			// Catalog-only listens also block until cancellation. Track them
			// even though they need neither resource watches nor a VFS stream,
			// otherwise Server.Close waits forever for a client's auto-listen.
			r.mu.Lock()
			if r.closed || len(r.streams) >= maxResourceSubscriptions {
				r.mu.Unlock()
				return nil, subscriptionError("subscription stream limit reached or server closed")
			}
			r.streams[token] = struct{}{}
			r.mu.Unlock()
			defer func() {
				r.registrationMu.Lock()
				r.release(token)
				r.registrationMu.Unlock()
			}()
			if len(q.Params.Notifications.ResourceSubscriptions) != 0 {
				r.registrationMu.Lock()
				_, err := r.reserve(token, q.Params.Notifications.ResourceSubscriptions)
				r.registrationMu.Unlock()
				if err != nil {
					return nil, err
				}
			}
			return next(context.WithValue(streamCtx, subscriptionContextKey{}, token), method, req)
		case "resources/subscribe":
			q := req.(*mcp.SubscribeRequest)
			if q.Params == nil {
				return nil, subscriptionError("missing resource URI")
			}
			r.registrationMu.Lock()
			defer r.registrationMu.Unlock()
			token := &subscriptionToken{session: q.Session, cancel: func() {}}
			fresh, err := r.reserve(token, []string{q.Params.URI})
			if err != nil {
				return nil, err
			}
			out, err := next(context.WithValue(ctx, subscriptionContextKey{}, token), method, req)
			if err != nil && fresh {
				r.release(token)
			} else if err == nil {
				r.activate(token)
			}
			return out, err
		case "resources/unsubscribe":
			q := req.(*mcp.UnsubscribeRequest)
			if q.Params == nil {
				return nil, subscriptionError("missing resource URI")
			}
			r.registrationMu.Lock()
			defer r.registrationMu.Unlock()
			r.mu.Lock()
			w := r.sessions[q.Session][q.Params.URI]
			if w != nil && w.token.modern {
				r.mu.Unlock()
				return nil, subscriptionError("cancel the resource's subscriptions/listen stream instead")
			}
			if w != nil {
				w.ready = false
			}
			r.mu.Unlock()
			out, err := next(ctx, method, req)
			if w != nil {
				if err == nil {
					r.release(w.token)
				} else {
					r.activate(w.token)
				}
			}
			return out, err
		default:
			return next(ctx, method, req)
		}
	}
}

func (r *resourceSubscriptions) send(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/subscriptions/acknowledged" {
			out, err := next(ctx, method, req)
			if err == nil {
				token, _ := ctx.Value(subscriptionContextKey{}).(*subscriptionToken)
				r.activate(token)
			}
			return out, err
		}
		if method != "notifications/resources/updated" {
			return next(ctx, method, req)
		}
		params := req.GetParams().(*mcp.ResourceUpdatedNotificationParams)
		// This is a server-to-client request, whose Session is a server session.
		session, _ := req.GetSession().(*mcp.ServerSession)
		r.mu.Lock()
		w := r.sessions[session][params.URI]
		if r.closed || w == nil || !w.ready || !w.dirty {
			r.mu.Unlock()
			return nil, nil
		}
		w.dirty = false
		r.mu.Unlock()
		out, err := next(ctx, method, req)
		if err != nil {
			r.mu.Lock()
			if r.sessions[session][params.URI] == w {
				w.dirty = true
			}
			r.mu.Unlock()
		}
		return out, err
	}
}

func (r *resourceSubscriptions) run(changes <-chan vfs.Change) {
	defer r.wg.Done()
	defer r.stop()
	// A fixed window coalesces overlapping namespace/version hints without
	// postponing delivery indefinitely under continuous writes.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case change, ok := <-changes:
			if !ok {
				return
			}
			r.mu.Lock()
			for _, watches := range r.sessions {
				for _, w := range watches {
					if change.Affects(w.path) {
						w.dirty = true
					}
				}
			}
			r.mu.Unlock()
		case <-r.wake:
			// Activation wakes the loop; the next fixed tick sends retained
			// changes only after SDK registration/acknowledgment completed.
		case <-ticker.C:
			r.mu.Lock()
			uris := map[string]bool{}
			for _, watches := range r.sessions {
				for uri, w := range watches {
					if w.ready && w.dirty {
						uris[uri] = true
					}
				}
			}
			r.mu.Unlock()
			for uri := range uris {
				select {
				case <-r.done:
					return
				default:
				}
				_ = r.server.mcp.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: uri})
			}
		}
	}
}

func (r *resourceSubscriptions) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	close(r.done)
	if r.unwatch != nil {
		r.unwatch()
	}
	for token := range r.streams {
		token.cancel()
	}
	for _, watches := range r.sessions {
		for _, w := range watches {
			w.token.cancel()
		}
	}
}
