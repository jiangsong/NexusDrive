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
	"cloudfs/internal/daemon"
	"cloudfs/internal/provider"
)

// Google Drive and Box are OAuth accounts, so `config auth` must run the
// browser flow for them instead of asking the person to paste a refresh token
// they had to mint by hand somewhere else.
func TestBrowserAuthorizationCoversGoogleDriveAndBox(t *testing.T) {
	for _, tc := range []struct {
		typ        string
		wantParams map[string]string
	}{
		{"gdrive", map[string]string{"access_type": "offline", "prompt": "consent"}},
		{"box", nil},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			var form url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					http.NotFound(w, r)
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("%s exchanged the code with %s, not POST", tc.typ, r.Method)
				}
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
				form, _ = url.ParseQuery(string(body))
				json.NewEncoder(w).Encode(map[string]any{
					"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 3600,
				})
			}))
			defer srv.Close()

			p := configTestPath(t, fmt.Sprintf("  acct:\n    type: %s\n    client_id: app\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n",
				tc.typ, srv.URL+"/authorize", srv.URL+"/oauth/token"))
			var out, diag bytes.Buffer
			c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag,
				ReadSecret: func(string) (string, error) { return "private-secret", nil }}
			c.OpenURL = func(ctx context.Context, address string) error {
				u, err := url.Parse(address)
				if err != nil {
					return err
				}
				q := u.Query()
				for k, want := range tc.wantParams {
					if q.Get(k) != want {
						t.Errorf("authorize URL %s=%q, want %q", k, q.Get(k), want)
					}
				}
				resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"private-code"}}.Encode())
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
			if form.Get("code") != "private-code" || form.Get("client_secret") != "private-secret" {
				t.Fatalf("%s token exchange form = %v", tc.typ, form)
			}
			if strings.Contains(out.String()+diag.String(), "private-") {
				t.Fatal("authorization printed a secret, a code or a token")
			}
			cfg, _ := config.Load(p)
			r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["acct"])
			if err != nil || r.Extra["refresh_token"] != "private-refresh" || r.Extra["client_secret"] != "private-secret" {
				t.Fatalf("saved credentials = %v, %v", r.Extra["refresh_token"], err)
			}
		})
	}
}

// `config add` prints what `config auth` will ask for. For an account the
// daemon can authorize itself, that answer is "a browser", not "a refresh
// token you minted somewhere else" — the note is the only place the person
// learns which of the two they are in for.
func TestCredentialHintForOAuthAccountsPointsAtTheBrowser(t *testing.T) {
	for _, typ := range provider.Types() {
		if _, ok := daemon.OAuthProfileFor(typ); !ok {
			continue
		}
		hint := credentialHint(typ)
		if !strings.Contains(hint, "browser") && !strings.Contains(hint, "浏览器") {
			t.Errorf("%s: credential hint %q does not say the browser flow will do it", typ, hint)
		}
	}
}
