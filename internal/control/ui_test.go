package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusUIIsExplicitAndReadOnly(t *testing.T) {
	plain := NewServer(&Collector{}).Handler()
	rr := httptest.NewRecorder()
	plain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled UI status = %d", rr.Code)
	}

	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "fetch('/status'") {
		t.Fatalf("enabled UI status/body = %d %q", rr.Code, rr.Body.String())
	}
	for _, route := range []string{"/uploads?limit=50", "uploads/${action}"} {
		if !strings.Contains(body, route) {
			t.Errorf("UI does not use existing route %q", route)
		}
	}
	for _, header := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if rr.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q", got)
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST UI status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/", nil))
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatalf("HEAD UI status/body = %d/%d", rr.Code, rr.Body.Len())
	}
}

func TestStatusUIKeepsExistingRoutes(t *testing.T) {
	srv := NewServer(&Collector{Version: "ui-test"})
	srv.EnableUI()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version": "ui-test"`) {
		t.Fatalf("status route changed: %d %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/not-an-asset", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown UI path status = %d", rr.Code)
	}
}
