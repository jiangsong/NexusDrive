package control

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A device flow that never shows its QR code is abandoned, but the flow still
// owns cleanup: the HTTP client built for the exchange and the proxy checker
// started alongside it are released by the wait function, not by cancelling the
// context. Returning without calling it leaks both once per timed-out attempt,
// silently, on the one path where a person is already having a bad time.
func TestAnAuthorizationThatNeverShowsItsCodeStillRunsItsCleanup(t *testing.T) {
	restore := authPresentTimeout
	authPresentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { authPresentTimeout = restore })

	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "first", "pan115")
	waited := make(chan struct{})
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "pan115" })
	srv.auth.Device = func(ctx context.Context, name string, present func(string), scanned func()) (func(context.Context) error, error) {
		// The code never arrives — the provider is slow, or wedged.
		return func(waitCtx context.Context) error {
			close(waited)
			<-waitCtx.Done()
			return waitCtx.Err()
		}, nil
	}

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("start: %d %s, want the device-code timeout", rr.Code, rr.Body)
	}
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned flow was never waited on, so nothing it allocated was released")
	}
}

// And the port it held goes back, or one wedged provider stops every other
// account from being added until the daemon restarts.
func TestAnAuthorizationThatNeverShowsItsCodeGivesTheCallbackPortBack(t *testing.T) {
	restore := authPresentTimeout
	authPresentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { authPresentTimeout = restore })

	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "first", "pan115")
	writeConfigLine(t, cfg, "second", "aliyun")
	fa := &fakeAuth{release: make(chan error, 1)}
	oauth := fa.starter(func(s string) bool { return s == "aliyun" || s == "pan115" })
	oauth.Device = func(ctx context.Context, name string, present func(string), scanned func()) (func(context.Context) error, error) {
		return func(waitCtx context.Context) error { <-waitCtx.Done(); return waitCtx.Err() }, nil
	}
	srv.auth = oauth

	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/start", AuthStartRequest{}); rr.Code != http.StatusBadGateway {
		t.Fatalf("start: %d %s, want the device-code timeout", rr.Code, rr.Body)
	}
	deadline := time.After(2 * time.Second)
	for {
		rr := accountRequest(t, srv, http.MethodPost, "/accounts/second/auth/start", AuthStartRequest{})
		if rr.Code == http.StatusOK {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the callback port is still held by the abandoned device flow: %d %s", rr.Code, rr.Body)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}
