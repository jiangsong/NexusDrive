package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// The authorization routes let a page start an OAuth or QR login for a
// configured account. What crosses this boundary is what the person would see
// in a terminal: a URL to open, or a QR string to render. The credential does
// not: the daemon runs the callback listener, exchanges the code and saves the
// token itself (internal/daemon/auth.go). So a browser can drive a login for
// aliyun, baidu or 115 without the token ever passing through it.
//
// Password-, cookie- and external-token-type accounts have no such flow — the
// daemon cannot fetch their credential — and stay `cloudfs config auth
// --stdin`. The page shows the command; it never takes the value.

// AuthStarter is what the daemon supplies so this package need not import it.
// present is called with a URL (OAuth) or QR content (device); it must not
// block. wait completes the flow.
type AuthStarter struct {
	Supported func(remoteType string) bool
	OAuth     func(ctx context.Context, name string, present func(url string)) (redirect string, wait func(context.Context) error, err error)
	Device    func(ctx context.Context, name string, present func(qr string), scanned func()) (wait func(context.Context) error, err error)
}

// authSession is one in-flight login. It is addressed by an unguessable id;
// whoever holds it can poll or cancel, so it is a random 128-bit token.
type authSession struct {
	kind    string // "url" | "qr"
	value   string
	remote  string
	state   string // pending | scanned | done | denied | error
	err     string
	cancel  context.CancelFunc
	done    chan struct{}
	created time.Time
	mu      sync.Mutex
}

func (s *authSession) set(state, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "done" || s.state == "error" || s.state == "denied" {
		return // terminal states are final
	}
	s.state, s.err = state, errText
}

func (s *authSession) snapshot() (kind, value, state, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind, s.value, s.state, s.err
}

// authRegistry holds the running sessions, at most one per account so a second
// start cannot open a second callback listener on the same port.
type authRegistry struct {
	mu       sync.Mutex
	byID     map[string]*authSession
	byRemote map[string]string
}

func newAuthRegistry() *authRegistry {
	return &authRegistry{byID: map[string]*authSession{}, byRemote: map[string]string{}}
}

func (r *authRegistry) get(id string) *authSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *authRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byID[id]; ok {
		delete(r.byID, id)
		if r.byRemote[s.remote] == id {
			delete(r.byRemote, s.remote)
		}
	}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AuthStartRequest is POST /accounts/{name}/auth/start.
type AuthStartRequest struct {
	// RedirectURI overrides the OAuth callback the app is registered with.
	RedirectURI string `json:"redirect_uri,omitempty"`
}

// AuthStartResponse hands back what to present. Kind is "url" for OAuth or
// "qr" for a device code; Value is the URL or the QR content string.
type AuthStartResponse struct {
	Session     string `json:"session"`
	Kind        string `json:"kind"`
	Value       string `json:"value"`
	RedirectURI string `json:"redirect_uri,omitempty"`
}

// AuthStatusResponse is GET /accounts/{name}/auth/status.
type AuthStatusResponse struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func (s *Server) authStart(w http.ResponseWriter, r *http.Request, name string) {
	if s.auth == nil || s.auth.OAuth == nil {
		http.Error(w, "authorization flows are not wired on this daemon", http.StatusNotImplemented)
		return
	}
	if _, ok := s.collector.Config.Remotes[name]; !ok {
		http.NotFound(w, r)
		return
	}
	rtype := s.collector.Config.Remotes[name].Type
	if s.auth.Supported == nil || !s.auth.Supported(rtype) {
		http.Error(w, "this account's credential is set with `cloudfs config auth "+name+"`, not through a browser flow", http.StatusBadRequest)
		return
	}
	var in AuthStartRequest
	if r.ContentLength != 0 && !decodeMutation(w, r, &in) {
		return
	}
	s.authReg.mu.Lock()
	if existing, ok := s.authReg.byRemote[name]; ok {
		s.authReg.mu.Unlock()
		_ = existing
		http.Error(w, "an authorization for this account is already in progress; cancel it first", http.StatusConflict)
		return
	}
	id := randomID()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	sess := &authSession{remote: name, state: "pending", cancel: cancel, done: make(chan struct{}), created: time.Now()}
	s.authReg.byID[id] = sess
	s.authReg.byRemote[name] = id
	s.authReg.mu.Unlock()

	shown := make(chan struct{})
	var wait func(context.Context) error
	var startErr error
	if rtype == "pan115" {
		sess.kind = "qr"
		wait, startErr = s.auth.Device(ctx, name,
			func(qr string) { sess.mu.Lock(); sess.value = qr; sess.mu.Unlock(); closeOnce(shown) },
			func() { sess.set("scanned", "") })
	} else {
		sess.kind = "url"
		var redirect string
		redirect, wait, startErr = s.auth.OAuth(ctx, name,
			func(url string) { sess.mu.Lock(); sess.value = url; sess.mu.Unlock(); closeOnce(shown) })
		sess.mu.Lock()
		_ = redirect
		sess.mu.Unlock()
		if startErr == nil {
			// Wait for the URL to be presented before replying.
			select {
			case <-shown:
			case <-time.After(15 * time.Second):
				startErr = errors.New("authorization did not start in time")
			}
		}
		in.RedirectURI = redirect
	}
	if startErr != nil {
		cancel()
		s.authReg.remove(id)
		// The setup error can name a local secrets-file path or proxy
		// internals (it comes from reading credentials / building the proxy
		// before any URL is shown). Keep it server-side; the async path is
		// already generic, and this one must be too.
		log.Printf("control: authorization start for %q failed: %v", name, startErr)
		http.Error(w, "could not start authorization; check the account settings and the daemon log", http.StatusBadGateway)
		return
	}
	if rtype == "pan115" {
		select {
		case <-shown:
		case <-time.After(15 * time.Second):
			cancel()
			s.authReg.remove(id)
			http.Error(w, "the device code did not arrive in time", http.StatusBadGateway)
			return
		}
	}
	go func() {
		defer close(sess.done)
		err := wait(ctx)
		switch {
		case err == nil:
			sess.set("done", "")
		case errors.Is(err, context.Canceled):
			sess.set("denied", "authorization was cancelled")
		case errors.Is(err, context.DeadlineExceeded):
			sess.set("denied", "authorization timed out")
		default:
			// SaveCredentials and auth already redact; still, never the raw
			// provider text of a token exchange.
			sess.set("error", "authorization failed")
		}
	}()
	kind, value, _, _ := sess.snapshot()
	writeJSON(w, AuthStartResponse{Session: id, Kind: kind, Value: value, RedirectURI: in.RedirectURI})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request, name string) {
	id := r.URL.Query().Get("session")
	sess := s.authReg.get(id)
	if sess == nil || sess.remote != name {
		http.NotFound(w, r)
		return
	}
	_, _, state, errText := sess.snapshot()
	if state == "done" || state == "error" || state == "denied" {
		// A terminal poll can retire the session.
		defer s.authReg.remove(id)
	}
	writeJSON(w, AuthStatusResponse{State: state, Error: errText})
}

func (s *Server) authCancel(w http.ResponseWriter, r *http.Request, name string) {
	id := r.URL.Query().Get("session")
	sess := s.authReg.get(id)
	if sess == nil || sess.remote != name {
		http.NotFound(w, r)
		return
	}
	sess.cancel()
	s.authReg.remove(id)
	writeJSON(w, map[string]any{"cancelled": true})
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// authByName handles /accounts/{name}/auth/{start,status,cancel}.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, name, action string) {
	switch action {
	case "start":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.authStart(w, r, name)
	case "status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.authStatus(w, r, name)
	case "cancel":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.authCancel(w, r, name)
	default:
		http.NotFound(w, r)
	}
}
