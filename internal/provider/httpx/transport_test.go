package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/net/ratelimit"
)

func TestRawTransportPreservesSDKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "cloudfs-test" {
			t.Errorf("user agent = %q", r.UserAgent())
		}
		w.Header().Set("X-SDK-Error", "NoSuchKey")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<Error>sdk must parse me</Error>")
	}))
	defer server.Close()
	c := New(Options{HTTP: server.Client(), UserAgent: "cloudfs-test"})
	seen := ratelimit.Meta
	httpClient := &http.Client{Transport: c.RawTransport(func(*http.Request) ratelimit.Class {
		seen = ratelimit.Download
		return seen
	})}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/bucket/key?X-Amz-Signature=secret", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-SDK-Error") != "NoSuchKey" || !strings.Contains(string(body), "sdk must parse me") {
		t.Fatalf("raw response was consumed or changed: %d %q %q", resp.StatusCode, resp.Header, body)
	}
	if seen != ratelimit.Download {
		t.Fatalf("classifier result = %v", seen)
	}
}
