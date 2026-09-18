package gdrive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// invalid_grant on a Drive that worked yesterday almost always means one
// thing: the OAuth application is still in Testing, where Google expires a
// test user's authorization seven days after consent. The refresh token is
// not corrupt and re-authorizing buys another week, so an error that says
// only "token refresh failed" sends people round that loop indefinitely.
func TestRefreshExplainsTheSevenDayTestingExpiry(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name     string
		body     string
		wantHint bool
	}{
		{"an expired testing grant", `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, true},
		{"any other refusal", `{"error":"invalid_client","error_description":"Unauthorized"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body = tc.body
			p, err := New(Options{
				Name: "gd", APIBase: srv.URL, TokenURL: srv.URL,
				RefreshToken: "refresh-token", ClientID: "client",
				Client: httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
				Now:    func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.token(context.Background())
			if err == nil {
				t.Fatal("a refused refresh reported success")
			}
			if !errors.Is(err, provider.ErrAuth) {
				t.Errorf("error is not ErrAuth: %v", err)
			}
			hinted := strings.Contains(err.Error(), "Testing") && strings.Contains(err.Error(), "7 days")
			if hinted != tc.wantHint {
				t.Errorf("hint present = %v, want %v: %v", hinted, tc.wantHint, err)
			}
		})
	}
}
