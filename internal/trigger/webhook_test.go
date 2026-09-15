package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
)

// TestWebhookSignatureVerifies: the receiver-side Verify accepts the
// request the engine sends when it holds the right secret, rejects a
// wrong secret, and rejects a timestamp more than five minutes off.
func TestWebhookSignatureVerifies(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotReq = b, r.Clone(context.Background())
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	now := time.Unix(1_800_000_000, 0)
	e := New(Options{
		Secrets: func(ref string) (string, error) {
			if ref != "secretfile:hook" {
				t.Fatalf("secret ref %q", ref)
			}
			return "s3cret", nil
		},
		Now: func() time.Time { return now },
	})
	rule := config.Trigger{Name: "notify", Action: config.TriggerAction{Webhook: &config.WebhookAction{
		URL: srv.URL + "/hook", Secret: "secretfile:hook", Timeout: 5 * time.Second,
	}}}
	d := agent.Delivery{Rule: "notify", Path: "/work/report.md", Kind: "write", Origin: "kernel"}
	out, err := e.runWebhook(context.Background(), rule, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "HTTP 204") {
		t.Fatalf("output = %q", out)
	}
	mu.Lock()
	body, req := gotBody, gotReq
	mu.Unlock()
	if req == nil {
		t.Fatal("nothing arrived")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["rule"] != "notify" || payload["path"] != "/work/report.md" || payload["kind"] != "write" || payload["origin"] != "kernel" || payload["ts"] != float64(now.Unix()) {
		t.Fatalf("body = %s", body)
	}
	if !strings.HasPrefix(payload["uri"].(string), "cloudfs://") {
		t.Fatalf("uri = %v", payload["uri"])
	}
	if _, has := payload["download_url"]; has {
		t.Fatal("download_url present without include_download_url")
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("X-CloudFS-Timestamp") != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("timestamp header %q", req.Header.Get("X-CloudFS-Timestamp"))
	}
	if !strings.HasPrefix(req.Header.Get("X-CloudFS-Signature"), "sha256=") {
		t.Fatalf("signature header %q", req.Header.Get("X-CloudFS-Signature"))
	}
	// The documented receiver check, computed independently of Sign.
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(strconv.FormatInt(now.Unix(), 10) + "."))
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); req.Header.Get("X-CloudFS-Signature") != want {
		t.Fatalf("signature %q, want %q", req.Header.Get("X-CloudFS-Signature"), want)
	}

	if !Verify([]byte("s3cret"), req, body, now) {
		t.Fatal("the right secret was rejected")
	}
	if !Verify([]byte("s3cret"), req, body, now.Add(4*time.Minute)) {
		t.Fatal("a 4 minute skew was rejected")
	}
	if Verify([]byte("wrong"), req, body, now) {
		t.Fatal("a wrong secret was accepted")
	}
	if Verify([]byte("s3cret"), req, body, now.Add(6*time.Minute)) || Verify([]byte("s3cret"), req, body, now.Add(-6*time.Minute)) {
		t.Fatal("a 6 minute skew was accepted")
	}
	if Verify([]byte("s3cret"), req, append(bytes.Clone(body), ' '), now) {
		t.Fatal("a tampered body was accepted")
	}
	tampered := req.Clone(context.Background())
	tampered.Header.Set("X-CloudFS-Timestamp", strconv.FormatInt(now.Unix()+1, 10))
	if Verify([]byte("s3cret"), tampered, body, now) {
		t.Fatal("a re-stamped request was accepted")
	}
	tampered.Header.Del("X-CloudFS-Signature")
	if Verify([]byte("s3cret"), tampered, body, now) {
		t.Fatal("an unsigned request was accepted")
	}
}

// TestWebhookNon2xxIsAFailure: a 5xx is an error carrying the status, so
// the worker backs off and retries; the response body is kept as output.
func TestWebhookNon2xxIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, "try later")
	}))
	defer srv.Close()
	e := New(Options{Secrets: func(string) (string, error) { return "k", nil }})
	rule := config.Trigger{Name: "notify", Action: config.TriggerAction{Webhook: &config.WebhookAction{URL: srv.URL, Secret: "secretfile:k", Timeout: 5 * time.Second}}}
	out, err := e.runWebhook(context.Background(), rule, agent.Delivery{Rule: "notify", Path: "/a"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "try later") {
		t.Fatalf("output = %q", out)
	}
	// A secret that cannot be resolved never sends anything.
	e = New(Options{Secrets: func(string) (string, error) { return "", io.ErrUnexpectedEOF }})
	if _, err := e.runWebhook(context.Background(), rule, agent.Delivery{Rule: "notify"}); err == nil {
		t.Fatal("a missing secret should fail the delivery")
	}
}
