package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/embed"
	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

// embeddingServer is indexServer with an openai-style embedding block in
// the published configuration and a fake endpoint behind the index. The
// key reference and a credentialed base_url are both present so a leak
// of either has something to leak.
func embeddingServer(t *testing.T) (*fixture, *fakeIndex, *embed.Fake, http.Handler) {
	t.Helper()
	f, fi, _ := indexServer(t)
	f.coll.Config = &config.Config{}
	f.coll.Config.Index.Embedding = config.IndexEmbedding{
		Provider: "openai", Model: "text-embedding-3-small",
		BaseURL: "https://user:sk-live-topsecret@embed.example.com/v1",
		APIKey:  "keyring:index.embedding", Dimensions: 8, AllowRemote: true,
	}
	fake := embed.NewFake(8)
	fi.embedder = fake
	fi.status.Embedding = index.EmbeddingStatus{
		Provider: "openai", Model: "text-embedding-3-small", Dim: 8, Remote: true, Host: "embed.example.com",
		Healthy: true, Embedded: 10, Pending: 2, CharsThisMonth: 4000,
	}
	fi.status.Vectors, fi.status.MaxChunks = 10, 200000
	return f, fi, fake, NewServer(f.coll).Handler()
}

// TestEmbeddingStatusNeverLeaksTheKey: the endpoint panel learns where
// content goes and whether a key is set, never the key, the reference
// that resolves to it, the credentialed URL or the header it travels in.
func TestEmbeddingStatusNeverLeaksTheKey(t *testing.T) {
	_, _, _, h := embeddingServer(t)
	w := uiCallControl(t, h, "GET", "/index/embedding", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, secret := range []string{"sk-live-topsecret", "keyring:index.embedding", "index.embedding", "Authorization", "api_key\":\"", "user:"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaks %q: %s", secret, body)
		}
	}
	for name, values := range w.Header() {
		for _, v := range values {
			if strings.Contains(v, "sk-live-topsecret") || strings.Contains(v, "keyring:") || strings.EqualFold(name, "Authorization") {
				t.Fatalf("header %s leaks: %s", name, v)
			}
		}
	}
	var res EmbeddingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || res.Provider != "openai" || res.Model != "text-embedding-3-small" || res.Dim != 8 || !res.Remote || res.BaseHost != "embed.example.com" || !res.Healthy {
		t.Fatalf("%+v", res)
	}
	if !res.APIKeyConfigured || res.Embedded != 10 || res.Pending != 2 || res.Vectors != 10 || res.MaxChunks != 200000 || res.CharsThisMonth != 4000 {
		t.Fatalf("%+v", res)
	}
	if res.Estimate.Chars != 4000 || res.Estimate.TokensApprox != 1000 || !strings.Contains(res.Estimate.Formula, "300") || res.Estimate.Note == "" {
		t.Fatalf("estimate: %+v", res.Estimate)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cacheable: %v", w.Header())
	}
	if w := uiCallControl(t, h, "POST", "/index/embedding", "{}"); w.Code != 405 {
		t.Fatalf("POST status: %d", w.Code)
	}
}

// TestEmbeddingCheckCallsTheEndpointOnce: the check button is the one
// place the console spends a request, and it spends exactly one; reading
// the status spends none.
func TestEmbeddingCheckCallsTheEndpointOnce(t *testing.T) {
	_, fi, fake, h := embeddingServer(t)
	uiCallControl(t, h, "GET", "/index/embedding", "")
	if fake.Calls() != 0 {
		t.Fatalf("GET called the endpoint %d times", fake.Calls())
	}
	w := uiCallControl(t, h, "POST", "/index/embedding/check", "{}")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if fake.Calls() != 1 || len(fake.Batches()) != 1 || fake.Batches()[0] != 1 {
		t.Fatalf("calls %d batches %v", fake.Calls(), fake.Batches())
	}
	var res EmbeddingCheckResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Dim != 8 || res.Error != "" || res.LatencyMS < 0 {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(w.Body.String(), "sk-live-topsecret") || strings.Contains(w.Body.String(), "keyring:") {
		t.Fatalf("check leaks: %s", w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/index/embedding/check", ""); w.Code != 405 {
		t.Fatalf("GET check: %d", w.Code)
	}

	// A failing endpoint is reported, not raised: ok false with the error,
	// and still exactly one call.
	fake.SetError(context.DeadlineExceeded)
	w = uiCallControl(t, h, "POST", "/index/embedding/check", "{}")
	if w.Code != 200 {
		t.Fatalf("failing check: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.OK || !strings.Contains(res.Error, "deadline") {
		t.Fatalf("%+v %v", res, err)
	}
	if fake.Calls() != 2 {
		t.Fatalf("calls after a failing check: %d", fake.Calls())
	}

	// Without an embedder there is nothing to check: 409, and no call.
	fi.embedder = nil
	fi.status.Embedding = index.EmbeddingStatus{Provider: "none"}
	if w := uiCallControl(t, h, "POST", "/index/embedding/check", "{}"); w.Code != 409 {
		t.Fatalf("check without a provider: %d %s", w.Code, w.Body)
	}
}

// TestEmbeddingStatusWhenIndexDisabled: no index answers {"enabled":false}
// the way /index/status does, and the check route 404s; an index without
// a provider answers enabled:false with the provider named.
func TestEmbeddingStatusWhenIndexDisabled(t *testing.T) {
	f := newFixture(t)
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/index/embedding", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/index/embedding/check", "{}"); w.Code != 404 {
		t.Fatalf("check while disabled: %d", w.Code)
	}

	_, fi, h2 := indexServer(t)
	fi.status.Embedding = index.EmbeddingStatus{Provider: "none"}
	w = uiCallControl(t, h2, "GET", "/index/embedding", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res EmbeddingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Enabled || res.Provider != "none" || res.APIKeyConfigured {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestStatusCarriesTheEmbeddingLine: the /status index line the metrics
// and overview cards read from carries the vector count, the cap and the
// embedding block, so the console can draw "vectors N / max_chunks"
// without a second request.
func TestStatusCarriesTheEmbeddingLine(t *testing.T) {
	f, _, _, h := embeddingServer(t)
	st := NewServer(f.coll).collector.Collect(context.Background(), i18n.EN)
	if st.Index == nil || st.Index.Vectors != 10 || st.Index.MaxChunks != 200000 || st.Index.Embedding.Provider != "openai" || !st.Index.Embedding.Healthy {
		t.Fatalf("%+v", st.Index)
	}
	w := uiCallControl(t, h, "GET", "/metrics", "")
	if !strings.Contains(w.Body.String(), "cloudfs_index_vectors 10") || !strings.Contains(w.Body.String(), "cloudfs_index_embed_pending 2") {
		t.Fatalf("metrics: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "sk-live-topsecret") {
		t.Fatal("metrics leak the key")
	}
}
