package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Google and Box exchange the code with a form-encoded POST. Sending it as a
// query (Baidu's shape) puts the authorization code and the client secret in
// a URL that proxies and server logs keep; sending it as JSON (Aliyun's
// shape) is refused outright.
func TestFormTokenExchangePostsAFormAndKeepsSecretsOutOfTheURL(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("the exchange put %q in the URL", r.URL.RawQuery)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("content type = %q", ct)
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		got, _ = url.ParseQuery(string(body))
		w.Write([]byte(`{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600}`))
	}))
	defer srv.Close()

	var redirect string
	o := OAuthOptions{
		Client: testClient(), AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token",
		ClientID: "app", ClientSecret: "private-secret", RedirectURI: "http://127.0.0.1:0/callback",
		FormToken: true, RequireRefresh: true,
	}
	o.OpenURL = func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		redirect = u.Query().Get("redirect_uri")
		resp, err := http.Get(redirect + "?" + url.Values{"state": {u.Query().Get("state")}, "code": {"private-code"}}.Encode())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	token, err := Authorize(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "private-refresh" {
		t.Fatalf("token = %+v", token)
	}
	if got.Get("grant_type") != "authorization_code" || got.Get("code") != "private-code" ||
		got.Get("client_secret") != "private-secret" || got.Get("redirect_uri") != redirect {
		t.Fatalf("exchange form = %v", got)
	}
}
