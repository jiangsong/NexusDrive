package httpx

import (
	"fmt"
	"net/http"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// RequestClassifier assigns an SDK-owned HTTP request to the same metadata,
// download, or upload buckets used by native httpx requests.
type RequestClassifier func(*http.Request) ratelimit.Class

// RawTransport lets an HTTP SDK keep its response/error parser while still
// inheriting CloudFS proxy routing, rate limiting, circuit breaking, and
// request accounting. It deliberately does not retry or consume non-2xx
// bodies: the SDK sees the original response and owns retry safety.
func (c *Client) RawTransport(classify RequestClassifier) http.RoundTripper {
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if classify == nil {
		classify = func(*http.Request) ratelimit.Class { return ratelimit.Meta }
	}
	return &rawTransport{client: c, base: base, classify: classify}
}

type rawTransport struct {
	client   *Client
	base     http.RoundTripper
	classify RequestClassifier
}

func (t *rawTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	class := t.classify(req)
	c := t.client
	if c.limiters != nil {
		if b := c.limiters.Breaker(c.remote, c.account); b.Open() {
			return nil, fmt.Errorf("%w: %s is paused after repeated risk-control responses", provider.ErrRiskControl, c.remote)
		}
		key := ratelimit.Key{Remote: c.remote, Account: c.account, Class: class}
		if err := c.limiters.Limiter(key).Wait(req.Context()); err != nil {
			return nil, err
		}
	}
	request := req.Clone(req.Context())
	request.Header = req.Header.Clone()
	if c.UserAgent != "" && request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := t.base.RoundTrip(request)
	if err != nil {
		c.record(err, retry.Classify(err), class)
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.succeed(class)
		return resp, nil
	}
	// Do not include a signed query string in diagnostics or limiter state.
	safeURL := request.URL.Scheme + "://" + request.URL.Host + request.URL.EscapedPath()
	statusErr := &StatusError{Code: resp.StatusCode, Status: resp.Status, URL: safeURL}
	classified := error(statusErr)
	if after := parseRetryAfter(resp.Header.Get("Retry-After")); after > 0 {
		classified = &provider.RetryAfterError{Err: statusErr, RetryAfter: after}
	}
	c.record(classified, retry.Classify(classified), class)
	return resp, nil
}

var _ http.RoundTripper = (*rawTransport)(nil)
