package tianyi

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"

	"github.com/google/uuid"
)

// looseString decodes a value the 189 API spells inconsistently: file ids and
// result codes arrive as a JSON number in one endpoint and as a JSON string in
// the next, and the XML endpoints have no types at all. Decoding everything as
// a string keeps the call sites from caring.
type looseString string

// UnmarshalJSON accepts a string, a number, a bool or null.
func (l *looseString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null":
		*l = ""
		return nil
	case s[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*l = looseString(v)
		return nil
	default:
		*l = looseString(s)
		return nil
	}
}

// UnmarshalXML accepts the element's character data.
func (l *looseString) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var v string
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	*l = looseString(strings.TrimSpace(v))
	return nil
}

func (l looseString) String() string { return string(l) }

// envelope is the error envelope every 189 response carries. The JSON APIs use
// res_code/res_message, some of them use errorCode/errorMsg instead, and the
// XML upload host uses <code>/<message>. All four spellings are decoded so one
// mapper can handle every call site.
type envelope struct {
	ResCode    looseString `json:"res_code" xml:"res_code"`
	ResMessage string      `json:"res_message" xml:"res_message"`
	ErrorCode  looseString `json:"errorCode" xml:"errorCode"`
	ErrorMsg   string      `json:"errorMsg" xml:"errorMsg"`
	Code       looseString `json:"code" xml:"code"`
	Message    string      `json:"message" xml:"message"`
}

// apiErr reports the envelope's failure, if any, as a wrapped sentinel.
func (e *envelope) apiErr() error {
	for _, c := range []looseString{e.ResCode, e.ErrorCode, e.Code} {
		if codeOK(c) {
			continue
		}
		msg := firstNonEmpty(e.ResMessage, e.ErrorMsg, e.Message)
		return newAPIError(c.String(), msg)
	}
	return nil
}

// codeOK reports whether a result code means success. 189 signals success as
// an absent code, 0, or the string "SUCCESS" depending on the endpoint.
func codeOK(c looseString) bool {
	s := strings.TrimSpace(c.String())
	return s == "" || s == "0" || strings.EqualFold(s, "SUCCESS") || strings.EqualFold(s, "OK")
}

// responder is implemented by every response struct through the embedded
// envelope, so the request helpers can check for an in-band error before the
// caller looks at the decoded fields.
type responder interface{ apiErr() error }

// apiError is an in-band failure reported inside a 200 response.
type apiError struct {
	Code     string
	Message  string
	sentinel error
}

func newAPIError(code, message string) error {
	return &apiError{Code: code, Message: message, sentinel: classifyCode(code, message)}
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "no message"
	}
	return fmt.Sprintf("tianyi: api error %s: %s", e.Code, msg)
}

// Unwrap exposes the provider sentinel so callers can use errors.Is.
func (e *apiError) Unwrap() error { return e.sentinel }

// HTTPStatus reports 400 so retry.Classify treats an unrecognised API failure
// as terminal instead of retrying a call the server will keep rejecting. When
// a sentinel is set it wins: retry.Classify checks sentinels before it looks
// for an HTTP status.
func (e *apiError) HTTPStatus() int { return http.StatusBadRequest }

// errorClasses maps 189 result codes onto provider sentinels.
//
// Attested by public clients: InvalidSessionKey, UserInvalidOpenAppId,
// FileAlreadyExists, InfoSecurityErrorCode.
//
// UNVERIFIED: every other entry is a best reading of the codes 189 is reported
// to emit and needs checking against a live account. An unmapped code still
// fails the call (as a terminal error), so a wrong guess here degrades to "no
// special handling" rather than to data loss.
var errorClasses = map[string]error{
	// Session / credentials.
	"invalidsessionkey":     provider.ErrAuth,
	"invalidsessionsecret":  provider.ErrAuth,
	"userinvalidopenappid":  provider.ErrAuth,
	"invalidaccesstoken":    provider.ErrAuth,
	"invalidaccesstokenkey": provider.ErrAuth,
	"sessionexpired":        provider.ErrAuth,
	"logintimeout":          provider.ErrAuth,

	// Object lookup.
	"filenotfound":       provider.ErrNotFound,
	"filenotexists":      provider.ErrNotFound,
	"invalidfileid":      provider.ErrNotFound,
	"invalidfolderid":    provider.ErrNotFound,
	"sharefilenotexists": provider.ErrNotFound,

	// Name collisions.
	"filealreadyexists":          provider.ErrExists,
	"folderalreadyexists":        provider.ErrExists,
	"samenamefileorfolderexists": provider.ErrExists,

	// Throttling.
	"userdayflowoverlimited": provider.ErrRateLimited,
	"frequentoperation":      provider.ErrRateLimited,
	"toomanyrequests":        provider.ErrRateLimited,

	// Risk control. The upper layer circuit-breaks the whole account when it
	// sees ErrRiskControl instead of retrying, which is the only safe response:
	// hammering a 189 account that has been flagged is how a temporary flag
	// becomes a permanent ban.
	"infosecurityerrorcode": provider.ErrRiskControl,
	"riskcontrol":           provider.ErrRiskControl,
	"needcaptcha":           provider.ErrRiskControl,
	"accountlocked":         provider.ErrRiskControl,
	"useraccountabnormal":   provider.ErrRiskControl,
	"forbiddenoperation":    provider.ErrRiskControl,
}

// riskKeywords catch risk-control answers that arrive with an unmapped code but
// a recognisable Chinese message. Keyword matching is a fallback only: a code
// match always wins.
//
// UNVERIFIED: the exact wording 189 uses; these are the phrases its apps show.
var riskKeywords = []string{"风控", "风险", "验证码", "安全验证", "账号异常", "涉嫌"}

// rateKeywords catch throttling answers with an unmapped code.
var rateKeywords = []string{"频繁", "过快", "超出限制", "流量"}

func classifyCode(code, message string) error {
	if s, ok := errorClasses[strings.ToLower(strings.TrimSpace(code))]; ok {
		return s
	}
	for _, kw := range riskKeywords {
		if strings.Contains(message, kw) {
			return provider.ErrRiskControl
		}
	}
	for _, kw := range rateKeywords {
		if strings.Contains(message, kw) {
			return provider.ErrRateLimited
		}
	}
	return nil
}

// apiURL joins a path and query onto one of the driver's base URLs.
func apiURL(base, path string, q url.Values) string {
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// signedHeader builds the headers every api.cloud.189.cn call needs. The Date
// header and the Date inside the signature must be byte-identical, which is
// why both come from this one function.
//
// wantJSON selects the Accept header: the file APIs answer in JSON when asked
// to, while the upload host must be left alone so it answers in XML.
func (t *Tianyi) signedHeader(s session, method, rawURL string, wantJSON bool) (http.Header, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("tianyi: bad request url %q: %w", rawURL, err)
	}
	date := httpDate()
	h := http.Header{}
	h.Set("Date", date)
	h.Set("SessionKey", s.key)
	h.Set("Signature", signRequest(s.secret, s.key, method, u.EscapedPath(), date, ""))
	h.Set("Sign-Type", "1")
	h.Set("X-Request-ID", uuid.NewString())
	if wantJSON {
		h.Set("Accept", "application/json;charset=UTF-8")
	}
	return h, nil
}

// call performs one signed API request and checks the in-band error envelope.
// A session that the server rejects is dropped and the call retried exactly
// once; a second rejection surfaces as provider.ErrAuth so the upper layer
// marks the remote read-only instead of looping on the login box.
func (t *Tianyi) call(ctx context.Context, method, rawURL string, form map[string]string, class ratelimit.Class, out responder, asXML bool) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		s, err := t.ensureSession(ctx)
		if err != nil {
			return err
		}
		hdr, err := t.signedHeader(s, method, rawURL, !asXML)
		if err != nil {
			return err
		}
		req := httpx.Request{Method: method, URL: rawURL, Class: class, Header: hdr, Form: form}
		if asXML {
			// The upload host answers in XML unless a JSON Accept header is
			// negotiated, and this driver does not negotiate one.
			err = t.cli.XML(ctx, req, out)
		} else {
			err = t.cli.JSON(ctx, req, out)
		}
		if err == nil {
			err = out.apiErr()
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == 0 && errors.Is(err, provider.ErrAuth) {
			t.invalidate(s)
			continue
		}
		return err
	}
	return lastErr
}

// getJSON performs a signed GET whose response is JSON.
func (t *Tianyi) getJSON(ctx context.Context, rawURL string, class ratelimit.Class, out responder) error {
	return t.call(ctx, http.MethodGet, rawURL, nil, class, out, false)
}

// postJSON performs a signed form POST whose response is JSON.
func (t *Tianyi) postJSON(ctx context.Context, rawURL string, form map[string]string, class ratelimit.Class, out responder) error {
	return t.call(ctx, http.MethodPost, rawURL, form, class, out, false)
}

// getXML performs a signed GET whose response is XML (the upload host).
//
// UNVERIFIED: the upload host may expect its query string AES-ECB encrypted
// into a single `params` parameter, with that ciphertext also appended to the
// signature's canonical string. This driver sends plain query parameters and
// signs without a params term. If the host rejects them the call fails cleanly
// (nothing is uploaded), so the fallback is safe; it is the first thing to
// re-check if initMultiUpload starts returning a signature error.
func (t *Tianyi) getXML(ctx context.Context, rawURL string, class ratelimit.Class, out responder) error {
	return t.call(ctx, http.MethodGet, rawURL, nil, class, out, true)
}
