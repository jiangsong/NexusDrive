package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider/httpx"
)

func Test115QRPKCEFlow(t *testing.T) {
	var polls, exchanges atomic.Int32
	challenge := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		var data any
		switch r.URL.Path {
		case "/device":
			if r.Method != "POST" || r.Form.Get("client_id") != "app" || r.Form.Get("code_challenge_method") != "sha256" {
				t.Error("invalid device request")
			}
			challenge = r.Form.Get("code_challenge")
			data = map[string]any{"uid": "private-uid", "time": 0, "sign": "private-sign", "qrcode": "https://115.example/scan"}
		case "/poll":
			if r.URL.Query().Get("uid") != "private-uid" || r.URL.Query().Get("time") != "0" || r.URL.Query().Get("sign") != "private-sign" {
				t.Error("invalid poll parameters")
			}
			data = map[string]int{"status": int(polls.Add(1)) - 1}
		case "/token":
			exchanges.Add(1)
			verifier := r.Form.Get("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			if r.Method != "POST" || len(verifier) < 43 || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("uid") != "private-uid" {
				t.Error("invalid device exchange")
			}
			data = map[string]any{"access_token": "private-access", "refresh_token": "private-refresh", "expires_in": 7200}
		default:
			t.Error("unexpected path")
		}
		json.NewEncoder(w).Encode(map[string]any{"state": 1, "code": 0, "data": data})
	}))
	defer srv.Close()
	var shows, scanned int
	o := Device115Options{Client: testClient(), ClientID: "app", DeviceURL: srv.URL + "/device", PollURL: srv.URL + "/poll", TokenURL: srv.URL + "/token", PollInterval: time.Millisecond,
		Show: func(context.Context, string) error { shows++; return nil }, Scanned: func() { scanned++ }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := Authorize115(ctx, o)
	if err != nil || token.RefreshToken != "private-refresh" || polls.Load() != 3 || exchanges.Load() != 1 || shows != 1 || scanned != 1 {
		t.Fatalf("flow err=%v polls=%d exchange=%d shows=%d scanned=%d", err, polls.Load(), exchanges.Load(), shows, scanned)
	}
}

func Test115LongPollTimeoutIsNotAuthorizationFailure(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			w.Write([]byte(`{"state":true,"data":{"uid":"u","sign":"s","qrcode":"qr"}}`))
		case "/poll":
			if polls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			w.Write([]byte(`{"state":1,"data":{"status":2}}`))
		case "/token":
			w.Write([]byte(`{"state":1,"data":{"access_token":"access","refresh_token":"refresh"}}`))
		}
	}))
	defer srv.Close()
	client := httpx.New(httpx.Options{HTTP: &http.Client{Timeout: 50 * time.Millisecond}, Policy: retry.Policy{MaxAttempts: 1}})
	o := Device115Options{Client: client, ClientID: "app", DeviceURL: srv.URL + "/device", PollURL: srv.URL + "/poll", TokenURL: srv.URL + "/token", PollInterval: time.Millisecond, Show: func(context.Context, string) error { return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token, err := Authorize115(ctx, o); err != nil || token.RefreshToken != "refresh" || polls.Load() != 2 {
		t.Fatalf("long poll: %v polls=%d", err, polls.Load())
	}
}

func Test115InvalidQRCodeNeverExchanges(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			w.Write([]byte(`{"state":1,"data":{"uid":"u","sign":"s","qrcode":"qr"}}`))
		case "/poll":
			w.Write([]byte(`{"state":0,"data":{"status":2}}`))
		case "/token":
			exchanges.Add(1)
		}
	}))
	defer srv.Close()
	o := Device115Options{Client: testClient(), ClientID: "app", DeviceURL: srv.URL + "/device", PollURL: srv.URL + "/poll", TokenURL: srv.URL + "/token", Show: func(context.Context, string) error { return nil }}
	if _, err := Authorize115(context.Background(), o); !errors.Is(err, errDeviceRejected) || exchanges.Load() != 0 {
		t.Fatalf("invalid QR = %v exchange=%d", err, exchanges.Load())
	}
}
