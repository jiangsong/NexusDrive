package mcpsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPHTTPWildcardRequiresAuthentication(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0"} {
		t.Run(addr, func(t *testing.T) {
			loopback, err := isLoopback(addr)
			if err != nil || loopback {
				t.Fatalf("public bind misclassified: loopback=%v err=%v", loopback, err)
			}
			e := newEnv(t, Options{})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := ServeHTTPWithToken(ctx, e.server, addr, ""); err == nil || !strings.Contains(err.Error(), "without a token") {
				t.Fatalf("public MCP listener did not reject missing authentication: %v", err)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		if loopback, err := isLoopback(addr); err != nil || !loopback {
			t.Errorf("loopback rejected: %s %v", addr, err)
		}
	}
}

func TestMCPHTTPCancellationCompatibilityIsNarrowAndAuthenticated(t *testing.T) {
	e := newEnv(t, Options{})
	h := requireBearer(newMCPHTTPHandler(e.server), "test-token")
	valid := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2,"reason":"context canceled"}}`
	for _, tc := range []struct {
		name, body, version, method, token, origin string
		status                                     int
	}{
		{"redundant", valid, "2026-07-28", "", "test-token", "", 202},
		{"method header", valid, "2026-07-28", "notifications/cancelled", "test-token", "", 202},
		{"unauthenticated", valid, "2026-07-28", "", "", "", 401},
		{"foreign origin", valid, "2026-07-28", "", "test-token", "https://evil.example", 403},
		{"not notification", strings.Replace(valid, `"jsonrpc":`, `"id":1,"jsonrpc":`, 1), "2026-07-28", "", "test-token", "", 400},
		{"null request id", strings.Replace(valid, `"requestId":2`, `"requestId":null`, 1), "2026-07-28", "", "test-token", "", 400},
		{"wrong body method", strings.Replace(valid, "notifications/cancelled", "resources/read", 1), "2026-07-28", "", "test-token", "", 400},
		{"header mismatch", valid, "2026-07-28", "resources/read", "test-token", "", 400},
		{"metadata mismatch", strings.Replace(valid, `"requestId":2`, `"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"},"requestId":2`, 1), "2026-07-28", "", "test-token", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", tc.version)
			req.Header.Set("Mcp-Method", tc.method)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, req)
			if resp.Code != tc.status {
				t.Fatalf("got %d want %d: %s", resp.Code, tc.status, resp.Body.String())
			}
		})
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("HTTP envelope validation accessed provider")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "http://localhost/mcp", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Authorization", "Bearer test-token")
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("foreign Origin on %s was not rejected: %d", method, resp.Code)
		}
	}
}
