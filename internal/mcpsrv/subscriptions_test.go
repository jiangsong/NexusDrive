package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type subscriptionClient struct {
	session *mcp.ClientSession
	updates chan *mcp.ResourceUpdatedNotificationParams
	acks    chan *mcp.SubscriptionsAcknowledgedParams
}

func newSubscriptionClient(t *testing.T, server *Server, transports ...mcp.Transport) *subscriptionClient {
	t.Helper()
	c := &subscriptionClient{updates: make(chan *mcp.ResourceUpdatedNotificationParams, 64), acks: make(chan *mcp.SubscriptionsAcknowledgedParams, 64)}
	client := mcp.NewClient(&mcp.Implementation{Name: "subscription-test", Version: "1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) { c.updates <- req.Params },
	})
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if method == "notifications/subscriptions/acknowledged" {
				params := req.GetParams().(*mcp.SubscriptionsAcknowledgedParams)
				if len(params.Notifications.ResourceSubscriptions) != 0 {
					c.acks <- params
				}
			}
			return result, err
		}
	})
	var clientT mcp.Transport
	var serverSession *mcp.ServerSession
	var err error
	if len(transports) != 0 {
		clientT = transports[0]
	} else {
		var serverT mcp.Transport
		clientT, serverT = mcp.NewInMemoryTransports()
		serverSession, err = server.MCP().Connect(context.Background(), serverT, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	var cancel context.CancelFunc
	ctx, cancel := context.WithCancel(context.Background())
	c.session, err = client.Connect(ctx, clientT, nil)
	if err != nil {
		cancel()
		if serverSession != nil {
			serverSession.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.session.Close()
		cancel()
		if serverSession != nil {
			serverSession.Close()
		}
	})
	return c
}

func TestResourceSubscriptionsHTTPUnsubscribeReleasesIdleStream(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/file", []byte("old"))
	if _, err := e.fs.StatPath(context.Background(), "/work/file"); err != nil {
		t.Fatal(err)
	}
	handler := newMCPHTTPHandler(e.server)
	httpServer := httptest.NewServer(requireBearer(handler, "resource-test-token"))
	t.Cleanup(httpServer.Close)
	// Close MCP before the HTTP server waits for outstanding streaming requests.
	t.Cleanup(func() { e.server.Close() })
	client := newSubscriptionClient(t, e.server, &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           &http.Client{Transport: resourceAuthTransport{base: http.DefaultTransport}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	})
	uri := "cloudfs://ali/work/file"
	id := client.subscribe(t, uri)
	waitSubscriptionCount(t, e.server, 1)
	// A separate cancellation notification cannot target this live POST,
	// even if its request ID is known. Only closing its response cancels it.
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
	req, _ := http.NewRequest(http.MethodPost, httpServer.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer resource-test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redundant cancellation status=%d", resp.StatusCode)
	}
	waitSubscriptionCount(t, e.server, 1)
	if _, err := e.fs.WriteFile(context.Background(), "/work/file", []byte("HTTP notification"), false); err != nil {
		t.Fatal(err)
	}
	client.update(t, uri, id)
	client.quiet(t)
	if err := client.session.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	// No file writes or notification attempts may be required for cleanup.
	waitSubscriptionCount(t, e.server, 0)
	if _, err := client.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri}); err != nil {
		t.Fatalf("client connection after unsubscribe: %v", err)
	}
	client.subscribe(t, uri)
	waitSubscriptionCount(t, e.server, 1)
	client.session.Close()
	waitSubscriptionCount(t, e.server, 0)
}

func (c *subscriptionClient) subscribe(t *testing.T, uri string) any {
	t.Helper()
	if err := c.session.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	return c.acknowledged(t, uri)
}

func (c *subscriptionClient) acknowledged(t *testing.T, uri string) any {
	t.Helper()
	select {
	case ack := <-c.acks:
		if len(ack.Notifications.ResourceSubscriptions) != 1 || ack.Notifications.ResourceSubscriptions[0] != uri {
			t.Fatalf("unexpected subscription acknowledgment: %+v", ack)
		}
		return ack.Meta[mcp.MetaKeySubscriptionID]
	case <-time.After(3 * time.Second):
		t.Fatal("subscription was not acknowledged")
		return nil
	}
}

func TestResourceSubscriptionsWaitForAcknowledgmentPerSession(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	ctx := context.Background()
	e.fake.Seed("work/file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/work/file"); err != nil {
		t.Fatal(err)
	}
	uri := "cloudfs://ali/work/file"
	a := newSubscriptionClient(t, e.server)
	aID := a.subscribe(t, uri)
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	e.server.MCP().AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "notifications/subscriptions/acknowledged" {
				params := req.GetParams().(*mcp.SubscriptionsAcknowledgedParams)
				if len(params.Notifications.ResourceSubscriptions) != 0 {
					close(blocked)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			return next(ctx, method, req)
		}
	})
	b := newSubscriptionClient(t, e.server)
	if err := b.session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("second acknowledgment did not reach barrier")
	}
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("changed"), false); err != nil {
		t.Fatal(err)
	}
	a.update(t, uri, aID)
	b.quiet(t)
	once.Do(func() { close(release) })
	bID := b.acknowledged(t, uri)
	b.update(t, uri, bID)
	a.quiet(t)
	b.quiet(t)
}

func TestResourceSubscriptionsRejectInvalidBatchesAtomically(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	_, transport := mcp.NewInMemoryTransports()
	session, err := e.server.MCP().Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	valid := "cloudfs://ali/work/allowed"
	oversized := make([]string, maxResourceSessionSubscriptions+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("cloudfs://ali/work/%d", i)
	}
	for _, uris := range [][]string{
		{valid, "cloudfs://ali/private/secret"},
		{valid, "cloudfs://other/work/secret"},
		{valid, "cloudfs://ali/work/../secret"},
		{valid, "cloudfs://ali/work/file?unknown=yes"},
		{valid, valid}, oversized,
	} {
		called := false
		handler := e.server.subscriptions.receive(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			called = true
			return nil, nil
		})
		_, err := handler(context.Background(), "subscriptions/listen", &mcp.SubscriptionsListenRequest{
			Session: session,
			Params:  &mcp.SubscriptionsListenParams{Notifications: &mcp.NotificationSubscriptions{ResourceSubscriptions: uris}},
		})
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams || called {
			t.Fatalf("invalid batch reached SDK or wrong error: called=%v err=%v", called, err)
		}
		waitSubscriptionCount(t, e.server, 0)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("rejected subscriptions accessed provider")
	}
}

func TestResourceSubscriptionsEnforceSessionAndGlobalLimits(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	r := &e.server.subscriptions
	makeToken := func() *subscriptionToken {
		_, transport := mcp.NewInMemoryTransports()
		session, err := e.server.MCP().Connect(context.Background(), transport, nil)
		if err != nil {
			t.Fatal(err)
		}
		return &subscriptionToken{session: session, modern: true, cancel: func() {}}
	}
	uris := make([]string, maxResourceSessionSubscriptions)
	for i := range uris {
		uris[i] = fmt.Sprintf("cloudfs://ali/work/%d", i)
	}
	var tokens []*subscriptionToken
	for i := 0; i < maxResourceSubscriptions/maxResourceSessionSubscriptions; i++ {
		token := makeToken()
		if _, err := r.reserve(token, uris); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if _, err := r.reserve(tokens[0], []string{"cloudfs://ali/work/extra"}); err == nil {
		t.Fatal("per-session limit was not enforced")
	}
	next := makeToken()
	if _, err := r.reserve(next, uris[:1]); err == nil {
		t.Fatal("global URI limit was not enforced")
	}
	r.release(tokens[0])
	if _, err := r.reserve(next, uris); err != nil {
		t.Fatalf("released quota was not reusable: %v", err)
	}
	for _, token := range append(tokens, next) {
		r.release(token)
	}
	// Empty tracked sessions still have Wait goroutines. Bound those too.
	for i := len(tokens) + 1; i < maxResourceSubscriptionSessions; i++ {
		if _, err := r.reserve(makeToken(), uris[:1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.reserve(makeToken(), uris[:1]); err == nil {
		t.Fatal("tracked session limit was not enforced")
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("subscription admission accessed provider")
	}
}

func TestResourceSubscriptionsNamespaceChanges(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	ctx := context.Background()
	e.fake.Seed("work/dir/file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/work/dir/file"); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	c := newSubscriptionClient(t, e.server)
	uris := []string{"cloudfs://ali/work", "cloudfs://ali/work/dir/file", "cloudfs://ali/work/renamed/file?offset=0&length=1"}
	ids := map[string]any{}
	for _, uri := range uris {
		ids[uri] = c.subscribe(t, uri)
	}
	collect := func(want []string) {
		t.Helper()
		pending := map[string]bool{}
		for _, uri := range want {
			pending[uri] = true
		}
		deadline := time.After(3 * time.Second)
		for len(pending) != 0 {
			select {
			case event := <-c.updates:
				if !pending[event.URI] || fmt.Sprint(event.Meta[mcp.MetaKeySubscriptionID]) != fmt.Sprint(ids[event.URI]) {
					t.Fatalf("unexpected namespace update: %+v", event)
				}
				delete(pending, event.URI)
			case <-deadline:
				t.Fatalf("missing namespace notifications: %v", pending)
			}
		}
		c.quiet(t)
	}
	if err := e.fs.Rename(ctx, root.Ino, "dir", root.Ino, "renamed"); err != nil {
		t.Fatal(err)
	}
	collect(uris)
	if err := e.fs.Remove(ctx, root.Ino, "renamed", true); err != nil {
		t.Fatal(err)
	}
	collect([]string{uris[0], uris[2]})
	uri := "cloudfs://ali/work/future"
	ids[uri] = c.subscribe(t, uri)
	if _, err := e.fs.Mkdir(ctx, root.Ino, "future"); err != nil {
		t.Fatal(err)
	}
	collect([]string{uris[0], uri})
}

func TestResourceSubscriptionsServerCloseCancelsCatalogStream(t *testing.T) {
	e := newEnv(t, Options{})
	client := mcp.NewClient(&mcp.Implementation{Name: "catalog-test", Version: "1"}, &mcp.ClientOptions{
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {},
	})
	ack := make(chan struct{}, 1)
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "notifications/subscriptions/acknowledged" {
				ack <- struct{}{}
			}
			return next(ctx, method, req)
		}
	})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := e.server.MCP().Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog subscription did not start")
	}
	done := make(chan struct{})
	go func() { e.server.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server close blocked on catalog-only listen")
	}
}

func (c *subscriptionClient) update(t *testing.T, uri string, subscriptionID any) {
	t.Helper()
	select {
	case event := <-c.updates:
		if event.URI != uri || fmt.Sprint(event.Meta[mcp.MetaKeySubscriptionID]) != fmt.Sprint(subscriptionID) {
			t.Fatalf("wrong notification or correlation: %+v want URI=%s id=%v", event, uri, subscriptionID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resource change was not delivered")
	}
}

func (c *subscriptionClient) quiet(t *testing.T) {
	t.Helper()
	select {
	case event := <-c.updates:
		t.Fatalf("unexpected resource notification: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitSubscriptionCount(t *testing.T, s *Server, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.subscriptions.mu.Lock()
		got := s.subscriptions.count
		s.subscriptions.mu.Unlock()
		if got == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscription count=%d want=%d", got, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResourceSubscriptionsDeliverLocalAndRemoteChanges(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, ReadOnly: true})
	ctx := context.Background()
	e.fake.Seed("work/file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/work/file"); err != nil {
		t.Fatal(err)
	}
	c := newSubscriptionClient(t, e.server)
	uri := "cloudfs://ali/work/file"
	id := c.subscribe(t, uri)
	before := e.fake.TotalCalls()
	if _, err := e.fs.WriteFile(vfs.FromKernel(ctx), "/work/file", []byte("local"), false); err != nil {
		t.Fatal(err)
	}
	c.update(t, uri, id)
	c.quiet(t)
	if e.fake.TotalCalls() != before {
		t.Fatal("resource subscription downloaded or polled")
	}
	if err := c.session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	waitSubscriptionCount(t, e.server, 0)
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("after cancellation"), false); err != nil {
		t.Fatal(err)
	}
	c.quiet(t)

	// Use a clean remote version to exercise the actual delta path.
	e.fake.Seed("work/remote", []byte("old remote"))
	root, err := e.fs.StatPath(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Meta().SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	uri = "cloudfs://ali/work/remote"
	id = c.subscribe(t, uri)
	e.fake.Seed("work/remote", []byte("new remote"))
	refresher := vfs.NewRefresher(e.fs, time.Hour)
	if _, err := refresher.PollOnce(ctx, e.fs.Mounts()[0]); err != nil {
		t.Fatal(err)
	}
	c.update(t, uri, id)
	if _, err := refresher.PollOnce(ctx, e.fs.Mounts()[0]); err != nil {
		t.Fatal(err)
	}
	c.quiet(t)
}

func TestResourceSubscriptionsIsolateSessionsAndReleaseDisconnects(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/file", []byte("old"))
	ctx := context.Background()
	if _, err := e.fs.StatPath(ctx, "/work/file"); err != nil {
		t.Fatal(err)
	}
	a, b := newSubscriptionClient(t, e.server), newSubscriptionClient(t, e.server)
	uri := "cloudfs://ali/work/file"
	aID, bID := a.subscribe(t, uri), b.subscribe(t, uri)
	waitSubscriptionCount(t, e.server, 2)
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("both"), false); err != nil {
		t.Fatal(err)
	}
	a.update(t, uri, aID)
	b.update(t, uri, bID)
	a.quiet(t)
	b.quiet(t)
	a.session.Close()
	waitSubscriptionCount(t, e.server, 1)
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("only b"), false); err != nil {
		t.Fatal(err)
	}
	b.update(t, uri, bID)
	a.quiet(t)
	b.session.Close()
	waitSubscriptionCount(t, e.server, 0)
}
