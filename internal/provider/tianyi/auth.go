package tianyi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// session is one authenticated PC session. sessionKey identifies it and
// sessionSecret keys the request signature; the family variants are returned
// by the same call and kept so a future family-cloud driver can reuse them.
type session struct {
	key           string
	secret        string
	familyKey     string
	familySecret  string
	accessToken   string
	establishedAt time.Time
}

func (s session) valid() bool { return s.key != "" && s.secret != "" }

// signRequest computes the Signature header: an uppercase hex HMAC-SHA1, keyed
// by the session secret, over the canonical string
//
//	SessionKey=<key>&Operate=<METHOD>&RequestURI=<path>&Date=<http date>
//
// with "&params=<value>" appended when the request carries an encrypted params
// blob.
//
// UNVERIFIED: the field order above is what community clients use and is
// load-bearing (HMAC over a different order yields a different digest), but it
// is not published by 189. If signing starts failing, this ordering is the
// first thing to re-check against a live capture.
func signRequest(sessionSecret, sessionKey, operate, requestURI, date, params string) string {
	canonical := fmt.Sprintf("SessionKey=%s&Operate=%s&RequestURI=%s&Date=%s",
		sessionKey, strings.ToUpper(operate), requestURI, date)
	if params != "" {
		canonical += "&params=" + params
	}
	mac := hmac.New(sha1.New, []byte(sessionSecret))
	mac.Write([]byte(canonical))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

// httpDate renders the Date header in the GMT form the signature expects.
func httpDate() string { return time.Now().UTC().Format(http.TimeFormat) }

// ensureSession returns a usable session, logging in if necessary. Login runs
// under a mutex so a burst of concurrent calls produces one login, not one per
// caller: the 189 login box is itself throttled and captcha-guarded.
func (t *Tianyi) ensureSession(ctx context.Context) (session, error) {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	if t.sess.valid() {
		return t.sess, nil
	}
	s, err := t.login(ctx)
	if err != nil {
		return session{}, err
	}
	t.sess = s
	return s, nil
}

// invalidate drops the session the caller was using. It compares keys so a
// caller holding a stale session cannot discard a newer one established by a
// concurrent login.
func (t *Tianyi) invalidate(used session) {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	if t.sess.key == used.key {
		t.sess = session{}
	}
}

// Session returns the current session key, or "" when not logged in. It exists
// for diagnostics and tests; the secret is deliberately not exposed.
func (t *Tianyi) Session() string {
	t.authMu.Lock()
	defer t.authMu.Unlock()
	return t.sess.key
}

// login establishes a session, preferring a configured access token because it
// skips the login box entirely (and therefore skips any captcha).
func (t *Tianyi) login(ctx context.Context) (session, error) {
	if t.accessToken != "" {
		return t.sessionFromToken(ctx, "", t.accessToken)
	}
	toURL, err := t.passwordLogin(ctx)
	if err != nil {
		return session{}, err
	}
	return t.sessionFromToken(ctx, toURL, "")
}

// userSession is the XML document getSessionForPC.action returns. This is one
// of the two places 189 answers in XML, so it is decoded with httpx.Client.XML.
// No XMLName field is declared on purpose: that makes the decode tolerant of
// the root element name, which differs between the token and redirect forms.
type userSession struct {
	envelope
	SessionKey          string `xml:"sessionKey"`
	SessionSecret       string `xml:"sessionSecret"`
	FamilySessionKey    string `xml:"familySessionKey"`
	FamilySessionSecret string `xml:"familySessionSecret"`
	AccessToken         string `xml:"accessToken"`
}

// sessionFromToken exchanges either an access token or the redirect URL from a
// password login for a PC session.
//
// This call is not signed (there is no session yet), so it does not go through
// Tianyi.call.
func (t *Tianyi) sessionFromToken(ctx context.Context, redirectURL, token string) (session, error) {
	q := url.Values{}
	q.Set("appId", appID)
	q.Set("clientType", clientType)
	q.Set("version", appVersion)
	q.Set("channelId", channelID)
	q.Set("rand", strconv.FormatInt(time.Now().UnixNano(), 10))
	switch {
	case token != "":
		q.Set("accessToken", token)
	case redirectURL != "":
		q.Set("redirectURL", redirectURL)
	default:
		return session{}, fmt.Errorf("%w: tianyi: no access token and no login redirect", provider.ErrAuth)
	}

	var out userSession
	// XML, not JSON: getSessionForPC.action answers with a <userSession>
	// document regardless of the Accept header.
	err := t.cli.XML(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    apiURL(t.auth, "/getSessionForPC.action", q),
		Class:  ratelimit.Meta,
	}, &out)
	if err != nil {
		return session{}, fmt.Errorf("tianyi: getSessionForPC: %w", err)
	}
	if err := out.apiErr(); err != nil {
		return session{}, err
	}
	if out.SessionKey == "" || out.SessionSecret == "" {
		return session{}, fmt.Errorf("%w: tianyi: getSessionForPC returned no session key", provider.ErrAuth)
	}
	return session{
		key:           out.SessionKey,
		secret:        out.SessionSecret,
		familyKey:     out.FamilySessionKey,
		familySecret:  out.FamilySessionSecret,
		accessToken:   out.AccessToken,
		establishedAt: time.Now(),
	}, nil
}

// loginParams are the one-shot values the PC login page embeds in JavaScript.
type loginParams struct {
	lt           string
	reqID        string
	paramID      string
	captchaToken string
	returnURL    string
}

// jsVar matches `name = "value"` and `name = 'value'` as the login page writes
// them, and the "name":"value" JSON spelling some builds of the page use.
//
// UNVERIFIED: the page is server-rendered HTML+JS with no contract; the
// extraction below is the only way to get these values and will need revisiting
// whenever 189 rebuilds the page.
func jsVar(body, name string) string {
	// The name is preceded by a non-identifier character so that looking for
	// "lt" does not match the tail of another variable's name.
	edge := `(?:^|[^A-Za-z0-9_$])`
	for _, pat := range []string{
		edge + name + `\s*=\s*"([^"]*)"`,
		edge + name + `\s*=\s*'([^']*)'`,
		`"` + name + `"\s*:\s*"([^"]*)"`,
	} {
		if m := regexp.MustCompile(pat).FindStringSubmatch(body); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// initLoginParams fetches the PC login page and scrapes the parameters the
// login box requires.
func (t *Tianyi) initLoginParams(ctx context.Context) (loginParams, error) {
	q := url.Values{}
	q.Set("appId", appID)
	q.Set("clientType", clientType)
	q.Set("returnURL", returnURL)
	q.Set("timeStamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	resp, err := t.cli.Do(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    apiURL(t.web, "/api/portal/unifyLoginForPC.action", q),
		Class:  ratelimit.Meta,
	})
	if err != nil {
		return loginParams{}, fmt.Errorf("tianyi: fetch login page: %w", err)
	}
	body := string(resp.Bytes)
	p := loginParams{
		lt:           jsVar(body, "lt"),
		reqID:        jsVar(body, "reqId"),
		paramID:      jsVar(body, "paramId"),
		captchaToken: jsVar(body, "captchaToken"),
		returnURL:    jsVar(body, "returnUrl"),
	}
	// lt and paramId are the two the submit endpoint rejects the request
	// without, so a missing one is reported by name rather than sent blindly.
	var missing []string
	if p.lt == "" {
		missing = append(missing, "lt")
	}
	if p.paramID == "" {
		missing = append(missing, "paramId")
	}
	if len(missing) > 0 {
		return loginParams{}, fmt.Errorf("%w: tianyi: login page did not carry %s",
			provider.ErrAuth, strings.Join(missing, ", "))
	}
	if p.returnURL == "" {
		p.returnURL = returnURL
	}
	return p, nil
}

// encryptConf is the login box's RSA parameters. pre is a prefix (typically
// "{NRP}") the server expects in front of the ciphertext.
type encryptConf struct {
	Result looseString `json:"result"`
	Msg    string      `json:"msg"`
	Data   struct {
		Pre    string `json:"pre"`
		PubKey string `json:"pubKey"`
	} `json:"data"`
}

// loginSubmitResp is the login box's answer. result 0 means success and toUrl
// carries the redirect that getSessionForPC.action turns into a session.
type loginSubmitResp struct {
	Result looseString `json:"result"`
	Msg    string      `json:"msg"`
	ToURL  string      `json:"toUrl"`
}

// passwordLogin runs the RSA login-box flow and returns the redirect URL that
// getSessionForPC.action exchanges for a session.
//
// UNVERIFIED: the loginSubmit form field names (appKey, accountType, userName,
// password, captchaToken, paramId, dynamicCheck, cb_SaveName, isOauth2) and
// the accountType value. They are what community clients post. A wrong name
// produces a failed login, never a wrong file operation.
//
// The credentials never leave the process in the clear: both are RSA-PKCS1v15
// encrypted with the key the server publishes, hex-encoded, and prefixed with
// the server-supplied `pre` marker.
func (t *Tianyi) passwordLogin(ctx context.Context) (string, error) {
	params, err := t.initLoginParams(ctx)
	if err != nil {
		return "", err
	}

	var conf encryptConf
	if err := t.cli.JSON(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    t.auth + "/api/logbox/config/encryptConf.do",
		Class:  ratelimit.Meta,
		Form:   map[string]string{"appId": appID},
	}, &conf); err != nil {
		return "", fmt.Errorf("tianyi: encryptConf: %w", err)
	}
	if !codeOK(conf.Result) {
		return "", newAPIError(conf.Result.String(), conf.Msg)
	}
	if conf.Data.PubKey == "" {
		return "", fmt.Errorf("%w: tianyi: encryptConf returned no public key", provider.ErrAuth)
	}

	user, err := rsaEncryptHex(conf.Data.PubKey, t.username)
	if err != nil {
		return "", fmt.Errorf("tianyi: encrypt username: %w", err)
	}
	pass, err := rsaEncryptHex(conf.Data.PubKey, t.password)
	if err != nil {
		return "", fmt.Errorf("tianyi: encrypt password: %w", err)
	}

	var out loginSubmitResp
	hdr := http.Header{}
	// lt and REQID identify the login-page session the parameters came from.
	hdr.Set("lt", params.lt)
	hdr.Set("REQID", params.reqID)
	hdr.Set("Referer", t.auth+"/api/logbox/oauth2/unifyAccountLogin.do")
	if err := t.cli.JSON(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    t.auth + "/api/logbox/oauth2/loginSubmit.do",
		Class:  ratelimit.Meta,
		Header: hdr,
		Form: map[string]string{
			"appKey":       appID,
			"accountType":  accountTyp,
			"userName":     conf.Data.Pre + user,
			"password":     conf.Data.Pre + pass,
			"validateCode": "",
			"captchaToken": params.captchaToken,
			"returnUrl":    params.returnURL,
			"dynamicCheck": "FALSE",
			"clientType":   clientType,
			"cb_SaveName":  "1",
			"isOauth2":     "false",
			"state":        "",
			"paramId":      params.paramID,
		},
	}, &out); err != nil {
		return "", fmt.Errorf("tianyi: loginSubmit: %w", err)
	}
	if !codeOK(out.Result) {
		// A non-zero result here is a credential or captcha problem, both of
		// which are auth failures the upper layer must not retry blindly, so an
		// otherwise unrecognised code is reported as provider.ErrAuth.
		ae := &apiError{Code: out.Result.String(), Message: out.Msg,
			sentinel: classifyCode(out.Result.String(), out.Msg)}
		if ae.sentinel == nil {
			ae.sentinel = provider.ErrAuth
		}
		return "", ae
	}
	if out.ToURL == "" {
		return "", fmt.Errorf("%w: tianyi: loginSubmit returned no redirect url", provider.ErrAuth)
	}
	return out.ToURL, nil
}

// rsaEncryptHex encrypts one credential with the login box's public key and
// returns uppercase hex, which is the encoding the login box expects.
//
// The key arrives as bare base64 of a DER SubjectPublicKeyInfo. PKCS#1 and a
// PEM-wrapped key are both accepted too, because which one the server sends
// has changed before.
func rsaEncryptHex(pubKey, plain string) (string, error) {
	der, err := decodePublicKeyDER(pubKey)
	if err != nil {
		return "", err
	}
	var rsaPub *rsa.PublicKey
	if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
		p, ok := pub.(*rsa.PublicKey)
		if !ok {
			return "", fmt.Errorf("tianyi: login key is %T, want RSA", pub)
		}
		rsaPub = p
	} else if p, err2 := x509.ParsePKCS1PublicKey(der); err2 == nil {
		rsaPub = p
	} else {
		return "", fmt.Errorf("tianyi: parse login public key: %w", err)
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, []byte(plain))
	if err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(ct)), nil
}

func decodePublicKeyDER(pubKey string) ([]byte, error) {
	s := strings.TrimSpace(pubKey)
	if strings.Contains(s, "-----BEGIN") {
		block, _ := pem.Decode([]byte(s))
		if block == nil {
			return nil, fmt.Errorf("tianyi: login public key is not valid PEM")
		}
		return block.Bytes, nil
	}
	// The server sends unwrapped base64; strip any line breaks it inserted.
	s = strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("tianyi: login public key is not base64: %w", err)
	}
	return der, nil
}
