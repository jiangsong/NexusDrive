package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// apiPrefix is the path prefix every metadata endpoint lives under.
const apiPrefix = "/1/clouddrive"

// envelopeStatuses are the HTTP statuses on which Quark still returns a JSON
// envelope that carries the real reason for the failure. They are passed as
// ExpectStatus so httpx hands us the body instead of turning the response into
// a StatusError. 429 and 5xx are deliberately absent: those carry no useful
// envelope and httpx's retry, AIMD and breaker handling is exactly right for
// them. 403 is included because Quark answers a risk-control challenge with
// 403 and a body, and httpx would otherwise map it to ErrLinkExpired, which is
// meaningless on the API host.
var envelopeStatuses = []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden}

// commonParams are the query parameters the Quark PC client sends on every
// call. The service answers 400 when they are missing.
//
// UNVERIFIED: uc_param_str is sent empty by the PC client and the server
// appears not to inspect it; confirm it is still ignored.
func commonParams() url.Values {
	v := url.Values{}
	v.Set("pr", "ucpro")
	v.Set("fr", "pc")
	v.Set("uc_param_str", "")
	return v
}

// apiURL builds an absolute URL for an API path from the injected base URL.
func (q *Quark) apiURL(path string, params url.Values) string {
	if params == nil {
		params = commonParams()
	}
	return q.base + apiPrefix + path + "?" + params.Encode()
}

// envelope is the response wrapper every Quark endpoint uses. `data` is left
// raw because it is an object on most endpoints and an array on the download
// endpoint.
type envelope struct {
	Status   int             `json:"status"`
	Code     int             `json:"code"`
	Message  string          `json:"message"`
	ReqID    string          `json:"req_id"`
	RiskType int             `json:"risk_type"`
	Data     json.RawMessage `json:"data"`
	Metadata struct {
		Total int `json:"_total"`
		Size  int `json:"_size"`
		Page  int `json:"_page"`
		Count int `json:"_count"`
	} `json:"metadata"`
}

// into decodes the envelope's data member into v. A missing or null data
// member is not an error: several endpoints answer with an empty body on
// success.
func (e *envelope) into(v any) error {
	if v == nil || len(e.Data) == 0 || string(e.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("quark: decode response data: %w (data: %s)", err, snippet(e.Data))
	}
	return nil
}

// APIError is a non-success Quark envelope. It keeps the server's own code and
// message so a bug report can be matched against the service, and unwraps to a
// provider sentinel so callers can use errors.Is.
type APIError struct {
	// HTTP is the transport status the envelope arrived with.
	HTTP int
	// Status is the status the envelope itself reports; Quark answers most
	// failures with HTTP 200 and the real status here.
	Status int
	// Code is the Quark application error code.
	Code int
	// Message is the server's human-readable message, usually Chinese.
	Message string
	// ReqID is Quark's request id, worth quoting in a bug report.
	ReqID string
	// Sentinel is the provider sentinel this error maps to, or nil when the
	// failure could not be classified.
	Sentinel error
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("quark: api error (http %d, status %d, code %d, req_id %q): %s",
		e.HTTP, e.Status, e.Code, e.ReqID, e.Message)
}

// Unwrap exposes the provider sentinel to errors.Is.
func (e *APIError) Unwrap() error { return e.Sentinel }

// HTTPStatus implements retry.HTTPStatuser. Quark reports most failures as
// HTTP 200 with the real status in the envelope, so the envelope wins when it
// carries one. An unclassifiable failure with no status at all is reported as
// 400 so the retry layer treats it as terminal: replaying an operation the
// server has already rejected is how an account walks into risk control.
func (e *APIError) HTTPStatus() int {
	if e.Status >= 400 {
		return e.Status
	}
	if e.HTTP >= 400 {
		return e.HTTP
	}
	return http.StatusBadRequest
}

// codeSentinels maps Quark application error codes onto provider sentinels.
//
// UNVERIFIED: every numeric code in this table. They are drawn from community
// clients and from observed responses, not from documentation, and must be
// confirmed against a live account. The message-keyword classification in
// classify() is the robust path; this table only sharpens it. An unknown code
// is never treated as success.
var codeSentinels = map[int]error{
	31001: provider.ErrAuth,        // 未登录 / cookie rejected
	31023: provider.ErrAuth,        // session expired
	32003: provider.ErrExists,      // duplicate name in the target directory
	41013: provider.ErrNotFound,    // unknown file or directory id
	41014: provider.ErrNotFound,    // parent directory gone
	42001: provider.ErrRateLimited, // request rate exceeded
	43001: provider.ErrRiskControl, // risk control: challenge required
	43002: provider.ErrRiskControl, // risk control: account temporarily blocked
}

// riskKeywords mark a risk-control response. They are checked before every
// other keyword class on purpose: mistaking risk control for a plain rate
// limit makes the upper layer retry, and retrying into Quark's risk control is
// what turns a captcha challenge into a multi-hour account block. 频繁
// ("too frequent") is included here rather than with the rate-limit keywords
// for the same reason — Quark escalates straight from that message to a block.
var riskKeywords = []string{"风控", "频繁", "验证", "captcha", "risk", "异常", "机器人"}

var authKeywords = []string{"未登录", "登录", "unauthorized", "not login", "invalid cookie"}

var notFoundKeywords = []string{"不存在", "已删除", "not found", "no such"}

var existsKeywords = []string{"已存在", "同名", "already exists", "duplicate"}

var rateKeywords = []string{"too many requests", "rate limit", "限流", "qps"}

// classify maps one failed envelope onto a provider sentinel, or nil when the
// failure cannot be recognised.
func classify(httpStatus int, e *envelope) error {
	// A non-zero risk_type is Quark's structured way of saying "this account
	// looks like a bot"; it can accompany an otherwise ordinary-looking body.
	// UNVERIFIED: whether risk_type appears at the envelope level, inside
	// data, or both.
	if e.RiskType != 0 {
		return provider.ErrRiskControl
	}
	if s, ok := codeSentinels[e.Code]; ok {
		return s
	}
	msg := strings.ToLower(e.Message)
	for _, kw := range riskKeywords {
		if strings.Contains(msg, kw) {
			return provider.ErrRiskControl
		}
	}
	for _, kw := range authKeywords {
		if strings.Contains(msg, kw) {
			return provider.ErrAuth
		}
	}
	for _, kw := range notFoundKeywords {
		if strings.Contains(msg, kw) {
			return provider.ErrNotFound
		}
	}
	for _, kw := range existsKeywords {
		if strings.Contains(msg, kw) {
			return provider.ErrExists
		}
	}
	for _, kw := range rateKeywords {
		if strings.Contains(msg, kw) {
			return provider.ErrRateLimited
		}
	}
	switch {
	case httpStatus == http.StatusUnauthorized, e.Status == http.StatusUnauthorized:
		return provider.ErrAuth
	case httpStatus == http.StatusForbidden, e.Status == http.StatusForbidden:
		// A bare 403 from the API host is never a legitimate answer: it is the
		// WAF, not the drive. Treat it as risk control so the account is
		// circuit-broken rather than retried.
		return provider.ErrRiskControl
	}
	return nil
}

// ok reports whether an envelope describes success.
func (e *envelope) ok() bool { return e.Code == 0 && (e.Status == 0 || e.Status == http.StatusOK) }

// call performs one API request: it attaches the rotating cookie and the
// client headers, decodes the envelope, absorbs any refreshed cookie and maps
// a failure onto a sentinel. out, when non-nil, receives the envelope's data
// member.
func (q *Quark) call(ctx context.Context, r httpx.Request, out any) (*envelope, error) {
	if len(r.ExpectStatus) == 0 {
		r.ExpectStatus = envelopeStatuses
	}
	r.Header = q.requestHeader(r.Header)
	resp, err := q.cli.Do(ctx, r)
	if err != nil {
		return nil, err
	}
	q.absorbCookies(resp.Header)

	var env envelope
	if err := json.Unmarshal(resp.Bytes, &env); err != nil {
		// A non-JSON body on one of the envelope statuses is the WAF's HTML
		// challenge page, not the drive. Classify it by status rather than
		// reporting a decode error the retry layer would replay.
		switch resp.Status {
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("quark: %s: %w: non-JSON body (%s)", r.URL, provider.ErrAuth, snippet(resp.Bytes))
		case http.StatusForbidden:
			return nil, fmt.Errorf("quark: %s: %w: non-JSON body, likely a risk-control challenge page (%s)",
				r.URL, provider.ErrRiskControl, snippet(resp.Bytes))
		}
		return nil, fmt.Errorf("quark: decode %s response: %w (body: %s)", r.URL, err, snippet(resp.Bytes))
	}
	if !env.ok() {
		return nil, &APIError{
			HTTP: resp.Status, Status: env.Status, Code: env.Code,
			Message: env.Message, ReqID: env.ReqID,
			Sentinel: classify(resp.Status, &env),
		}
	}
	if err := env.into(out); err != nil {
		return nil, err
	}
	return &env, nil
}

// meta and upload wrappers keep the rate-limit class next to the call site.
func (q *Quark) getJSON(ctx context.Context, url string, class ratelimit.Class, out any) (*envelope, error) {
	return q.call(ctx, httpx.Request{Method: http.MethodGet, URL: url, Class: class}, out)
}

func (q *Quark) postJSON(ctx context.Context, url string, class ratelimit.Class, body, out any) (*envelope, error) {
	return q.call(ctx, httpx.Request{Method: http.MethodPost, URL: url, Class: class, JSON: body}, out)
}

// requestHeader builds the header set every Quark request needs. The Cookie is
// taken from the rotating jar under the lock.
func (q *Quark) requestHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	if out.Get("User-Agent") == "" {
		out.Set("User-Agent", q.ua)
	}
	if out.Get("Referer") == "" {
		out.Set("Referer", DefaultReferer)
	}
	out.Set("Cookie", q.CookieHeader())
	return out
}

// setCookieHeader seeds the jar from a raw Cookie header value.
func (q *Quark) setCookieHeader(raw string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		q.putCookieLocked(strings.TrimSpace(name), strings.TrimSpace(value))
	}
}

func (q *Quark) putCookieLocked(name, value string) {
	if name == "" {
		return
	}
	if _, seen := q.cookieVals[name]; !seen {
		q.cookieOrder = append(q.cookieOrder, name)
	}
	q.cookieVals[name] = value
}

// CookieHeader returns the Cookie header value currently in effect, including
// any value the server has rotated. Callers that persist credentials should
// write this back to the config so a restart does not fall back to a stale
// cookie.
func (q *Quark) CookieHeader() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	parts := make([]string, 0, len(q.cookieOrder))
	for _, name := range q.cookieOrder {
		parts = append(parts, name+"="+q.cookieVals[name])
	}
	return strings.Join(parts, "; ")
}

// absorbCookies folds any Set-Cookie from a response back into the jar. Quark
// rotates the __puus session cookie on its own schedule and invalidates the
// previous value; a client that keeps replaying the cookie it was configured
// with is logged out within hours, so every response is inspected.
func (q *Quark) absorbCookies(h http.Header) {
	if len(h) == 0 {
		return
	}
	resp := http.Response{Header: h}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		q.putCookieLocked(c.Name, c.Value)
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
