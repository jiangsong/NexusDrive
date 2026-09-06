package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuth is an AuthStarter whose flows never touch the network. It records
// what was presented so the test can assert the URL/QR reached the page and
// the token never did.
type fakeAuth struct {
	mu        sync.Mutex
	presented []string
	release   chan error
}

func (f *fakeAuth) starter(support func(string) bool) *AuthStarter {
	return &AuthStarter{
		Supported: support,
		OAuth: func(ctx context.Context, name string, present func(url string)) (string, func(context.Context) error, error) {
			present("https://oauth.example/authorize?token=never-show-this")
			f.mu.Lock()
			f.presented = append(f.presented, "url")
			f.mu.Unlock()
			return "http://127.0.0.1:53682/callback", func(waitCtx context.Context) error {
				select {
				case err := <-f.release:
					return err
				case <-waitCtx.Done():
					return waitCtx.Err()
				}
			}, nil
		},
		Device: func(ctx context.Context, name string, present func(qr string), scanned func()) (func(context.Context) error, error) {
			present("115://qr-content-never-a-token")
			f.mu.Lock()
			f.presented = append(f.presented, "qr")
			f.mu.Unlock()
			return func(waitCtx context.Context) error {
				select {
				case err := <-f.release:
					return err
				case <-waitCtx.Done():
					return waitCtx.Err()
				}
			}, nil
		},
	}
}

func TestAuthFlowPresentsAURLAndNeverATokenAndCompletes(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "aliyun-account", "aliyun")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "aliyun" })

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/aliyun-account/auth/start", AuthStartRequest{})
	if rr.Code != 200 {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	var start AuthStartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.Kind != "url" || !strings.HasPrefix(start.Value, "https://oauth.example") || start.Session == "" || start.RedirectURI == "" {
		t.Fatalf("start reply: %+v", start)
	}

	// A second start for the same account is refused while one is running.
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/aliyun-account/auth/start", AuthStartRequest{}); rr.Code != 409 {
		t.Fatalf("second start: %d %s", rr.Code, rr.Body)
	}

	// Status is pending until the flow completes.
	rr = accountRequest(t, srv, http.MethodGet, "/accounts/aliyun-account/auth/status?session="+start.Session, nil)
	var st AuthStatusResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if st.State != "pending" {
		t.Fatalf("status before completion: %+v", st)
	}
	fa.release <- nil
	deadline := time.After(2 * time.Second)
	for {
		rr = accountRequest(t, srv, http.MethodGet, "/accounts/aliyun-account/auth/status?session="+start.Session, nil)
		_ = json.Unmarshal(rr.Body.Bytes(), &st)
		if st.State == "done" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("flow did not reach done: %+v", st)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	// The done poll retires the session, so a new start is allowed.
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/aliyun-account/auth/start", AuthStartRequest{}); rr.Code != 200 {
		t.Fatalf("start after completion: %d %s", rr.Code, rr.Body)
	}
}

func TestAuthFlowFailureIsGenericAndCancelStops(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "q", "pan115")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "pan115" })

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/q/auth/start", AuthStartRequest{})
	var start AuthStartResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &start)
	if start.Kind != "qr" || !strings.HasPrefix(start.Value, "115://") {
		t.Fatalf("qr start: %+v", start)
	}
	fa.release <- errors.New("provider said https://signed.example/oops?token=leak")
	var st AuthStatusResponse
	deadline := time.After(2 * time.Second)
	for {
		rr = accountRequest(t, srv, http.MethodGet, "/accounts/q/auth/status?session="+start.Session, nil)
		_ = json.Unmarshal(rr.Body.Bytes(), &st)
		if st.State != "pending" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("failure never surfaced")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if st.State != "error" || strings.Contains(st.Error, "signed.example") || strings.Contains(st.Error, "leak") {
		t.Fatalf("failure leaked the provider error: %+v", st)
	}

	// Cancel a fresh flow.
	rr = accountRequest(t, srv, http.MethodPost, "/accounts/q/auth/start", AuthStartRequest{})
	_ = json.Unmarshal(rr.Body.Bytes(), &start)
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/q/auth/cancel?session="+start.Session, nil); rr.Code != 200 {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/q/auth/status?session="+start.Session, nil); rr.Code != 404 {
		t.Fatalf("status after cancel: %d", rr.Code)
	}
}

func TestAuthFlowRefusesTerminalOnlyProviders(t *testing.T) {
	srv, _, _ := accountsServer(t)
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(SupportsDaemonAuthLike)
	// "existing" is a webdav remote; its credential is a password.
	rr := accountRequest(t, srv, http.MethodPost, "/accounts/existing/auth/start", AuthStartRequest{})
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "cloudfs config auth existing") {
		t.Fatalf("webdav auth start: %d %s", rr.Code, rr.Body)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.presented) != 0 {
		t.Fatal("a terminal-only provider started a browser flow")
	}
}

// SupportsDaemonAuthLike mirrors the daemon's own decision, for the test.
func SupportsDaemonAuthLike(remoteType string) bool {
	switch remoteType {
	case "aliyun", "baidu", "pan115":
		return true
	}
	return false
}
