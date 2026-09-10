package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	_ "cloudfs/internal/provider/dropbox"
)

// The web UI authorizes through this path rather than through the CLI, so the
// public-client case has to hold here too. The failure it guards against is
// nastier than a refusal: the authorization succeeds against the provider, the
// person sees the consent screen accept, and the save then fails on a
// client_secret they were never asked for and their app console never issued.
// saveCredentials rejects an empty value, so the key must be left out entirely.
func TestAPublicClientAuthorizationSavesOnlyTheRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		form, _ := url.ParseQuery(string(body))
		if form.Get("code_verifier") == "" {
			t.Error("the exchange carried no PKCE verifier")
		}
		if got := form.Get("client_secret"); got != "" {
			t.Errorf("the exchange carried a client secret %q for a public client", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 14400,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("cache:\n  dir: %q\nsecrets:\n  backend: file\nremotes:\n  db:\n    type: dropbox\n    client_id: app-key\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n",
		filepath.Join(dir, "cache"), srv.URL+"/authorize", srv.URL+"/oauth/token")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	present := func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge") == "" {
			t.Error("the authorize URL carried no PKCE challenge")
		}
		if q.Get("token_access_type") != "offline" {
			t.Errorf("token_access_type = %q, want offline", q.Get("token_access_type"))
		}
		go func() {
			resp, err := http.Get(q.Get("redirect_uri") + "?" +
				url.Values{"state": {q.Get("state")}, "code": {"private-code"}}.Encode())
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		}()
		return nil
	}
	_, wait, err := StartOAuthFlow(ctx, cfg, "db", "http://127.0.0.1:0/callback", present)
	if err != nil {
		t.Fatalf("a public client could not start an authorization: %v", err)
	}
	if err := wait(ctx); err != nil {
		t.Fatalf("the authorization did not persist: %v", err)
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := config.NewSecretStore(saved).ResolveRemote(saved.Remotes["db"])
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
