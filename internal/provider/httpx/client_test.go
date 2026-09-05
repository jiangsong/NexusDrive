package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

func newClient(t *testing.T, limiters *ratelimit.Registry) *Client {
	t.Helper()
	return New(Options{
		Limiters: limiters,
		Remote:   "test",
		Policy:   retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 3},
	})
}

func TestStatusErrorMapsToSentinels(t *testing.T) {
	cases := map[int]error{
		401: provider.ErrAuth,
		403: provider.ErrLinkExpired,
		404: provider.ErrNotFound,
		409: provider.ErrConflict,
		429: provider.ErrRateLimited,
		503: provider.ErrTransient,
	}
	for code, want := range cases {
		e := &StatusError{Code: code, Status: http.StatusText(code)}
		if !errors.Is(e, want) {
			t.Errorf("status %d should map to %v", code, want)
		}
		if e.HTTPStatus() != code {
			t.Errorf("HTTPStatus = %d", e.HTTPStatus())
		}
	}
	// A 400 has no sentinel and must classify as terminal.
	if retry.Classify(&StatusError{Code: 400}) != retry.ClassTerminal {
		t.Error("400 should be terminal")
	}
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newClient(t, nil)
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.JSON(context.Background(), Request{Method: "GET", URL: srv.URL}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || calls.Load() != 3 {
		t.Fatalf("out=%+v after %d calls", out, calls.Load())
	}
}

func TestDoesNotRetryTerminalStatus(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newClient(t, nil)
	_, err := c.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal status retried %d times", calls.Load())
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 || !strings.Contains(se.Error(), "bad request") {
		t.Fatalf("error = %v", err)
	}
}

func TestStatusErrorRedactsURLQueryCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rejected", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := newClient(t, nil)
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL + "/upload?sig=private-token#fragment"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "fragment") {
		t.Fatalf("signed URL leaked through error: %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.URL != srv.URL+"/upload" {
		t.Fatalf("redacted URL = %#v", se)
	}
}

func TestRetryAfterFeedsTheLimiter(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	reg := ratelimit.NewRegistry(func(ratelimit.Key) ratelimit.Options {
		return ratelimit.Options{Rate: 8, MinRate: 1}
	}, ratelimit.BreakerOptions{})
	c := newClient(t, reg)
	// Give the retry enough attempts to get past the 429.
	c.policy = retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 3}

	if _, err := c.Do(context.Background(), Request{Method: "GET", URL: srv.URL, Class: ratelimit.Meta}); err != nil {
		t.Fatal(err)
	}
	rate := reg.Limiter(ratelimit.Key{Remote: "test", Class: ratelimit.Meta}).Rate()
	if rate >= 8 {
		t.Fatalf("a 429 should have halved the rate, got %v", rate)
	}
}

func TestBreakerBlocksAfterRiskControl(t *testing.T) {
	reg := ratelimit.NewRegistry(
		func(ratelimit.Key) ratelimit.Options { return ratelimit.Options{Rate: 4} },
		ratelimit.BreakerOptions{Threshold: 1, Cooldown: time.Hour},
	)
	reg.Breaker("test", "").Trip()

	c := newClient(t, reg)
	_, err := c.Do(context.Background(), Request{Method: "GET", URL: "http://127.0.0.1:1/never"})
	if !errors.Is(err, provider.ErrRiskControl) {
		t.Fatalf("an open breaker should refuse the call, got %v", err)
	}
	if !strings.Contains(err.Error(), "paused until") {
		t.Fatalf("error should say when it recovers: %v", err)
	}
}

func TestRateLimiterGatesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	reg := ratelimit.NewRegistry(func(ratelimit.Key) ratelimit.Options {
		return ratelimit.Options{Rate: 20, Burst: 1}
	}, ratelimit.BreakerOptions{})
	c := newClient(t, reg)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Do(context.Background(), Request{Method: "GET", URL: srv.URL, Class: ratelimit.Meta}); err != nil {
			t.Fatal(err)
		}
	}
	// Three requests at 20/s with burst 1 must take at least two refills.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("rate limiting did not delay the requests: %v", elapsed)
	}
}

func TestStreamingAndRange(t *testing.T) {
	content := strings.Repeat("abcdefghij", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			if rng != "bytes=10-19" {
				http.Error(w, "unexpected range "+rng, http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(content[10:20]))
			return
		}
		w.Write([]byte(content))
	}))
	defer srv.Close()

	c := newClient(t, nil)
	resp, err := c.Do(context.Background(), Request{
		Method:       "GET",
		URL:          srv.URL,
		Header:       http.Header{"Range": []string{RangeHeader(10, 10)}},
		ExpectStatus: []int{http.StatusPartialContent, http.StatusOK},
		Stream:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != content[10:20] {
		t.Fatalf("range read = %q", got)
	}
}

func TestRangeHeaderFormat(t *testing.T) {
	if got := RangeHeader(0, 100); got != "bytes=0-99" {
		t.Errorf("RangeHeader(0,100) = %q", got)
	}
	if got := RangeHeader(50, 0); got != "bytes=50-" {
		t.Errorf("open-ended range = %q", got)
	}
}

func TestJSONAndFormBodiesAreRetryable(t *testing.T) {
	var bodies []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newClient(t, nil)
	err := c.JSON(context.Background(), Request{
		Method: "POST", URL: srv.URL, JSON: map[string]string{"key": "value"},
	}, &struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("retried body differs: %q", bodies)
	}
	if !strings.Contains(bodies[0], `"key":"value"`) {
		t.Fatalf("json body = %q", bodies[0])
	}

	// Form encoding.
	calls.Store(1)
	bodies = nil
	c2 := newClient(t, nil)
	if _, err := c2.Do(context.Background(), Request{
		Method: "POST", URL: srv.URL, Form: map[string]string{"a b": "c&d"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0] != "a+b=c%26d" {
		t.Fatalf("form body = %q", bodies)
	}
}

func TestStreamingBodyWithoutGetBodyIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newClient(t, nil)
	_, err := c.Do(context.Background(), Request{
		Method: http.MethodPost, URL: srv.URL, Body: strings.NewReader("one-shot"),
	})
	if err == nil {
		t.Fatal("expected the server error")
	}
	if calls.Load() != 1 || body != "one-shot" {
		t.Fatalf("one-shot body was replayed: calls=%d body=%q", calls.Load(), body)
	}
}

func TestStreamingBodyWithGetBodyIsRetried(t *testing.T) {
	var calls atomic.Int64
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(t, nil)
	_, err := c.Do(context.Background(), Request{
		Method: http.MethodPost, URL: srv.URL, Body: strings.NewReader("replayable"),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("replayable")), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("replayable body mismatch: calls=%d bodies=%q", calls.Load(), bodies)
	}
}

func TestUserAgentIsSent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := New(Options{UserAgent: "pan.baidu.com"})
	if _, err := c.Do(context.Background(), Request{Method: "GET", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if seen != "pan.baidu.com" {
		t.Fatalf("User-Agent = %q", seen)
	}
}

func TestDecodeErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	c := newClient(t, nil)
	var out struct{}
	err := c.JSON(context.Background(), Request{Method: "GET", URL: srv.URL}, &out)
	if err == nil || !strings.Contains(err.Error(), "not json") {
		t.Fatalf("decode error should quote the body: %v", err)
	}
}
