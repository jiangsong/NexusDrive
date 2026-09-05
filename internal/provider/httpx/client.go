// Package httpx is the shared HTTP client every provider uses. It applies the
// proxy rules, the per-(remote, class) rate limiter and the circuit breaker in
// one place, so a driver only describes requests (docs/DESIGN.md §4.2).
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// Client wraps http.Client with limiting, breaking and error classification.
type Client struct {
	http     *http.Client
	remote   string
	account  string
	limiters *ratelimit.Registry
	policy   retry.Policy
	// UserAgent is sent on every request; several Chinese providers require a
	// specific value for downloads to work at all.
	UserAgent string
}

// Options configures a Client.
type Options struct {
	// HTTP is the underlying client, normally proxy.Manager.Client(override).
	HTTP *http.Client
	// Remote and Account key the rate limiter and breaker.
	Remote  string
	Account string
	// Limiters is optional; without it no rate limiting is applied.
	Limiters  *ratelimit.Registry
	Policy    retry.Policy
	UserAgent string
}

// New builds a Client.
func New(opt Options) *Client {
	h := opt.HTTP
	if h == nil {
		h = &http.Client{Timeout: 60 * time.Second}
	}
	p := opt.Policy
	if p.MaxAttempts == 0 {
		p = retry.Policy{Backoff: retry.DefaultBackoff, MaxAttempts: 4}
	}
	return &Client{
		http: h, remote: opt.Remote, account: opt.Account,
		limiters: opt.Limiters, policy: p, UserAgent: opt.UserAgent,
	}
}

// Request describes one call.
type Request struct {
	Method string
	URL    string
	// Class selects which rate-limit bucket applies.
	Class ratelimit.Class
	// Header entries are added to the request.
	Header http.Header
	// Body, when non-nil, is sent. Provide GetBody for retries.
	Body io.Reader
	// GetBody re-creates the body for a retry. Without it, a request with a
	// body is not retried.
	GetBody func() (io.ReadCloser, error)
	// JSON, when non-nil, is marshalled as the body with the JSON content type
	// and is always retryable.
	JSON any
	// Form is sent as application/x-www-form-urlencoded when non-empty.
	Form map[string]string
	// ExpectStatus lists acceptable status codes. Empty means 2xx.
	ExpectStatus []int
	// Stream keeps the response body open for the caller to read and close.
	Stream bool
}

// Response is a completed call.
type Response struct {
	Status int
	Header http.Header
	// Body is set when Request.Stream is true; the caller must close it.
	Body io.ReadCloser
	// Bytes holds the whole body when Stream is false.
	Bytes []byte
}

// StatusError carries an HTTP status so retry.Classify can act on it.
type StatusError struct {
	Code   int
	Status string
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("http %s for %s: %s", e.Status, e.URL, body)
}

// HTTPStatus implements retry.HTTPStatuser.
func (e *StatusError) HTTPStatus() int { return e.Code }

// Unwrap maps well-known statuses onto provider sentinels so callers can use
// errors.Is without inspecting codes.
func (e *StatusError) Unwrap() error {
	switch e.Code {
	case http.StatusUnauthorized:
		return provider.ErrAuth
	case http.StatusForbidden, http.StatusGone:
		return provider.ErrLinkExpired
	case http.StatusNotFound:
		return provider.ErrNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return provider.ErrConflict
	case http.StatusTooManyRequests:
		return provider.ErrRateLimited
	}
	if e.Code >= 500 {
		return provider.ErrTransient
	}
	return nil
}

// Do performs the request with limiting, retries and classification.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	if c.limiters != nil {
		if b := c.limiters.Breaker(c.remote, c.account); b.Open() {
			return nil, fmt.Errorf("%w: %s is paused until %s after repeated risk-control responses",
				provider.ErrRiskControl, c.remote, b.OpenUntil().Format(time.RFC3339))
		}
	}
	var resp *Response
	policy := c.policy
	// A caller-owned streaming body cannot be replayed safely. Reusing it on a
	// retry sends whatever unread suffix remains (usually an empty body), which
	// can silently corrupt PUTs and multipart appends. JSON, Form, and explicit
	// GetBody requests remain retryable because buildBody can reconstruct them.
	if r.Body != nil && r.GetBody == nil && r.JSON == nil && len(r.Form) == 0 {
		policy.MaxAttempts = 1
	}
	err := policy.Do(ctx, func() error {
		var e error
		resp, e = c.once(ctx, r)
		return e
	}, func(err error, class retry.Class, _ time.Duration) {
		c.record(err, class, r.Class)
	})
	if err != nil {
		c.record(err, retry.Classify(err), r.Class)
		return nil, err
	}
	c.succeed(r.Class)
	return resp, nil
}

func (c *Client) record(err error, class retry.Class, reqClass ratelimit.Class) {
	if c.limiters == nil {
		return
	}
	switch class {
	case retry.ClassRetryable:
		c.limiters.Limiter(ratelimit.Key{Remote: c.remote, Account: c.account, Class: reqClass}).
			Throttled(retry.RetryAfter(err))
	case retry.ClassRiskControl:
		c.limiters.Limiter(ratelimit.Key{Remote: c.remote, Account: c.account, Class: reqClass}).
			Throttled(retry.RetryAfter(err))
		c.limiters.Breaker(c.remote, c.account).Trip()
	}
}

func (c *Client) succeed(class ratelimit.Class) {
	if c.limiters == nil {
		return
	}
	c.limiters.Limiter(ratelimit.Key{Remote: c.remote, Account: c.account, Class: class}).Succeeded()
}

func (c *Client) once(ctx context.Context, r Request) (*Response, error) {
	if c.limiters != nil {
		key := ratelimit.Key{Remote: c.remote, Account: c.account, Class: r.Class}
		if err := c.limiters.Limiter(key).Wait(ctx); err != nil {
			return nil, err
		}
	}
	body, getBody, contentType, err := buildBody(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, err
	}
	if getBody != nil {
		req.GetBody = getBody
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if !statusOK(httpResp.StatusCode, r.ExpectStatus) {
		defer httpResp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		se := &StatusError{Code: httpResp.StatusCode, Status: httpResp.Status, URL: errorURL(r.URL), Body: string(snippet)}
		if ra := parseRetryAfter(httpResp.Header.Get("Retry-After")); ra > 0 {
			return nil, &provider.RetryAfterError{Err: se, RetryAfter: ra}
		}
		return nil, se
	}
	out := &Response{Status: httpResp.StatusCode, Header: httpResp.Header}
	if r.Stream {
		out.Body = httpResp.Body
		return out, nil
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	out.Bytes = data
	return out, nil
}

// errorURL removes query credentials carried by presigned download and upload
// session URLs before an error can reach logs, journals, metrics, or a user.
func errorURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func buildBody(r Request) (io.Reader, func() (io.ReadCloser, error), string, error) {
	switch {
	case r.JSON != nil:
		b, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, nil, "", err
		}
		return bytes.NewReader(b), func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(b)), nil
		}, "application/json", nil
	case len(r.Form) > 0:
		var sb strings.Builder
		first := true
		for k, v := range r.Form {
			if !first {
				sb.WriteByte('&')
			}
			first = false
			sb.WriteString(urlEncode(k))
			sb.WriteByte('=')
			sb.WriteString(urlEncode(v))
		}
		s := sb.String()
		return strings.NewReader(s), func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(s)), nil
		}, "application/x-www-form-urlencoded", nil
	case r.Body != nil:
		if r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, nil, "", err
			}
			return body, r.GetBody, "", nil
		}
		return r.Body, r.GetBody, "", nil
	default:
		return nil, nil, "", nil
	}
}

func urlEncode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			sb.WriteByte(ch)
		case ch == ' ':
			sb.WriteByte('+')
		default:
			fmt.Fprintf(&sb, "%%%02X", ch)
		}
	}
	return sb.String()
}

func statusOK(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 300
	}
	for _, e := range expect {
		if code == e {
			return true
		}
	}
	return false
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// JSON performs a request and decodes the response into out.
func (c *Client) JSON(ctx context.Context, r Request, out any) error {
	resp, err := c.Do(ctx, r)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Bytes, out); err != nil {
		snippet := string(resp.Bytes)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("httpx: decode %s response: %w (body: %s)", r.URL, err, snippet)
	}
	return nil
}

// XML performs a request and decodes the response into out.
func (c *Client) XML(ctx context.Context, r Request, out any) error {
	resp, err := c.Do(ctx, r)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(resp.Bytes, out); err != nil {
		snippet := string(resp.Bytes)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("httpx: decode %s response: %w (body: %s)", r.URL, err, snippet)
	}
	return nil
}

// RangeHeader builds a byte-range header value.
func RangeHeader(off, n int64) string {
	if n <= 0 {
		return fmt.Sprintf("bytes=%d-", off)
	}
	return fmt.Sprintf("bytes=%d-%d", off, off+n-1)
}

// FromConfig returns the shared client the daemon placed in a driver's config
// under provider.ConfigHTTPClient, or a new client built from fallback when
// the key is absent. The bool reports whether the shared client was used;
// drivers do not normally need it, but tests assert on it.
//
// Every driver factory should start with this call: it is what makes proxy
// routing, rate limiting and circuit breaking apply to that driver.
func FromConfig(cfg map[string]any, fallback Options) (*Client, bool) {
	if v, ok := provider.HTTPClientFrom(cfg); ok {
		if c, ok := v.(*Client); ok && c != nil {
			return c, true
		}
	}
	return New(fallback), false
}

// RangeBody adapts a response body to the byte range that was requested.
//
// A server that honours Range answers 206 and the body is already the window.
// A server that ignores it answers 200 with the whole object, and returning
// that body unchanged would hand the caller bytes from offset 0 while claiming
// they start at off. The block cache would store them under the wrong index,
// which is silent corruption rather than a visible failure. So on a 200 the
// leading bytes are discarded and the result is bounded.
//
// Every driver that accepts both 200 and 206 must route its body through this.
func RangeBody(resp *Response, off, n int64) (io.ReadCloser, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("httpx: no response body to range over")
	}
	body := resp.Body
	if resp.Status == http.StatusOK && off > 0 {
		if _, err := io.CopyN(io.Discard, body, off); err != nil {
			body.Close()
			return nil, fmt.Errorf("httpx: server ignored Range and the object is shorter than offset %d: %w", off, err)
		}
	}
	if n > 0 {
		return &limitedBody{Reader: io.LimitReader(body, n), closer: body}, nil
	}
	return body, nil
}

// limitedBody bounds a stream while still closing the underlying body.
type limitedBody struct {
	io.Reader
	closer io.Closer
}

func (l *limitedBody) Close() error { return l.closer.Close() }
