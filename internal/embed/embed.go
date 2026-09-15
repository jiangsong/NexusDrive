// Package embed turns chunk text into vectors through an OpenAI-compatible
// or ollama endpoint (docs/agent-roadmap.md §3.5). It is the one place that
// knows the wire formats; the index worker and hybrid search only see
// Embedder and Health.
//
// The client follows the provider idiom: requests go through httpx (so the
// proxy rules apply to the http.Client handed in), a private ratelimit
// limiter paces them at index.embedding.qps and halves on 429, and a private
// breaker opens for a minute after five failures with no success in between
// so an endpoint that is down is not hammered by the worker. A 429 is a pace
// signal and is retried after its Retry-After; a 5xx or a connection failure
// is an outage signal and is not retried here — the worker retries later,
// which keeps the request count the breaker sees equal to the failures.
package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider/httpx"
)

// Embedder is what the index needs from an embedding endpoint.
type Embedder interface {
	// Embed returns one vector per text, in order. Every vector has Dim()
	// elements once Dim is known.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	// Dim is the vector size: 0 until the first successful call probes it,
	// pinned afterwards. A later response of a different size is an error,
	// never a silent change.
	Dim() int
}

// Health is the endpoint state the index status and console report.
type Health struct {
	// Healthy is false while the breaker is open or the last call failed.
	Healthy bool
	// LastError is the most recent failure, cleared by the next success.
	LastError string
	// BreakerOpenUntil is when requests resume; zero while closed.
	BreakerOpenUntil time.Time
	// Remote reports that chunk text leaves this machine, and Host is where
	// it goes.
	Remote bool
	Host   string
}

// Reporter is implemented by every Embedder in this package.
type Reporter interface {
	Status() Health
}

var (
	// ErrBreakerOpen is returned without a request while the endpoint is
	// paused after repeated failures.
	ErrBreakerOpen = errors.New("embed: endpoint paused after repeated failures")
	// ErrRemoteDisabled is returned by New for an endpoint outside this
	// machine when allow_remote is false. config.Validate rejects such a
	// block already; this is the guard for a hand-built config.
	ErrRemoteDisabled = errors.New("embed: endpoint is outside this machine and allow_remote is false")
)

// Breaker tuning: five failures with no success between them open the
// breaker for a minute (TODO.md T-39).
const (
	breakerThreshold = 5
	breakerWindow    = time.Hour
	breakerCooldown  = 60 * time.Second
	// throttleAttempts bounds how many 429s one batch waits through.
	throttleAttempts = 3
)

// Options configures New.
type Options struct {
	// Config is a validated index.embedding block.
	Config config.IndexEmbedding
	// APIKey is the resolved secret (SecretStore.Get of Config.APIKey), or
	// empty for an endpoint without authentication.
	APIKey string
	// Client is the http.Client to send through, normally
	// proxy.Manager.Client(Config.Proxy, Config.Timeout) so the proxy rules
	// apply. Nil with Proxy set builds exactly that; nil with no Proxy uses
	// a direct client (tests).
	Client *http.Client
	Proxy  *proxy.Manager
}

// wire is the provider-specific half: how one batch is posted and decoded.
type wire interface {
	embed(ctx context.Context, c *Client, texts []string) ([][]float32, error)
}

// Client is the Embedder for a configured endpoint. It is safe for
// concurrent use; the index worker and search share one.
type Client struct {
	cfg     config.IndexEmbedding
	apiKey  string
	http    *httpx.Client
	wire    wire
	limiter *ratelimit.Limiter
	breaker *ratelimit.Breaker
	sem     chan struct{}

	mu      sync.Mutex
	dim     int
	lastErr string
}

// New builds the client for cfg. Provider none yields (nil, nil): callers
// keep the *Client nil check before wrapping it in an Embedder, because a
// nil *Client inside a non-nil interface is not "no embedding".
func New(opt Options) (*Client, error) {
	cfg := opt.Config
	if !cfg.Enabled() {
		return nil, nil
	}
	var w wire
	switch cfg.Provider {
	case "openai":
		w = openaiWire{}
	case "ollama":
		w = ollamaWire{}
	default:
		return nil, fmt.Errorf("embed: unknown provider %q", cfg.Provider)
	}
	if cfg.Model == "" {
		return nil, errors.New("embed: model is required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("embed: base_url is required")
	}
	if cfg.Provider == "openai" && opt.APIKey == "" {
		return nil, errors.New("embed: openai needs an api_key; run cloudfs index auth")
	}
	if cfg.RemoteEndpoint() && !cfg.AllowRemote {
		return nil, fmt.Errorf("%w: %s", ErrRemoteDisabled, cfg.EndpointHost())
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 64
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.QPS <= 0 {
		cfg.QPS = 4
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	hc := opt.Client
	if hc == nil && opt.Proxy != nil {
		hc = opt.Proxy.Client(cfg.Proxy, cfg.Timeout)
	}
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		cfg:    cfg,
		apiKey: opt.APIKey,
		wire:   w,
		// One attempt per request: retries and their accounting are the
		// client's own (see the package comment), so httpx must not add
		// its own backoff underneath.
		http: httpx.New(httpx.Options{
			HTTP:      hc,
			Remote:    "embedding",
			Policy:    retry.Policy{Backoff: retry.DefaultBackoff, MaxAttempts: 1},
			UserAgent: "cloudfs/0.1",
		}),
		limiter: ratelimit.New(ratelimit.Options{Rate: cfg.QPS, MinRate: cfg.QPS / 8}),
		breaker: ratelimit.NewBreaker(ratelimit.BreakerOptions{Threshold: breakerThreshold, Window: breakerWindow, Cooldown: breakerCooldown}),
		sem:     make(chan struct{}, cfg.Concurrency),
	}, nil
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// Dim returns the probed vector size, 0 before the first success.
func (c *Client) Dim() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dim
}

// Status reports the endpoint state for index status and the console.
func (c *Client) Status() Health {
	c.mu.Lock()
	lastErr := c.lastErr
	c.mu.Unlock()
	until := c.breaker.OpenUntil()
	return Health{
		Healthy:          until.IsZero() && lastErr == "",
		LastError:        lastErr,
		BreakerOpenUntil: until,
		Remote:           c.cfg.RemoteEndpoint(),
		Host:             c.cfg.EndpointHost(),
	}
}

// Embed posts texts in batches of Config.Batch, at most Config.Concurrency
// at a time, and returns their vectors in order. The first error cancels
// the remaining batches.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if until := c.breaker.OpenUntil(); !until.IsZero() {
		return nil, fmt.Errorf("%w until %s", ErrBreakerOpen, until.Format(time.RFC3339))
	}
	out := make([][]float32, len(texts))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}
	for start := 0; start < len(texts) && ctx.Err() == nil; start += c.cfg.Batch {
		end := min(start+c.cfg.Batch, len(texts))
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-c.sem }()
			vecs, err := c.batch(ctx, texts[start:end])
			if err != nil {
				fail(err)
				return
			}
			copy(out[start:end], vecs)
		}(start, end)
	}
	wg.Wait()
	errMu.Lock()
	defer errMu.Unlock()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// batch sends one request, waiting through the limiter and any 429
// Retry-After, then checks the shape of the answer and pins the dimension.
func (c *Client) batch(ctx context.Context, texts []string) ([][]float32, error) {
	var vecs [][]float32
	for attempt := 0; ; attempt++ {
		if until := c.breaker.OpenUntil(); !until.IsZero() {
			return nil, fmt.Errorf("%w until %s", ErrBreakerOpen, until.Format(time.RFC3339))
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		var err error
		vecs, err = c.wire.embed(reqCtx, c, texts)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			// The caller gave up; do not count its cancellation against
			// the endpoint.
			return nil, ctx.Err()
		}
		var se *httpx.StatusError
		throttled := errors.As(err, &se) && se.Code == http.StatusTooManyRequests
		if throttled {
			c.limiter.Throttled(retry.RetryAfter(err))
			if attempt+1 < throttleAttempts {
				continue
			}
		}
		c.fail(err, !throttled && retry.Classify(err) == retry.ClassRetryable)
		return nil, err
	}
	if err := c.check(vecs, len(texts)); err != nil {
		c.fail(err, false)
		return nil, err
	}
	c.limiter.Succeeded()
	c.breaker.Reset()
	c.mu.Lock()
	c.lastErr = ""
	c.mu.Unlock()
	return vecs, nil
}

// fail records err; outage is true for a 5xx or a connection failure, which
// counts toward opening the breaker.
func (c *Client) fail(err error, outage bool) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
	if outage {
		c.breaker.Trip()
	}
}

// check verifies one vector per text, all of one size, and that the size
// matches the configured openai dimensions and the size pinned earlier.
func (c *Client) check(vecs [][]float32, want int) error {
	if len(vecs) != want {
		return fmt.Errorf("embed: endpoint returned %d vectors for %d texts", len(vecs), want)
	}
	if want == 0 {
		return nil
	}
	dim := len(vecs[0])
	if dim == 0 {
		return errors.New("embed: endpoint returned an empty vector")
	}
	for i, v := range vecs {
		if len(v) != dim {
			return fmt.Errorf("embed: vector %d has %d dimensions, vector 0 has %d", i, len(v), dim)
		}
	}
	if c.cfg.Dimensions > 0 && dim != c.cfg.Dimensions {
		return fmt.Errorf("embed: endpoint returned %d dimensions, configured dimensions is %d; the model may not support the parameter", dim, c.cfg.Dimensions)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dim == 0 {
		c.dim = dim
	} else if c.dim != dim {
		return fmt.Errorf("embed: endpoint returned %d dimensions, pinned at %d since the first call; a model change needs a full re-embed", dim, c.dim)
	}
	return nil
}

// post sends one JSON request and decodes the reply through httpx, so the
// proxy rules, status classification and URL scrubbing all apply.
func (c *Client) post(ctx context.Context, url string, body, out any) error {
	h := make(http.Header)
	if c.apiKey != "" {
		h.Set("Authorization", "Bearer "+c.apiKey)
	}
	return c.http.JSON(ctx, httpx.Request{Method: http.MethodPost, URL: url, Header: h, JSON: body, Class: ratelimit.Meta}, out)
}
