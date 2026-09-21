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
	"sync/atomic"
	"testing"

	"cloudfs/internal/config"
)

// Baidu treats a refresh immediately after an authorization-code exchange as
// suspicious. The access token returned by the exchange must therefore reach
// the first account check instead of being discarded.
func TestBaiduBrowserAuthorizationChecksWithIssuedAccessToken(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if n := exchanges.Add(1); n != 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"Trigger security policy"}`)
				return
			}
			if r.Method != http.MethodGet || r.URL.Query().Get("code") != "private-code" {
				t.Fatalf("token exchange = %s %v", r.Method, r.URL.Query())
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 2_592_000,
			})
		case "/rest/2.0/xpan/file":
			if r.URL.Query().Get("method") != "list" || r.URL.Query().Get("access_token") != "private-access" {
				t.Fatalf("account check query = %v", r.URL.Query())
			}
			io.WriteString(w, `{"errno":0,"list":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := configTestPath(t, fmt.Sprintf("  bd:\n    type: baidu\n    client_id: app\n    root_id: /\n    base_url: %q\n    oauth_url: %q\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n",
		srv.URL, srv.URL, srv.URL+"/authorize", srv.URL+"/oauth/token"))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag,
		ReadSecret: func(string) (string, error) { return "private-secret", nil }}
	c.OpenURL = func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		q := u.Query()
		resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{
			"state": {q.Get("state")}, "code": {"private-code"},
		}.Encode())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}

	if err := runConfig(context.Background(), []string{
		"auth", "bd", "--redirect-uri", "http://127.0.0.1:0/callback", "--config", p,
	}, c); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("token exchanges = %d, want only the authorization-code exchange", exchanges.Load())
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["bd"])
	if err != nil {
		t.Fatal(err)
	}
	if r.Extra["access_token"] != "private-access" || r.Extra["refresh_token"] != "private-refresh" {
		t.Fatal("Baidu authorization did not persist both tokens")
	}
	if strings.Contains(out.String()+diag.String(), "private-") {
		t.Fatal("authorization output exposed a credential")
	}
}
