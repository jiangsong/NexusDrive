package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider/httpx"
)

func testClient() *httpx.Client {
	return httpx.New(httpx.Options{HTTP: &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Policy: retry.Policy{MaxAttempts: 1}})
}

func callbackRequest(t *testing.T, method, address, host string, want int) {
	t.Helper()
	req, err := http.NewRequest(method, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("callback status=%d body=%s want=%d", resp.StatusCode, b, want)
	}
	if strings.Contains(string(b), "private-code") || strings.Contains(string(b), "private-secret") {
		t.Fatal("callback leaked credentials")
	}
}

func TestOAuthRejectsInvalidCallbacksThenExchangesOnce(t *testing.T) {
	for _, asJSON := range []bool{true, false} {
		t.Run(map[bool]string{true: "json", false: "query"}[asJSON], func(t *testing.T) {
			var calls atomic.Int32
			var challenge, redirect string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				params := map[string]string{}
				if asJSON {
					if r.Method != "POST" || r.URL.RawQuery != "" {
						t.Error("JSON exchange leaked query or wrong method")
					}
					if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
						t.Error(err)
					}
				} else {
					if r.Method != "GET" {
						t.Error("Baidu exchange must use GET")
					}
					for k, v := range r.URL.Query() {
						params[k] = v[0]
					}
				}
				if params["code"] != "private-code" || params["client_secret"] != "private-secret" || params["redirect_uri"] != redirect || params["grant_type"] != "authorization_code" {
					t.Errorf("incorrect exchange fields")
				}
				sum := sha256.Sum256([]byte(params["code_verifier"]))
				if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
					t.Error("PKCE verifier does not match challenge")
				}
				w.Write([]byte(`{"access_token":"private-access","refresh_token":"private-refresh","expires_in":7200}`))
			}))
			defer srv.Close()
			o := OAuthOptions{Client: testClient(), AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", ClientID: "app", ClientSecret: "private-secret", RedirectURI: "http://127.0.0.1:0/callback", JSONToken: asJSON, PKCE: true, RequireRefresh: true}
			o.OpenURL = func(ctx context.Context, address string) error {
				u, _ := url.Parse(address)
				q := u.Query()
				redirect = q.Get("redirect_uri")
				challenge = q.Get("code_challenge")
				if strings.Contains(address, "private-secret") || len(q.Get("state")) < 43 || q.Get("code_challenge_method") != "S256" {
					t.Error("invalid authorization URL")
				}
				valid := url.Values{"state": {q.Get("state")}, "code": {"private-code"}}
				callbackRequest(t, "GET", redirect+"?state=wrong&code=private-code", "", 400)
				callbackRequest(t, "POST", redirect+"?"+valid.Encode(), "", 405)
				callbackRequest(t, "GET", redirect+"?"+valid.Encode(), "evil.example", 404)
				callbackRequest(t, "GET", redirect+"?"+valid.Encode()+"&state=duplicate", "", 400)
				callbackRequest(t, "GET", redirect+"?"+valid.Encode()+"&error=denied", "", 400)
				callbackRequest(t, "GET", redirect+"?"+valid.Encode(), "", 200)
				callbackRequest(t, "GET", redirect+"?"+valid.Encode(), "", 409)
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			token, err := Authorize(ctx, o)
			if err != nil || token.RefreshToken != "private-refresh" || calls.Load() != 1 {
				t.Fatalf("authorization: calls=%d err=%v", calls.Load(), err)
			}
			u, _ := url.Parse(redirect)
			l, err := net.Listen("tcp", u.Host)
			if err != nil {
				t.Fatalf("callback listener leaked: %v", err)
			}
			l.Close()
		})
	}
}

func TestOAuthCancelAndDenyDoNotExchange(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deny"}[deny], func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			o := OAuthOptions{Client: testClient(), AuthorizeURL: srv.URL, TokenURL: srv.URL, ClientID: "app", RedirectURI: "http://127.0.0.1:0/callback"}
			o.OpenURL = func(_ context.Context, address string) error {
				u, _ := url.Parse(address)
				q := u.Query()
				if deny {
					callbackRequest(t, "GET", q.Get("redirect_uri")+"?"+url.Values{"state": {q.Get("state")}, "error": {"access_denied"}}.Encode(), "", 200)
				} else {
					cancel()
				}
				return nil
			}
			_, err := Authorize(ctx, o)
			if err == nil || calls.Load() != 0 {
				t.Fatalf("denial/cancel exchanged: %v calls=%d", err, calls.Load())
			}
			if !deny && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel=%v", err)
			}
		})
	}
}

func TestOAuthExchangeErrorsAreRedactedAndNeverReplayed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		w.Write([]byte("private-code private-secret private-refresh"))
	}))
	defer srv.Close()
	o := OAuthOptions{Client: testClient(), AuthorizeURL: srv.URL, TokenURL: srv.URL, ClientID: "app", ClientSecret: "private-secret", RedirectURI: "http://127.0.0.1:0/callback"}
	o.OpenURL = func(ctx context.Context, address string) error {
		u, _ := url.Parse(address)
		q := u.Query()
		callbackRequest(t, "GET", q.Get("redirect_uri")+"?"+url.Values{"state": {q.Get("state")}, "code": {"private-code"}}.Encode(), "", 200)
		return nil
	}
	_, err := Authorize(context.Background(), o)
	if err == nil || strings.Contains(err.Error(), "private-") || calls.Load() != 1 {
		t.Fatalf("exchange calls=%d error=%v", calls.Load(), err)
	}
}

func TestOAuthRejectsUnsafeEndpointsBeforeOpeningBrowser(t *testing.T) {
	for _, redirect := range []string{"http://0.0.0.0:80/callback", "https://127.0.0.1:80/callback", "http://localhost:80/callback", "http://127.0.0.1/callback", "http://127.0.0.1:80/callback?x=y"} {
		o := OAuthOptions{Client: testClient(), ClientID: "app", AuthorizeURL: "https://example.invalid/authorize", TokenURL: "https://example.invalid/token", RedirectURI: redirect, OpenURL: func(context.Context, string) error { t.Error("opened browser for invalid redirect"); return nil }}
		if _, err := Authorize(context.Background(), o); err == nil {
			t.Fatalf("accepted %s", redirect)
		}
	}
	for _, raw := range []string{"http://example.invalid/token", "https://user:pass@example.invalid/token", "https://example.invalid/token?secret=x"} {
		if _, err := endpoint(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
