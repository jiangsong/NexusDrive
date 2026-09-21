package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProbeRequiresAuthenticatedHandshakeAndMemoryAccess(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "memory_list", Description: "test"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"facts": []any{}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-local" {
			http.Error(w, "denied", 401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	c, m := Probe(context.Background(), ts.URL, "test-local")
	if c != "connected" || m != "readable; writes require authorization" {
		t.Fatalf("%s %s", c, m)
	}
	c, _ = Probe(context.Background(), ts.URL, "bad")
	if c == "connected" {
		t.Fatal("unauthorized shown connected")
	}
}
