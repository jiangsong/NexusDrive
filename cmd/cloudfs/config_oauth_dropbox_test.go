package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// Dropbox authorizes as a public client: there is no client secret to ask for,
// and asking would be worse than not having a wizard at all — the person would
// be prompted for a value their app console never issued. The exchange is
// proved by the PKCE verifier instead, and what comes back is a lasting grant
// rather than the few-hour access token the old single-field prompt collected.
func TestBrowserAuthorizationCoversDropboxWithoutAskingForASecret(t *testing.T) {
	var form url.Values
	var authorizeQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		form, _ = url.ParseQuery(string(body))
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 14400,
		})
	}))
	defer srv.Close()

	p := configTestPath(t, fmt.Sprintf("  acct:\n    type: dropbox\n    client_id: app-key\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n",
		srv.URL+"/authorize", srv.URL+"/oauth/token"))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag,
		ReadSecret: func(label string) (string, error) {
			t.Errorf("authorization asked for %q; a public client has no secret to give", label)
			return "", nil
		}}
	c.OpenURL = func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		authorizeQuery = u.Query()
		resp, err := http.Get(authorizeQuery.Get("redirect_uri") + "?" +
			url.Values{"state": {authorizeQuery.Get("state")}, "code": {"private-code"}}.Encode())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	args := []string{"auth", "acct", "--redirect-uri", "http://127.0.0.1:0/callback", "--no-check", "--config", p}
	if err := runConfig(context.Background(), args, c); err != nil {
		t.Fatal(err)
	}

	if got := authorizeQuery.Get("token_access_type"); got != "offline" {
		t.Errorf("authorize URL token_access_type=%q, want offline; without it the grant expires in hours", got)
	}
	if got := authorizeQuery.Get("code_challenge_method"); got != "S256" {
		t.Errorf("authorize URL code_challenge_method=%q, want S256", got)
	}
	if authorizeQuery.Get("code_challenge") == "" {
		t.Error("the authorize URL carries no PKCE challenge")
	}
	if form.Get("code_verifier") == "" {
		t.Error("the token exchange sent no PKCE verifier, so nothing proves it came from this attempt")
	}
	if form.Get("client_secret") != "" {
		t.Errorf("the token exchange sent a client secret %q; a public client has none", form.Get("client_secret"))
	}
	if strings.Contains(out.String()+diag.String(), "private-") {
		t.Fatal("authorization printed a secret, a code or a token")
	}

	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["acct"])
	if err != nil {
		t.Fatal(err)
	}
	if r.Extra["refresh_token"] != "private-refresh" {
		t.Fatalf("saved refresh token = %v", r.Extra["refresh_token"])
	}
	if secret, ok := r.Extra["client_secret"]; ok && secret != "" {
		t.Errorf("a client secret was stored for a public client: %v", secret)
	}
}

// The single-field fallback asked for an access token, which Dropbox expires in
// hours; that was the whole defect. Reaching it again would silently restore
// the account that dies the same afternoon.
func TestDropboxNoLongerFallsBackToTheSingleAccessTokenPrompt(t *testing.T) {
	p := configTestPath(t, "  acct:\n    type: dropbox\n    client_id: app-key\n")
	var out, diag bytes.Buffer
	asked := ""
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag,
		ReadSecret: func(label string) (string, error) {
			asked = label
			return "pasted-token", nil
		}}
	// No OpenURL and a short deadline: the browser flow cannot complete here,
	// and it does not need to. What matters is which path it took, and a
	// prompt for a token means it took the wrong one.
	_ = runConfig(context.Background(), []string{"auth", "acct", "--no-check", "--no-browser", "--timeout", "1s", "--config", p}, c)
	if strings.Contains(asked, "access_token") {
		t.Fatalf("authorization asked for %q, the short-lived token the browser flow exists to replace", asked)
	}
}

// A throwaway access token is still the fastest way to try an account out, and
// docs/dropbox.md documents it. Naming the field explicitly must keep working.
func TestAnExplicitAccessTokenFieldStillImportsAShortLivedToken(t *testing.T) {
	p := configTestPath(t, "  acct:\n    type: dropbox\n    client_id: app-key\n")
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader("short-lived\n"), Out: &out, Err: &diag,
		ReadSecret: func(string) (string, error) { return "short-lived", nil }}
	args := []string{"auth", "acct", "--field", "access_token", "--no-check", "--config", p}
	if err := runConfig(context.Background(), args, c); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["acct"])
	if err != nil {
		t.Fatal(err)
	}
	if r.Extra["access_token"] != "short-lived" {
		t.Fatalf("saved access token = %v", r.Extra["access_token"])
	}
}
