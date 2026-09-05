package mcpsrv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func subscriptionHTTPServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(newMCPHTTPHandler(s))
	t.Cleanup(h.Close)
	t.Cleanup(func() { s.Close() })
	return h
}

func subscriptionRPC(t *testing.T, endpoint, version, session, method string, params any) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", version)
	req.Header.Set("Mcp-Method", method)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestResourceSubscriptionsLegacyHTTP(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/file", []byte("old"))
	ctx := context.Background()
	if _, err := e.fs.StatPath(ctx, "/work/file"); err != nil {
		t.Fatal(err)
	}
	h := subscriptionHTTPServer(t, e.server)
	resp, data := subscriptionRPC(t, h.URL, "2025-11-25", "", "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "legacy-subscription-test", "version": "1"},
	})
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || sessionID == "" || !bytes.Contains(data, []byte(`"protocolVersion":"2025-11-25"`)) {
		t.Fatalf("legacy initialize: status=%d session=%q body=%s", resp.StatusCode, sessionID, data)
	}

	// The legacy notification stream is an independent GET, unlike the modern
	// POST's response. Exercise real HTTP rather than invoking SDK hooks.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, h.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", sessionID)
	updates := make(chan *mcp.ResourceUpdatedNotificationParams, 64)
	streamDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			streamDone <- err
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var msg struct {
				Method string                                 `json:"method"`
				Params *mcp.ResourceUpdatedNotificationParams `json:"params"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &msg) == nil && msg.Method == "notifications/resources/updated" {
				select {
				case updates <- msg.Params:
				case <-streamCtx.Done():
				}
			}
		}
		streamDone <- scanner.Err()
	}()
	uri := "cloudfs://ali/work/file"
	for i := 0; i < 2; i++ {
		resp, data = subscriptionRPC(t, h.URL, "2025-11-25", sessionID, "resources/subscribe", map[string]string{"uri": uri})
		if resp.StatusCode != http.StatusOK || bytes.Contains(data, []byte(`"error"`)) {
			t.Fatalf("legacy subscribe: %d %s", resp.StatusCode, data)
		}
	}
	waitSubscriptionCount(t, e.server, 1)
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("changed"), false); err != nil {
		t.Fatal(err)
	}
	c := &subscriptionClient{updates: updates}
	c.update(t, uri, nil)
	c.quiet(t)
	resp, data = subscriptionRPC(t, h.URL, "2025-11-25", sessionID, "resources/unsubscribe", map[string]string{"uri": uri})
	if resp.StatusCode != http.StatusOK || bytes.Contains(data, []byte(`"error"`)) {
		t.Fatalf("legacy unsubscribe: %d %s", resp.StatusCode, data)
	}
	waitSubscriptionCount(t, e.server, 0)
	if _, err := e.fs.WriteFile(ctx, "/work/file", []byte("unsubscribed"), false); err != nil {
		t.Fatal(err)
	}
	c.quiet(t)
	cancel()
	select {
	case <-streamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("legacy SSE did not disconnect")
	}
}

func TestResourceSubscriptionsHTTPRejectsUnauthorizedBatch(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	h := subscriptionHTTPServer(t, e.server)
	for _, uris := range [][]string{
		{"cloudfs://ali/work/ok", "cloudfs://ali/secret"},
		{"cloudfs://ali/work/ok", "cloudfs://ali/work/ok"},
	} {
		resp, body := subscriptionRPC(t, h.URL, "2026-07-28", "", "subscriptions/listen", map[string]any{
			"_meta":         map[string]any{mcp.MetaKeyProtocolVersion: "2026-07-28", mcp.MetaKeyClientCapabilities: map[string]any{}},
			"notifications": map[string]any{"resourceSubscriptions": uris},
		})
		if !bytes.Contains(body, []byte(`"code":-32602`)) || bytes.Contains(body, []byte("notifications/subscriptions/acknowledged")) {
			t.Fatalf("invalid batch was not atomically rejected: %d %s", resp.StatusCode, body)
		}
		waitSubscriptionCount(t, e.server, 0)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("invalid HTTP subscription accessed provider")
	}
}
