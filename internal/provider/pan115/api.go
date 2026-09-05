package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// apiEnvelope is the shape every 115 Open response shares. The platform is not
// consistent about types: `state` arrives as a bool, an int or a string
// depending on the endpoint, and `code`/`errno` are sometimes strings, so the
// flexible scalars below absorb the difference instead of failing the decode.
type apiEnvelope struct {
	State   flexBool        `json:"state"`
	Code    flexInt         `json:"code"`
	Errno   flexInt         `json:"errno"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`

	// List endpoints put paging metadata beside `data` rather than inside it.
	Count  flexInt `json:"count"`
	Offset flexInt `json:"offset"`
	Limit  flexInt `json:"limit"`
}

// ok reports whether the envelope describes a successful call. 115 signals
// success with state=true, and some endpoints only set code/errno to 0.
func (e *apiEnvelope) ok() bool {
	if bool(e.State) {
		return true
	}
	return int(e.Code) == 0 && int(e.Errno) == 0 && e.Message == "" && e.Error == ""
}

// errCode returns the numeric error the envelope carries, preferring `code`.
func (e *apiEnvelope) errCode() int {
	if int(e.Code) != 0 {
		return int(e.Code)
	}
	return int(e.Errno)
}

// errMessage returns whichever human-readable field the endpoint filled in.
func (e *apiEnvelope) errMessage() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Error
}

// Error codes 115 Open returns. Only the ones a driver must act on are named.
//
// UNVERIFIED: the 401401xx token codes are taken from the 115 Open error table
// and community SDKs; confirm each against a live account before relying on
// the exact number. The mapping degrades safely either way, because unknown
// codes fall through to a terminal error rather than to a retry loop.
const (
	// codeAccessTokenInvalid and codeAccessTokenExpired mean "refresh once".
	codeAccessTokenInvalid = 40140116
	codeAccessTokenExpired = 40140117
	// codeRefreshTokenExpired and codeRefreshTokenInvalid are unrecoverable:
	// the user has to re-authorise the app.
	codeRefreshTokenExpired = 40140119
	codeRefreshTokenInvalid = 40140125
	// codeNeedAppVerify is the classic "请在 115 客户端中验证" response. 115
	// returns it when it suspects a third-party client; see mapError.
	codeNeedAppVerify = 911
	// codeTooFrequent is 115's "操作频繁" throttle.
	codeTooFrequent = 990009
	// codeDirExists is returned by /open/folder/add for a duplicate name.
	// UNVERIFIED: 20004 comes from the classic web API; the Open platform may
	// use a different number. A miss only costs a less specific error.
	codeDirExists = 20004
	// codeNotFound is returned when a file_id no longer exists.
	// UNVERIFIED: exact value.
	codeNotFound = 20130827
)

// riskWords are message fragments 115 uses when it wants the account to prove
// itself in the official client. They must map to provider.ErrRiskControl:
// the upper layer circuit-breaks the account on that signal instead of
// hammering the API until 115 bans it (docs/DESIGN.md §4.2).
var riskWords = []string{
	"permissiondenied",
	"permission denied",
	"需要验证",
	"验证账号",
	"账号验证",
	"风控",
	"app verification",
	"abnormal",
}

// rateWords are message fragments that mean "slow down" rather than "you are
// suspected of being a bot".
var rateWords = []string{
	"频繁",
	"too many",
	"rate limit",
}

// mapError translates a 115 Open error envelope into a provider sentinel so
// internal/net/retry can classify it without knowing anything about 115.
func mapError(code int, message string) error {
	base := fmt.Errorf("pan115: api error %d: %s", code, message)
	lower := strings.ToLower(message)
	switch code {
	case codeNeedAppVerify:
		return fmt.Errorf("%w: %w", provider.ErrRiskControl, base)
	case codeTooFrequent:
		return fmt.Errorf("%w: %w", provider.ErrRateLimited, base)
	case codeAccessTokenInvalid, codeAccessTokenExpired,
		codeRefreshTokenExpired, codeRefreshTokenInvalid:
		return fmt.Errorf("%w: %w", provider.ErrAuth, base)
	case codeDirExists:
		return fmt.Errorf("%w: %w", provider.ErrExists, base)
	case codeNotFound:
		return fmt.Errorf("%w: %w", provider.ErrNotFound, base)
	}
	for _, w := range riskWords {
		if strings.Contains(lower, w) {
			return fmt.Errorf("%w: %w", provider.ErrRiskControl, base)
		}
	}
	for _, w := range rateWords {
		if strings.Contains(lower, w) {
			return fmt.Errorf("%w: %w", provider.ErrRateLimited, base)
		}
	}
	if strings.Contains(lower, "token") && (strings.Contains(lower, "invalid") || strings.Contains(lower, "expire")) {
		return fmt.Errorf("%w: %w", provider.ErrAuth, base)
	}
	if strings.Contains(lower, "不存在") || strings.Contains(lower, "not exist") {
		return fmt.Errorf("%w: %w", provider.ErrNotFound, base)
	}
	if strings.Contains(lower, "已存在") || strings.Contains(lower, "exists") {
		return fmt.Errorf("%w: %w", provider.ErrExists, base)
	}
	return base
}

// call performs an authenticated Open-API request and decodes `data` into out.
// A 401-style answer triggers exactly one token refresh followed by one retry;
// a second failure surfaces as provider.ErrAuth so the mount goes read-only
// rather than spinning on a dead token.
func (p *Pan115) call(ctx context.Context, r httpx.Request, out any) (*apiEnvelope, error) {
	env, err := p.callOnce(ctx, r, out)
	if err == nil || !errors.Is(err, provider.ErrAuth) {
		return env, err
	}
	// Refreshing is pointless when the refresh token itself is dead.
	if env != nil {
		switch env.errCode() {
		case codeRefreshTokenExpired, codeRefreshTokenInvalid:
			return env, err
		}
	}
	if rerr := p.refreshToken(ctx); rerr != nil {
		return env, fmt.Errorf("%w (refresh failed: %v)", err, rerr)
	}
	return p.callOnce(ctx, r, out)
}

func (p *Pan115) callOnce(ctx context.Context, r httpx.Request, out any) (*apiEnvelope, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := p.http.Do(ctx, r)
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(resp.Bytes, &env); err != nil {
		return nil, fmt.Errorf("pan115: decode %s: %w", r.URL, err)
	}
	if !env.ok() {
		return &env, mapError(env.errCode(), env.errMessage())
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &env, fmt.Errorf("pan115: decode %s data: %w", r.URL, err)
		}
	}
	return &env, nil
}

// flexInt decodes a JSON number, string or null into an int.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("pan115: %q is not a number", s)
	}
	*f = flexInt(int64(v))
	return nil
}

// flexInt64 decodes a JSON number, string or null into an int64. 115 reports
// file sizes as strings on some endpoints and as numbers on others.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("pan115: %q is not a number", s)
	}
	*f = flexInt64(int64(v))
	return nil
}

// flexString decodes a JSON string, number or null into a string. File ids
// arrive quoted from most endpoints and bare from a few.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*f = flexString(str)
		return nil
	}
	*f = flexString(s)
	return nil
}

// flexBool decodes true/false, 0/1 and "true"/"false" into a bool.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	switch strings.ToLower(s) {
	case "true", "1":
		*f = true
	case "false", "0", "null", "":
		*f = false
	default:
		return fmt.Errorf("pan115: %q is not a boolean", s)
	}
	return nil
}
