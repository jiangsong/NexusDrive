package embed

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/testx"
)

// testConfig is a validated embedding block pointing at srv.
func testConfig(t *testing.T, provider, baseURL string) config.IndexEmbedding {
	t.Helper()
	cfg := config.IndexEmbedding{Provider: provider, BaseURL: baseURL, Model: "test-model", Batch: 4, Concurrency: 1, QPS: 1000, Timeout: 5 * time.Second, Quantize: "int8"}
	if provider == "openai" {
		cfg.APIKey = "keyring:test"
		cfg.Dimensions = 8
	}
	return cfg
}

// vec returns a vector of dim floats whose first element identifies n, so
// tests can check ordering across batches.
func vec(dim int, n float64) []float32 {
	v := make([]float32, dim)
	v[0] = float32(n)
	return v
}

// openaiTestRequest is the wire shape the tests decode the request body into.
type openaiTestRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     *int     `json:"dimensions"`
}

func TestOpenAIBatchesAndSendsDimensions(t *testing.T) {
	var mu sync.Mutex
	var batches [][]string
	var auths []string
	var paths []string
	var dims []*int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		batches = append(batches, req.Input)
		auths = append(auths, r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		dims = append(dims, req.Dimensions)
		mu.Unlock()
		if req.Model != "test-model" || req.EncodingFormat != "float" {
			t.Errorf("request = %+v", req)
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		// Answer out of order on purpose: the client must sort by index.
		for i := len(req.Input) - 1; i >= 0; i-- {
			n, _ := strings.CutPrefix(req.Input[i], "t")
			var f float64
			for _, c := range n {
				f = f*10 + float64(c-'0')
			}
			data = append(data, item{Object: "embedding", Index: i, Embedding: vec(8, f)})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "test-model"})
	}))
	defer srv.Close()

	c, err := New(Options{Config: testConfig(t, "openai", srv.URL+"/v1"), APIKey: "sk-secret"})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}
	out, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(out), len(texts))
	}
	for i, v := range out {
		if len(v) != 8 || v[0] != float32(i) {
			t.Fatalf("vector %d = %v; order across batches lost", i, v)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 3 || len(batches[0]) != 4 || len(batches[1]) != 4 || len(batches[2]) != 2 {
		t.Fatalf("batches = %v, want 4+4+2", batches)
	}
	for i := range batches {
		if auths[i] != "Bearer sk-secret" {
			t.Fatalf("Authorization = %q", auths[i])
		}
		if paths[i] != "/v1/embeddings" {
			t.Fatalf("path = %q, want /v1/embeddings", paths[i])
		}
		if dims[i] == nil || *dims[i] != 8 {
			t.Fatalf("dimensions = %v, want 8", dims[i])
		}
	}
	if c.Model() != "test-model" || c.Dim() != 8 {
		t.Fatalf("model = %q dim = %d", c.Model(), c.Dim())
	}
	h := c.Status()
	if !h.Healthy || h.LastError != "" || h.Remote || h.Host != "127.0.0.1" {
		t.Fatalf("health = %+v", h)
	}
}

func TestOllamaUsesTheBatchEndpoint(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var inputs [][]string
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		inputs = append(inputs, req.Input)
		hadAuth = hadAuth || r.Header.Get("Authorization") != ""
		mu.Unlock()
		if req.Model != "test-model" {
			t.Errorf("model = %q", req.Model)
		}
		var embs [][]float32
		for i := range req.Input {
			embs = append(embs, vec(4, float64(i)))
		}
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "embeddings": embs})
	}))
	defer srv.Close()

	c, err := New(Options{Config: testConfig(t, "ollama", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Embed(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 || len(out[4]) != 4 || out[4][0] != 0 || out[3][0] != 3 {
		t.Fatalf("out = %v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/api/embed" || paths[1] != "/api/embed" {
		t.Fatalf("paths = %v, want two calls to the batch endpoint /api/embed", paths)
	}
	if len(inputs[0]) != 4 || len(inputs[1]) != 1 {
		t.Fatalf("inputs = %v, want 4+1", inputs)
	}
	if hadAuth {
		t.Fatal("ollama request carried an Authorization header with no key configured")
	}
	if c.Dim() != 4 {
		t.Fatalf("dim = %d, want 4 probed from the first response", c.Dim())
	}
}

func TestDimIsProbedOnceAndPinned(t *testing.T) {
	var dim atomic.Int32
	dim.Store(6)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var embs [][]float32
		for range req.Input {
			embs = append(embs, vec(int(dim.Load()), 1))
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embs})
	}))
	defer srv.Close()

	c, err := New(Options{Config: testConfig(t, "ollama", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Dim() != 0 {
		t.Fatalf("dim = %d before any call, want 0 (unknown)", c.Dim())
	}
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if c.Dim() != 6 {
		t.Fatalf("dim = %d, want 6", c.Dim())
	}
	dim.Store(7)
	if _, err := c.Embed(context.Background(), []string{"y"}); err == nil {
		t.Fatal("a response with a different dimension was accepted")
	} else if !strings.Contains(err.Error(), "6") || !strings.Contains(err.Error(), "7") {
		t.Fatalf("error should name both dimensions: %v", err)
	}
	if c.Dim() != 6 {
		t.Fatalf("dim moved to %d; it must stay pinned at 6", c.Dim())
	}
	// A configured openai dimension that the server ignores is caught too.
	c2, err := New(Options{Config: testConfig(t, "openai", srv.URL), APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("server returning 7 dims for a configured 8 was accepted")
	}
}

func TestBreakerOpensAfterFiveServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusBadGateway)
	}))
	defer srv.Close()

	c, err := New(Options{Config: testConfig(t, "ollama", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := c.Embed(ctx, []string{"x"}); err == nil {
			t.Fatalf("call %d succeeded against a 502 server", i)
		}
		if got := hits.Load(); got != int32(i+1) {
			t.Fatalf("after call %d the server saw %d requests, want %d (no retries on 5xx)", i, got, i+1)
		}
	}
	before := time.Now()
	for i := 0; i < 3; i++ {
		_, err := c.Embed(ctx, []string{"x"})
		if !errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("call while open: err = %v, want ErrBreakerOpen", err)
		}
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("server saw %d requests, want exactly 5: an open breaker sends nothing", got)
	}
	h := c.Status()
	if h.Healthy {
		t.Fatalf("healthy = true with the breaker open: %+v", h)
	}
	if h.LastError == "" || !strings.Contains(h.LastError, "502") {
		t.Fatalf("last error = %q, want the 502", h.LastError)
	}
	until := h.BreakerOpenUntil.Sub(before)
	if until < 55*time.Second || until > 65*time.Second {
		t.Fatalf("breaker open for %s, want about 60s", until)
	}
}

func TestThrottledHonoursRetryAfter(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("timing assertion; meaningless under the race detector")
	}
	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		n := len(stamps)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var embs [][]float32
		for range req.Input {
			embs = append(embs, vec(3, 1))
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embs})
	}))
	defer srv.Close()

	c, err := New(Options{Config: testConfig(t, "ollama", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("a 429 with Retry-After should be retried after the wait: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(stamps))
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 900*time.Millisecond {
		t.Fatalf("retry came after %s, want at least the 1s Retry-After", gap)
	}
	if h := c.Status(); !h.Healthy || h.LastError != "" {
		t.Fatalf("a throttle followed by success should leave the endpoint healthy: %+v", h)
	}
	// A 429 does not feed the breaker: it is a pace signal, not an outage.
	if h := c.Status(); !h.BreakerOpenUntil.IsZero() {
		t.Fatalf("breaker opened on a 429: %+v", h)
	}
}

func TestFakeIsDeterministicAndNormalised(t *testing.T) {
	f := NewFake(16)
	a, err := f.Embed(context.Background(), []string{"hello", "world", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || len(a[0]) != 16 {
		t.Fatalf("shape = %d x %d", len(a), len(a[0]))
	}
	for i := range a[0] {
		if a[0][i] != a[2][i] || a[0][i] != b[0][i] {
			t.Fatalf("same text embedded differently: %v vs %v vs %v", a[0], a[2], b[0])
		}
	}
	same := true
	for i := range a[0] {
		if a[0][i] != a[1][i] {
			same = false
		}
	}
	if same {
		t.Fatal("different texts got the same vector")
	}
	for _, v := range a {
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(sum)-1) > 1e-4 {
			t.Fatalf("norm = %f, want 1", math.Sqrt(sum))
		}
	}
	if f.Calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.Calls())
	}
	if got := f.Batches(); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("batches = %v, want [3 1]", got)
	}
	if f.Dim() != 16 || f.Model() == "" {
		t.Fatalf("dim = %d model = %q", f.Dim(), f.Model())
	}
	if h := f.Status(); !h.Healthy || h.Remote {
		t.Fatalf("health = %+v", h)
	}
	var _ Embedder = f
	// Fault injection for the worker tests: an open breaker.
	until := time.Now().Add(time.Minute)
	f.SetBreakerOpenUntil(until)
	if _, err := f.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen", err)
	}
	if h := f.Status(); h.Healthy || !h.BreakerOpenUntil.Equal(until) {
		t.Fatalf("health = %+v", h)
	}
	f.SetBreakerOpenUntil(time.Time{})
	f.SetError(errors.New("injected"))
	if _, err := f.Embed(context.Background(), []string{"x"}); err == nil || err.Error() != "injected" {
		t.Fatalf("err = %v", err)
	}
	if h := f.Status(); h.Healthy || h.LastError != "injected" {
		t.Fatalf("health = %+v", h)
	}
	f.SetError(nil)
	if _, err := f.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if h := f.Status(); !h.Healthy || h.LastError != "" {
		t.Fatalf("health = %+v", h)
	}
}

func TestNoneProviderYieldsNoEmbedder(t *testing.T) {
	c, err := New(Options{Config: config.IndexEmbedding{Provider: "none"}})
	if err != nil || c != nil {
		t.Fatalf("New(none) = %v, %v; want nil, nil", c, err)
	}
	c, err = New(Options{Config: config.IndexEmbedding{}})
	if err != nil || c != nil {
		t.Fatalf("New(empty) = %v, %v; want nil, nil", c, err)
	}
	if _, err := New(Options{Config: config.IndexEmbedding{Provider: "cohere", Model: "m", BaseURL: "http://127.0.0.1:1"}}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	// The privacy gate holds even for a hand-built config that skipped
	// config.Validate.
	if _, err := New(Options{Config: config.IndexEmbedding{Provider: "openai", Model: "m", BaseURL: "https://api.openai.com/v1"}, APIKey: "k"}); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("err = %v, want ErrRemoteDisabled", err)
	}
	if _, err := New(Options{Config: config.IndexEmbedding{Provider: "openai", Model: "m", BaseURL: "http://127.0.0.1:1/v1"}}); err == nil {
		t.Fatal("openai without a key accepted")
	}
}

// A connection failure is an outage like a 5xx: five in a row open the
// breaker. Concurrency above one must not lose the order of the vectors.
func TestConnectionFailuresFeedTheBreakerAndConcurrencyKeepsOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var embs [][]float32
		for _, s := range req.Input {
			embs = append(embs, vec(2, float64(len(s))))
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embs})
	}))
	cfg := testConfig(t, "ollama", srv.URL)
	cfg.Batch = 1
	cfg.Concurrency = 4
	c, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for i := 0; i < 20; i++ {
		texts = append(texts, strings.Repeat("x", i))
	}
	out, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range out {
		if v[0] != float32(i) {
			t.Fatalf("vector %d = %v: order lost across concurrent batches", i, v)
		}
	}
	srv.Close()
	for i := 0; i < 5; i++ {
		if _, err := c.Embed(context.Background(), []string{"x"}); err == nil || errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("call %d against a closed server: err = %v", i, err)
		}
	}
	if _, err := c.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen after five connection failures", err)
	}
	if h := c.Status(); h.Healthy || h.BreakerOpenUntil.IsZero() {
		t.Fatalf("health = %+v", h)
	}
}
