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
	// ctx is the request the reservation was made under; its session scope
	// decides which paths may be watched. nil means the process-wide scope.
	ctx    context.Context
	cancel context.CancelFunc
}

// context returns the request context the token was reserved under.
func (t *subscriptionToken) context() context.Context {
	if t.ctx == nil {
		return context.Background()
	}
	return t.ctx
}

type subscriptionContextKey struct{}

type resourceWatch struct {
	path  string
	token *subscriptionToken
	ready bool
	dirty bool
}

const resourceNotificationTargetKey = "cloudfs.dev/resource-notification-target"

// resourceNotificationTarget is an in-process routing envelope. The SDK's
// public ResourceUpdated API broadcasts, so one sender attaches its intended
// session and the sending middleware drops every other leg before transport.
// The envelope is stripped before JSON encoding and never reaches a client.
type resourceNotificationTarget struct {
	session   *mcp.ServerSession
	delivered bool
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

	// senders carries URIs from the coalescing loop to one sender goroutine
	// per session. The SDK broadcasts a resource update to its subscribers one
	// after another, under a ten-second deadline of its own, so a client whose
	// transport has stalled holds that call. Sending inline would stop this
	// server noticing changes at all while that lasted; sending from one
	// shared goroutine kept detection alive but still made every other
	// subscriber wait in line behind the stuck one. A queue and a goroutine
	// per session make a stall cost only the client that is stalling.
	senders map[*mcp.ServerSession]*sessionSender
	// notify performs one delivery. It is a field so a test can stall one
	// session's transport without stalling the others. Its result reports
	// whether the intended session accepted the notification; a disappeared
	// SDK subscription leaves the watch dirty for a later retry.
	notify func(*mcp.ServerSession, string) bool
}

// sessionSender is one client's delivery queue and the goroutine draining it.
type sessionSender struct {
	uris chan string
	// queued names the URIs already handed to this sender, so a slow send does
	// not accumulate duplicates of the same notification behind it.
	queued map[string]bool
	done   chan struct{}
}

// deliveryQueue bounds what can be waiting on one stalled client; each session
// has its own. Overflow is not a loss: the watch stays dirty and the next tick
// offers it again.
const deliveryQueue = 256

func (r *resourceSubscriptions) init(server *Server) {
	r.server = server
	r.sessions = make(map[*mcp.ServerSession]map[string]*resourceWatch)
	r.streams = make(map[*subscriptionToken]struct{})
	r.done, r.wake = make(chan struct{}), make(chan struct{}, 1)
	r.senders = make(map[*mcp.ServerSession]*sessionSender)
	r.notify = func(session *mcp.ServerSession, uri string) bool {
		target := &resourceNotificationTarget{session: session}
		_ = r.server.mcp.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{
			URI:  uri,
			Meta: mcp.Meta{resourceNotificationTargetKey: target},
		})
		return target.delivered
	}
}

// startSenderLocked gives a session its own queue and sender goroutine. The
// caller holds r.mu.
func (r *resourceSubscriptions) startSenderLocked(session *mcp.ServerSession) {
	if _, ok := r.senders[session]; ok {
		return
	}
	s := &sessionSender{uris: make(chan string, deliveryQueue), queued: map[string]bool{}, done: make(chan struct{})}
	r.senders[session] = s
	r.wg.Add(1)
	go r.deliver(session, s)
}

// stopSenderLocked releases a session's sender. The caller holds r.mu.
func (r *resourceSubscriptions) stopSenderLocked(session *mcp.ServerSession) {
	s, ok := r.senders[session]
	if !ok {
		return
	}
	delete(r.senders, session)
	close(s.done)
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
		q, err := r.server.resourceQueryFor(token.context(), uri)
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
		r.startSenderLocked(token.session)
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
			r.stopSenderLocked(token.session)
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
			token := &subscriptionToken{session: q.Session, modern: true, ctx: streamCtx, cancel: cancel}
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
			token := &subscriptionToken{session: q.Session, ctx: ctx, cancel: func() {}}
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
		target, targeted := params.Meta[resourceNotificationTargetKey].(*resourceNotificationTarget)
		if !targeted {
			return next(ctx, method, req)
		}
		// This is a server-to-client request, whose Session is a server session.
		session, _ := req.GetSession().(*mcp.ServerSession)
		if target.session != session {
			return nil, nil
		}
		r.mu.Lock()
		w := r.sessions[session][params.URI]
		if r.closed || w == nil || !w.ready {
			r.mu.Unlock()
			return nil, nil
		}
		r.mu.Unlock()
		// The SDK also injects its modern listen-request ID into Meta. Copy all
		// of that metadata while removing only our private routing value.
		clean := *params
		clean.Meta = make(mcp.Meta, len(params.Meta)-1)
		for key, value := range params.Meta {
			if key != resourceNotificationTargetKey {
				clean.Meta[key] = value
			}
		}
		out, err := next(ctx, method, &mcp.ServerRequest[*mcp.ResourceUpdatedNotificationParams]{
			Session: session,
			Params:  &clean,
		})
		if err != nil {
			r.mu.Lock()
			if r.sessions[session][params.URI] == w {
				w.dirty = true
			}
			r.mu.Unlock()
		} else {
			target.delivered = true
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
			for session, watches := range r.sessions {
				sender := r.senders[session]
				if sender == nil {
					continue
				}
				for uri, w := range watches {
					if !w.ready || !w.dirty || sender.queued[uri] {
						continue
					}
					select {
					case sender.uris <- uri:
						sender.queued[uri] = true
					case <-r.done:
						r.mu.Unlock()
						return
					default:
						// This session's sender is behind. The watch is still
						// dirty, so the next tick offers the URI again;
						// nothing is dropped, and no other session waits.
					}
				}
			}
			r.mu.Unlock()
		}
	}
}

// deliver sends one session's coalesced notifications. Every session has its
// own, so the cost of a stalled client is bounded to that client's queue: it
// delays neither change detection nor another subscriber's notifications.
func (r *resourceSubscriptions) deliver(session *mcp.ServerSession, sender *sessionSender) {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case <-sender.done:
			return
		case uri := <-sender.uris:
			r.mu.Lock()
			w := r.sessions[session][uri]
			if w == nil || !w.ready || !w.dirty {
				delete(sender.queued, uri)
				r.mu.Unlock()
				continue
			}
			w.dirty = false
			r.mu.Unlock()

			delivered := r.notify(session, uri)

			r.mu.Lock()
			delete(sender.queued, uri)
			if r.sessions[session][uri] == w && !delivered {
				w.dirty = true
			}
			r.mu.Unlock()
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
	for session := range r.senders {
		r.stopSenderLocked(session)
	}
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
