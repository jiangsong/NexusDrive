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

	"cloudfs/internal/config"
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
	if start.Kind != "qr" || start.Session == "" || start.Value != "" {
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

func TestQuarkAuthIsADeviceFlowWithAScannablePNG(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "quark-account", "quark")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "quark" })

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/quark-account/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	var start AuthStartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.Kind != "qr" || start.Session == "" || start.Value != "" {
		t.Fatalf("Quark start = %+v", start)
	}
	rr = accountRequest(t, srv, http.MethodGet, "/accounts/quark-account/auth/qr?session="+start.Session, nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rr.Body.String(), "\x89PNG\r\n\x1a\n") {
		t.Fatalf("QR image = %d type=%q bytes=%x", rr.Code, rr.Header().Get("Content-Type"), rr.Body.Bytes()[:min(rr.Body.Len(), 16)])
	}
	accountRequest(t, srv, http.MethodPost, "/accounts/quark-account/auth/cancel?session="+start.Session, nil)
}

func TestSuccessfulAuthorizationReloadsTheSavedConfigurationBeforeDone(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "quark-account", "quark")
	wantRoot := "authorized-root"
	srv.auth = &AuthStarter{
		Supported: func(kind string) bool { return kind == "quark" },
		OAuth: func(context.Context, string, func(string)) (string, func(context.Context) error, error) {
			return "", nil, errors.New("unexpected OAuth flow")
		},
		Device: func(_ context.Context, _ string, present func(string), _ func()) (func(context.Context) error, error) {
			present("https://scan.example/qr")
			return func(context.Context) error {
				return config.SetRemoteField(cfg.SourcePath, "quark-account", config.SetRemoteFieldOptions{
					Fields: map[string]*string{"root_id": &wantRoot},
				})
			}, nil
		},
	}

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/quark-account/auth/start", AuthStartRequest{})
	var start AuthStartResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &start) != nil {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	deadline := time.After(2 * time.Second)
	for {
		rr = accountRequest(t, srv, http.MethodGet, "/accounts/quark-account/auth/status?session="+start.Session, nil)
		var status AuthStatusResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &status)
		if status.State == "done" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("authorization did not finish: %+v", status)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := srv.collector.ConfigView().Remotes["quark-account"].Extra["root_id"]; got != wantRoot {
		t.Fatalf("published root_id = %v, want %q", got, wantRoot)
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
	case "aliyun", "baidu", "gdrive", "box", "pan115", "quark":
		return true
	}
	return false
}
