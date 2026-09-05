// Package auth implements interactive authorization separately from provider
// refresh logic. It never writes credentials or logs token responses.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider/httpx"
)

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

type OAuthOptions struct {
	// Client must disable redirects and retries for single-use exchanges;
	// daemon.AuthorizationHTTP supplies this policy and proxy/rate limiting.
	Client                 *httpx.Client
	AuthorizeURL, TokenURL string
	ClientID, ClientSecret string
	RedirectURI, Scope     string
	// JSONToken selects Aliyun's POST JSON exchange. Otherwise a GET query
	// implements Baidu's documented token exchange.
	JSONToken      bool
	PKCE           bool
	RequireRefresh bool
	AuthParams     url.Values
	// OpenURL displays/opens the authorization URL after the callback socket
	// is listening. It must return without waiting for authorization.
	OpenURL func(context.Context, string) error
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("auth: endpoint must be an absolute URL without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("auth: endpoints require HTTPS (HTTP allowed only on literal loopback addresses)")
	}
	return u, nil
}

type callback struct {
	code   string
	denied bool
}

// Authorize binds a literal loopback address, checks an unguessable state and
// exchanges exactly one valid callback. No credential is persisted until the
// returned token is handled by the caller. Invalid callbacks do not consume it.
func Authorize(ctx context.Context, opt OAuthOptions) (Token, error) {
	var zero Token
	if opt.Client == nil || opt.OpenURL == nil || opt.ClientID == "" {
		return zero, errors.New("auth: client, client_id and URL presenter are required")
	}
	authorize, err := endpoint(opt.AuthorizeURL)
	if err != nil {
		return zero, err
	}
	if _, err := endpoint(opt.TokenURL); err != nil {
		return zero, err
	}
	redirect, err := url.Parse(opt.RedirectURI)
	if err != nil || redirect.Scheme != "http" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" || redirect.Port() == "" || redirect.Path == "" {
		return zero, errors.New("auth: redirect must be an HTTP loopback URL with port and callback path")
	}
	ip := net.ParseIP(redirect.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return zero, errors.New("auth: redirect host must be a literal loopback address")
	}
	state, err := randomString()
	if err != nil {
		return zero, err
	}
	verifier, err := randomString()
	if err != nil {
		return zero, err
	}
	l, err := net.Listen("tcp", redirect.Host)
	if err != nil {
		return zero, fmt.Errorf("auth: listen for callback: %w", err)
	}
	redirect.Host = l.Addr().String()
	callbackURI := redirect.String()
	result := make(chan callback, 1)
	var accepted atomic.Bool
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if r.Host != redirect.Host || r.URL.Path != redirect.Path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "use GET", 405)
			return
		}
		if len(r.URL.RawQuery) > 8192 {
			http.Error(w, "callback too large", 400)
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "invalid authorization state", 400)
			return
		}
		code, denial := q.Get("code"), q.Get("error")
		if (code == "") == (denial == "") || len(q["code"]) > 1 || len(q["error"]) > 1 || len(code) > 4096 {
			http.Error(w, "invalid authorization callback", 400)
			return
		}
		if !accepted.CompareAndSwap(false, true) {
			http.Error(w, "authorization already received", 409)
			return
		}
		fmt.Fprintln(w, "Authorization response received. Return to the CloudFS terminal to see the result.")
		result <- callback{code: code, denied: denial != ""}
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(l) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		srv.Close()
		<-serveDone
	}()
	q := url.Values{}
	for k, vs := range opt.AuthParams {
		q[k] = append([]string(nil), vs...)
	}
	q.Set("client_id", opt.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", callbackURI)
	q.Set("scope", opt.Scope)
	q.Set("state", state)
	if opt.PKCE {
		sum := sha256.Sum256([]byte(verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	authorize.RawQuery = q.Encode()
	if err := opt.OpenURL(ctx, authorize.String()); err != nil {
		return zero, err
	}
	var cb callback
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case cb = <-result:
	}
	if cb.denied {
		return zero, errors.New("auth: authorization was denied")
	}
	params := map[string]string{"grant_type": "authorization_code", "client_id": opt.ClientID, "code": cb.code, "redirect_uri": callbackURI}
	if opt.ClientSecret != "" {
		params["client_secret"] = opt.ClientSecret
	}
	if opt.PKCE {
		params["code_verifier"] = verifier
	}
	req := httpx.Request{Class: ratelimit.Meta, Stream: true, URL: opt.TokenURL}
	if opt.JSONToken {
		req.Method = http.MethodPost
		req.JSON = params
	} else {
		req.Method = http.MethodGet
		values := url.Values{}
		for k, v := range params {
			values.Set(k, v)
		}
		req.URL += "?" + values.Encode()
	}
	resp, err := opt.Client.Do(ctx, req)
	if err != nil {
		return zero, redactedExchangeError(ctx, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return zero, redactedExchangeError(ctx, err)
	}
	if len(b) > 1<<20 {
		return zero, errors.New("auth: token response exceeds 1 MiB")
	}
	var token Token
	if err := json.Unmarshal(b, &token); err != nil {
		return zero, errors.New("auth: invalid token response")
	}
	if token.Error != "" || token.AccessToken == "" || (opt.RequireRefresh && token.RefreshToken == "") {
		return zero, errors.New("auth: server rejected the code or omitted required tokens")
	}
	if strings.ContainsAny(token.AccessToken, "\r\n") || strings.ContainsAny(token.RefreshToken, "\r\n") {
		return zero, errors.New("auth: malformed token response")
	}
	return token, nil
}

func redactedExchangeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var status *httpx.StatusError
	if errors.As(err, &status) {
		return fmt.Errorf("auth: token exchange failed (HTTP %d); check application settings and authorization", status.Code)
	}
	// Transport errors can contain URLs with authorization codes and secrets.
	return errors.New("auth: token exchange failed; check connectivity and proxy settings")
}
