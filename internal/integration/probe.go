package integration

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// Probe verifies an authenticated MCP handshake and bounded memory read. It
// never upgrades hook verification: a CLI probe is not a real client hook run.
func Probe(ctx context.Context, url, token string) (connection, memory string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "cloudfs-integration-check", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearerTransport{token}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return "unavailable or unauthorized", "unavailable"
	}
	defer sess.Close()
	result, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "memory_list", Arguments: map[string]any{"agent": "personal", "limit": 1}})
	if err != nil || result.IsError {
		return "connected", "unavailable or unauthorized"
	}
	return "connected", "readable; writes require authorization"
}
