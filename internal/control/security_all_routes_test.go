package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEveryRouteIsEitherGuardedOrArguedOpen walks the route table and holds
// every route to the same rule, so the next route added here cannot quietly
// skip the guard the way /cache/drop did. A guarded route refuses a mutation
// without the control header, refuses any request from another origin, and
// refuses a request whose Host is not this machine — before it looks at
// anything else. The open set is exactly the four read-only routes NewServer
// argues for; growing it is a decision, not a side effect.
func TestEveryRouteIsEitherGuardedOrArguedOpen(t *testing.T) {
	f := newFixture(t)
	s := NewServer(f.coll)

	open := map[string]bool{}
	for _, r := range s.routes() {
		if r.open {
			open[r.pattern] = true
		}
	}
	for _, want := range []string{"/healthz", "/readyz", "/status", "/metrics"} {
		if !open[want] {
			t.Fatalf("%s is expected to be open", want)
		}
		delete(open, want)
	}
	for extra := range open {
		t.Fatalf("%s is marked open; add it to the argued list in NewServer or guard it", extra)
	}

	for _, r := range s.routes() {
		if r.open {
			continue
		}
		target := r.pattern
		if r.probe != "" {
			target = r.probe
		}
		for _, tc := range []struct {
			name, method, host, origin, header string
		}{
			{"mutation without the control header", "POST", "cloudfs", "", ""},
			{"mutation from another origin", "POST", "cloudfs", "https://evil.invalid", "1"},
			{"read from another origin", "GET", "cloudfs", "https://evil.invalid", ""},
			{"rebound host", "GET", "cloudfs.attacker.invalid", "", ""},
			{"rebound host mutation", "POST", "cloudfs.attacker.invalid", "", "1"},
		} {
			t.Run(target+" "+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, "http://"+tc.host+target, strings.NewReader("{}"))
				req.Host = tc.host
				req.Header.Set("Content-Type", "application/json")
				if tc.origin != "" {
					req.Header.Set("Origin", tc.origin)
				}
				if tc.header != "" {
					req.Header.Set("X-CloudFS-Control", tc.header)
				}
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, req)
				if w.Code != 403 {
					t.Fatalf("%s %s: got %d %s, want 403 before any handler logic runs", tc.method, target, w.Code, strings.TrimSpace(w.Body.String()))
				}
			})
		}
	}
}
