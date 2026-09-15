package control

import (
	"cloudfs/internal/config"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// The authorization routes let a page start an OAuth or QR login for a
// configured account. What crosses this boundary is what the person would see
// in a terminal: a URL to open, or a QR string to render. The credential does
// not: the daemon runs the callback listener, exchanges the code and saves the
// token itself (internal/daemon/auth.go). So a browser can drive a login for
// aliyun, baidu or 115 without the token ever passing through it.
//
// Quark is the exception among cookie-backed accounts: its web QR exchange
// lets the daemon obtain the cookie without the browser page ever seeing it.
// Password- and external-token-type accounts remain terminal-only imports.

// AuthStarter is what the daemon supplies so this package need not import it.
// present is called with a URL (OAuth) or QR content (device); it must not
// block. wait completes the flow.
type AuthStarter struct {
	Supported func(remoteType string) bool
	OAuth     func(ctx context.Context, name string, present func(url string)) (redirect string, wait func(context.Context) error, err error)
	Device    func(ctx context.Context, name string, present func(qr string), scanned func()) (wait func(context.Context) error, err error)
	// FillClientID writes the shipped OAuth application's client id into a new
	// account that names none. It is supplied rather than called directly
	// because the table of applications belongs to the layer that performs
	// authorizations, and this package sits below it. Leaving it nil means no
	// application is shipped, which is exactly today's behaviour.
	FillClientID func(r *config.Remote)
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
//
// inFlight goes further: the OAuth callback binds one fixed loopback port, and
// a provider that requires an exactly-registered redirect URI leaves no room
// for an ephemeral one, so the port is a process-wide lock rather than a
// per-account one. Tracking it only per account meant two different accounts
// authorizing at once raced for the listener and the loser failed with
// "address already in use", which reached the page as a generic gateway error
// naming nothing anyone could act on.
type authRegistry struct {
	mu       sync.Mutex
	byID     map[string]*authSession
	byRemote map[string]string
	// inFlight is the id of the session holding the port, and inFlightRemote
	// the account it belongs to, for the refusal the page shows. It is keyed
	// by session rather than by account because the settle goroutine releases
	// the port when its own flow ends: keyed by account, a goroutine winding
	// down from a session that was already cancelled would release the lock a
	// newer session for that same account is holding, and the next account
	// would then race the live listener for the port.
	inFlight       string
	inFlightRemote string
}

func newAuthRegistry() *authRegistry {
	return &authRegistry{byID: map[string]*authSession{}, byRemote: map[string]string{}}
}

func (r *authRegistry) get(id string) *authSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// claim registers a session and takes the callback port for it. It reports the
// account already on the port when it cannot, and whether the refusal is
// "this account is already authorizing" rather than "somebody else has the
// port" — the page says different things about the two.
func (r *authRegistry) claim(id, remote string, sess *authSession) (holder string, inProgress bool, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.byRemote[remote]; running {
		return remote, true, false
	}
	if r.inFlight != "" {
		return r.inFlightRemote, false, false
	}
	r.byID[id] = sess
	r.byRemote[remote] = id
	r.inFlight, r.inFlightRemote = id, remote
	return "", false, true
}

// releasePort gives the callback port back without retiring the session: the
// result is still there to be polled, but nothing is listening any more. It
// takes the session id, so a goroutine winding down from a flow that was
// already replaced releases nothing.
func (r *authRegistry) releasePort(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight == id {
		r.inFlight, r.inFlightRemote = "", ""
	}
}

func (r *authRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byID[id]; ok {
		delete(r.byID, id)
		if r.byRemote[s.remote] == id {
			delete(r.byRemote, s.remote)
		}
		if r.inFlight == id {
			r.inFlight, r.inFlightRemote = "", ""
		}
	}
}

// authPresentTimeout is how long a start waits for the authorization URL or QR
// code before giving up on it. A variable so a test does not have to wait it
// out in real time.
var authPresentTimeout = 15 * time.Second

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AuthStartRequest is POST /accounts/{name}/auth/start. It carries nothing:
// the callback the daemon listens on is the one the provider's application is
// registered with, and letting a caller choose it would let any page that can
// reach the control plane pick which local port the listener binds. The
// response reports the redirect that was used.
type AuthStartRequest struct{}

// AuthStartResponse hands back what to present. Kind is "url" for OAuth or
// "qr" for a device flow. Value is populated only for OAuth; QR content stays
// in the server-side session and is exposed solely as a same-origin PNG.
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
		httpErrorT(w, r, http.StatusNotImplemented, "err.auth_unwired")
		return
	}
	remote, ok := s.collector.ConfigView().Remotes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	rtype := remote.Type
	if s.auth.Supported == nil || !s.auth.Supported(rtype) {
		httpErrorT(w, r, http.StatusBadRequest, "err.auth_terminal_only", name)
		return
	}
	var in AuthStartRequest
	if r.ContentLength != 0 && !decodeMutation(w, r, &in) {
		return
	}
	id := randomID()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	sess := &authSession{remote: name, state: "pending", cancel: cancel, done: make(chan struct{}), created: time.Now()}
	holder, inProgress, ok := s.authReg.claim(id, name, sess)
	if !ok {
		cancel()
		if inProgress {
			httpErrorT(w, r, http.StatusConflict, "err.auth_in_progress")
			return
		}
		httpErrorT(w, r, http.StatusConflict, "err.auth_port_busy", holder)
		return
	}

	shown := make(chan struct{})
	var wait func(context.Context) error
	var startErr error
	var redirectURI string
	if rtype == "pan115" || rtype == "quark" {
		sess.kind = "qr"
		wait, startErr = s.auth.Device(ctx, name,
			func(qr string) { sess.mu.Lock(); sess.value = qr; sess.mu.Unlock(); closeOnce(shown) },
			func() { sess.set("scanned", "") })
	} else {
		sess.kind = "url"
		var redirect string
		redirect, wait, startErr = s.auth.OAuth(ctx, name,
			func(url string) { sess.mu.Lock(); sess.value = url; sess.mu.Unlock(); closeOnce(shown) })
		if startErr == nil {
			// Wait for the URL to be presented before replying.
			select {
			case <-shown:
			case <-time.After(authPresentTimeout):
				startErr = errors.New("authorization did not start in time")
			}
		}
		redirectURI = redirect
	}
	if startErr != nil {
		cancel()
		s.authReg.remove(id)
		// The setup error can name a local secrets-file path or proxy
		// internals (it comes from reading credentials / building the proxy
		// before any URL is shown). Keep it server-side; the async path is
		// already generic, and this one must be too.
		log.Printf("control: authorization start for %q failed: %v", name, startErr)
		httpErrorT(w, r, http.StatusBadGateway, "err.auth_start_failed")
		return
	}
	if rtype == "pan115" || rtype == "quark" {
		select {
		case <-shown:
		case <-time.After(authPresentTimeout):
			cancel()
			s.authReg.remove(id)
			// wait owns this attempt's cleanup — the HTTP client it built for
			// the exchange, the proxy checker it started — and that cleanup
			// runs only when wait returns. Abandoning the flow without calling
			// it leaks both, once per timed-out attempt. The context is
			// already cancelled, so it returns at once.
			go func() {
				defer close(sess.done)
				defer s.authReg.releasePort(id)
				_ = wait(ctx)
			}()
			httpErrorT(w, r, http.StatusBadGateway, "err.device_code_timeout")
			return
		}
	}
	go func() {
		defer close(sess.done)
		// The callback listener is gone the moment the flow settles, however
		// it settled. Holding the process-wide lock until somebody polls or
		// cancels would let a closed browser tab block every other account
		// until the daemon restarted — and closing a tab sends nothing.
		defer s.authReg.releasePort(id)
		err := wait(ctx)
		switch {
		case err == nil:
			// OAuth/device implementations persist credentials before wait
			// succeeds. Publish that disk state before reporting done, otherwise
			// the very GET the page performs on completion still says
			// has_credentials=false until some unrelated config edit or restart.
			s.reloadConfigView()
			sess.set("done", "")
		case errors.Is(err, context.Canceled):
			sess.set("denied", "authorization was cancelled")
		case errors.Is(err, context.DeadlineExceeded):
			sess.set("denied", "authorization timed out")
		default:
			// SaveCredentials and auth already redact; still, never the raw
			// provider text of a token exchange.
			log.Printf("control: authorization for %q failed: %v", name, err)
			sess.set("error", "authorization failed")
		}
	}()
	kind, value, _, _ := sess.snapshot()
	if kind == "qr" {
		value = ""
	}
	writeJSON(w, AuthStartResponse{Session: id, Kind: kind, Value: value, RedirectURI: redirectURI})
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

func (s *Server) authQR(w http.ResponseWriter, r *http.Request, name string) {
	id := r.URL.Query().Get("session")
	sess := s.authReg.get(id)
	if sess == nil || sess.remote != name {
		http.NotFound(w, r)
		return
	}
	kind, value, state, _ := sess.snapshot()
	if kind != "qr" || value == "" || state == "done" || state == "error" || state == "denied" {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(value, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "cannot render authorization QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(png)))
	w.Write(png)
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// authByName handles /accounts/{name}/auth/{start,status,cancel,qr}.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, name, action string) {
	switch action {
	case "start":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		s.authStart(w, r, name)
	case "status":
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		s.authStatus(w, r, name)
	case "cancel":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		s.authCancel(w, r, name)
	case "qr":
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		s.authQR(w, r, name)
	default:
		http.NotFound(w, r)
	}
}
