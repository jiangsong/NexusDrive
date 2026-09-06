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
	// The CSP is tightened, not loosened: scripts and styles come from 'self',
	// never inline. An external script would be blocked.
	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP still allows inline or an external origin: %q", csp)
	}
	for _, header := range []string{"Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if rr.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("index cache control = %q", got)
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

// TestWebAppNeverAsksForACredential: the boundary is visible in the served
// bytes. No password input, and no credential field name appears as an input
// name or id anywhere in the app — credentials go through the terminal, and
// the page must not so much as offer a field for one.
func TestWebAppNeverAsksForACredential(t *testing.T) {
	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()
	var all strings.Builder
	for path := range srv.assets {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, uiReq(http.MethodGet, path))
		all.Write(rr.Body.Bytes())
	}
	blob := all.String()
	if strings.Contains(blob, `type="password"`) || strings.Contains(blob, "type=password") {
		t.Fatal("the app renders a password input")
	}
	for _, secret := range []string{"refresh_token", "client_secret", "access_token", `name="password"`, `name="cookie"`, `id="secret"`} {
		if strings.Contains(blob, secret) {
			t.Fatalf("the app names a credential field: %q", secret)
		}
	}
	// It does point people at the terminal for authorization.
	if !strings.Contains(blob, "config auth") {
		t.Fatal("the app does not tell the user where credentials are set")
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
