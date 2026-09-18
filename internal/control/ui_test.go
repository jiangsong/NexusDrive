package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// uiReq builds a request the UI guard accepts: a loopback Host, as a real
// browser on this machine sends. Tests that assert the guard set their own.
func uiReq(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "127.0.0.1:9101"
	return r
}

func TestWebAppIsExplicitAndReadOnly(t *testing.T) {
	plain := NewServer(&Collector{}).Handler()
	rr := httptest.NewRecorder()
	plain.ServeHTTP(rr, uiReq(http.MethodGet, "/"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled UI at / = %d", rr.Code)
	}

	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, "/"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `src="/ui/app.js"`) {
		t.Fatalf("index = %d %q", rr.Code, rr.Body.String())
	}
	// script-src is strict — 'self' only, no inline and no eval; that is the
	// boundary against injected code running. style-src allows inline style
	// attributes (the app is built with them) but still not an external origin.
	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("script-src is not restricted to self: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("script-src must not allow inline or eval: %q", csp)
	}
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("style-src should allow self + inline styles: %q", csp)
	}
	for _, header := range []string{"Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if rr.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("index cache control = %q", got)
	}

	// statusUI advertises HEAD alongside GET. HEAD is a safe read and must not
	// be rejected by the mutation-header guard before it reaches statusUI.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, uiReq(http.MethodHead, "/"))
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatalf("HEAD / = %d body=%q", rr.Code, rr.Body.String())
	}

	// Every embedded asset is served at its own path with the right type and
	// an ETag; a made-up path is a 404, the contract the mux keeps.
	for _, tc := range []struct{ path, contentType string }{
		{"/ui/app.js", "text/javascript; charset=utf-8"},
		{"/ui/app.css", "text/css; charset=utf-8"},
		{"/ui/screens/main.js", "text/javascript; charset=utf-8"},
	} {
		rr = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, tc.path))
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != tc.contentType || rr.Header().Get("ETag") == "" {
			t.Errorf("%s = %d type=%q etag=%q", tc.path, rr.Code, rr.Header().Get("Content-Type"), rr.Header().Get("ETag"))
		}
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, "/ui/not-an-asset.js"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown asset = %d", rr.Code)
	}

	// A conditional request revalidates against the ETag.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, "/ui/app.css"))
	etag := rr.Header().Get("ETag")
	req := uiReq(http.MethodGet, "/ui/app.css")
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", rr.Code)
	}

	// POST to an asset is refused; the app is read-only static. With the
	// control header it clears the guard and meets statusUI's own 405; without
	// it the guard refuses it first (covered by the rebound/cross-site test).
	req = uiReq(http.MethodPost, "/")
	req.Header.Set("X-CloudFS-Control", "1")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d", rr.Code)
	}
}

// TestWebAppRefusesReboundHostAndCrossSite: the app shell is static, but it is
// still behind the same loopback + same-origin guard as every other route, so a
// DNS-rebound Host or a cross-site fetch cannot read it. This closes the gap the
// route-guard enumeration test could not see, because the UI is mounted outside
// the routes() table.
func TestWebAppRefusesReboundHostAndCrossSite(t *testing.T) {
	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()

	// A rebound Host.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "attacker.example"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("rebound Host at / = %d, want 403", rr.Code)
	}

	// A cross-site fetch (loopback Host but a foreign Origin).
	req = httptest.NewRequest(http.MethodGet, "/ui/app.js", nil)
	req.Host = "127.0.0.1:9101"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site asset fetch = %d, want 403", rr.Code)
	}

	// A top-level navigation from a loopback Host still works (no Origin,
	// Sec-Fetch-Site: none).
	req = uiReq(http.MethodGet, "/")
	req.Header.Set("Sec-Fetch-Site", "none")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback navigation = %d, want 200", rr.Code)
	}
}

// TestWebAppCollectsOnlyTheOAuthApplicationSecret: the credential boundary is
// visible in the served bytes, and it has exactly one opening.
//
// No account credential — a token, a password, a cookie — is named as an input
// anywhere: those are obtained by a daemon-driven exchange or imported through
// the terminal. The exception is the secret of an OAuth application the person
// registered themselves, without which no authorization can begin at all; it
// lives in one module, so this test names that module rather than allowing a
// password field to appear anywhere it pleases.
func TestWebAppCollectsOnlyTheOAuthApplicationSecret(t *testing.T) {
	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()
	const appSecretModule = "/ui/auth_step.js"
	var all strings.Builder
	for path := range srv.assets {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, path))
		body := rr.Body.String()
		all.WriteString(body)
		if path == appSecretModule {
			continue
		}
		if strings.Contains(body, "type: 'password'") || strings.Contains(body, `type="password"`) || strings.Contains(body, "type=password") {
			t.Fatalf("%s renders a password input; the application secret belongs to %s alone", path, appSecretModule)
		}
		if strings.Contains(body, "client_secret") {
			t.Fatalf("%s names the application secret; it belongs to %s alone", path, appSecretModule)
		}
	}
	blob := all.String()
	for _, secret := range []string{"refresh_token", "access_token", `name="password"`, `name="cookie"`, `id="secret"`} {
		if strings.Contains(blob, secret) {
			t.Fatalf("the app names an account credential field: %q", secret)
		}
	}
	// The application secret is posted to the route that stores it and is
	// never read back: nothing in the app asks the daemon for one.
	if !strings.Contains(blob, "auth/app-secret") {
		t.Fatal("the app has no way to supply an OAuth application secret")
	}
	// It does point people at the terminal for credentials it cannot collect.
	if !strings.Contains(blob, "config auth") {
		t.Fatal("the app does not tell the user where credentials are set")
	}
	if strings.Contains(blob, "prompt(") {
		t.Fatal("the app uses a blocking browser prompt instead of an in-app dialog")
	}
	if strings.Contains(blob, "confirmDelete(t(") {
		t.Fatal("the app still calls the typed-confirmation component with its obsolete positional signature")
	}
	if !strings.Contains(blob, "get, subscribe") {
		t.Fatal("status-backed screens do not subscribe to the first live status update")
	}
}

func TestWebAppKeepsExistingDataRoutes(t *testing.T) {
	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, "/status"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version": "ui-test"`) {
		t.Fatalf("status route changed: %d", rr.Code)
	}
}
