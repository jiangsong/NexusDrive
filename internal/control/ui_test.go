package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebAppIsExplicitAndReadOnly(t *testing.T) {
	plain := NewServer(&Collector{}).Handler()
	rr := httptest.NewRecorder()
	plain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled UI at / = %d", rr.Code)
	}

	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
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
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != tc.contentType || rr.Header().Get("ETag") == "" {
			t.Errorf("%s = %d type=%q etag=%q", tc.path, rr.Code, rr.Header().Get("Content-Type"), rr.Header().Get("ETag"))
		}
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/not-an-asset.js", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown asset = %d", rr.Code)
	}

	// A conditional request revalidates against the ETag.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/app.css", nil))
	etag := rr.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/ui/app.css", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", rr.Code)
	}

	// POST to an asset is refused; the app is read-only static.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d", rr.Code)
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
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
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
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version": "ui-test"`) {
		t.Fatalf("status route changed: %d", rr.Code)
	}
}
