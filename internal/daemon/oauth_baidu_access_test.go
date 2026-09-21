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
)

func TestBaiduDaemonAuthorizationPersistsIssuedAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 2_592_000,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("cache:\n  dir: %q\nsecrets:\n  backend: file\nremotes:\n  bd:\n    type: baidu\n    client_id: app\n    client_secret: private-secret\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n",
		filepath.Join(dir, "cache"), srv.URL+"/authorize", srv.URL+"/oauth/token")
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	present := func(_ context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		q := u.Query()
		go func() {
			resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{
				"state": {q.Get("state")}, "code": {"private-code"},
			}.Encode())
			if err == nil {
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
			}
		}()
		return nil
	}
	_, wait, err := StartOAuthFlow(ctx, cfg, "bd", "http://127.0.0.1:0/callback", present)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(ctx); err != nil {
		t.Fatal(err)
	}

	saved, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := config.NewSecretStore(saved).ResolveRemote(saved.Remotes["bd"])
	if err != nil {
		t.Fatal(err)
	}
	if r.Extra["access_token"] != "private-access" || r.Extra["refresh_token"] != "private-refresh" {
		t.Fatal("Baidu daemon authorization did not persist both tokens")
	}
}
