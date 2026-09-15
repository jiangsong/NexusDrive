package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/provider/httpx"
)

func TestAuthorizeQuarkShowsQRPollsAndReturnsServerCookies(t *testing.T) {
	var polls atomic.Int32
	var shown string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_id") != "532" || r.URL.Query().Get("v") != "1.2" {
			t.Fatalf("token query = %q", r.URL.RawQuery)
		}
		http.SetCookie(w, &http.Cookie{Name: "login_seed", Value: "seed", Path: "/"})
		w.Write([]byte(`{"status":2000000,"data":{"members":{"token":"private-qr-token"}}}`))
	})
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		_, cookieErr := r.Cookie("login_seed")
		if r.URL.Query().Get("token") != "private-qr-token" || cookieErr != nil {
			t.Fatalf("poll did not keep token/cookies: %q", r.URL.RawQuery)
		}
		if polls.Add(1) == 1 {
			w.Write([]byte(`{"status":50004001}`))
			return
		}
		w.Write([]byte(`{"status":2000000,"data":{"members":{"service_ticket":"private-ticket"}}}`))
	})
	mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("st") != "private-ticket" {
			t.Fatalf("account ticket = %q", r.URL.Query().Get("st"))
		}
		http.SetCookie(w, &http.Cookie{Name: "__uid", Value: "u1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "p1", Path: "/"})
		w.Write([]byte(`{"success":true}`))
	})

	cookie, err := AuthorizeQuark(context.Background(), QuarkDeviceOptions{
		Client:   httpx.New(httpx.Options{HTTP: srv.Client()}),
		TokenURL: srv.URL + "/token", PollURL: srv.URL + "/poll",
		LoginURL: srv.URL + "/scan", AccountURL: srv.URL + "/account",
		PollInterval: time.Millisecond,
		Show:         func(_ context.Context, value string) error { shown = value; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls = %d", polls.Load())
	}
	u, err := url.Parse(shown)
	if err != nil || u.Path != "/scan" || u.Query().Get("token") != "private-qr-token" || u.Query().Get("client_id") != "532" {
		t.Fatalf("shown QR URL = %q (%v)", shown, err)
	}
	for _, part := range []string{"__pus=p1", "__uid=u1", "login_seed=seed"} {
		if !strings.Contains(cookie, part) {
			t.Fatalf("cookie %q missing %q", cookie, part)
		}
	}
}

func TestAuthorizeQuarkRejectsExpiredQRWithoutLeakingToken(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":2000000,"data":{"members":{"token":"private-token"}}}`))
	})
	mux.HandleFunc("/poll", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"status":50004002}`)) })
	_, err := AuthorizeQuark(context.Background(), QuarkDeviceOptions{
		Client:   httpx.New(httpx.Options{HTTP: srv.Client()}),
		TokenURL: srv.URL + "/token", PollURL: srv.URL + "/poll",
		LoginURL: srv.URL + "/scan", AccountURL: srv.URL + "/account",
		PollInterval: time.Millisecond, Show: func(context.Context, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("expired error = %v", err)
	}
}
