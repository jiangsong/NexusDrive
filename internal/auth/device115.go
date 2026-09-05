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
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider/httpx"
)

type Device115Options struct {
	Client                       *httpx.Client
	ClientID                     string
	DeviceURL, PollURL, TokenURL string
	PollInterval                 time.Duration
	Show                         func(context.Context, string) error
	Scanned                      func()
}

var errDeviceRejected = errors.New("auth: 115 rejected or expired the QR authorization; request a new code")

func deviceRequest(ctx context.Context, client *httpx.Client, req httpx.Request, out any) error {
	req.Stream = true
	req.Class = ratelimit.Meta
	resp, err := client.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("auth: device response exceeds 1 MiB")
	}
	var env struct {
		State json.RawMessage `json:"state"`
		Code  int             `json:"code"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return errors.New("auth: invalid device response")
	}
	state := strings.TrimSpace(string(env.State))
	if (state != "1" && state != "true") || env.Code != 0 {
		return errDeviceRejected
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return errors.New("auth: invalid device payload")
	}
	return nil
}

// Authorize115 implements the documented mobile QR PKCE flow. 115 calls the
// SHA256 challenge method "sha256", not the OAuth S256 spelling. Polling is a
// long poll: network timeouts are expected, token exchange replay is not.
func Authorize115(ctx context.Context, opt Device115Options) (Token, error) {
	var zero Token
	if opt.Client == nil || opt.ClientID == "" || opt.Show == nil {
		return zero, errors.New("auth: 115 requires client_id, HTTP client and QR presenter")
	}
	if opt.DeviceURL == "" {
		opt.DeviceURL = "https://passportapi.115.com/open/authDeviceCode"
	}
	if opt.PollURL == "" {
		opt.PollURL = "https://qrcodeapi.115.com/get/status/"
	}
	if opt.TokenURL == "" {
		opt.TokenURL = "https://passportapi.115.com/open/deviceCodeToToken"
	}
	for _, address := range []string{opt.DeviceURL, opt.PollURL, opt.TokenURL} {
		if _, err := endpoint(address); err != nil {
			return zero, err
		}
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = 2 * time.Second
	}
	verifier, err := randomString()
	if err != nil {
		return zero, err
	}
	sum := sha256.Sum256([]byte(verifier))
	var code struct {
		UID  string `json:"uid"`
		Time int64  `json:"time"`
		Sign string `json:"sign"`
		QR   string `json:"qrcode"`
	}
	err = deviceRequest(ctx, opt.Client, httpx.Request{Method: http.MethodPost, URL: opt.DeviceURL, Form: map[string]string{"client_id": opt.ClientID, "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "sha256"}}, &code)
	if err != nil {
		return zero, redactedDeviceError(ctx, err)
	}
	if code.UID == "" || code.Sign == "" || code.QR == "" || len(code.QR) > 4096 || code.Time < 0 {
		return zero, errors.New("auth: device response omitted valid QR parameters")
	}
	if err := opt.Show(ctx, code.QR); err != nil {
		return zero, err
	}
	query := url.Values{"uid": {code.UID}, "time": {strconv.FormatInt(code.Time, 10)}, "sign": {code.Sign}}
	scanned := false
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		var status struct {
			Status int `json:"status"`
		}
		err = deviceRequest(ctx, opt.Client, httpx.Request{Method: http.MethodGet, URL: opt.PollURL + "?" + query.Encode()}, &status)
		delay := opt.PollInterval
		if err != nil {
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			var timeout net.Error
			var httpErr *httpx.StatusError
			switch {
			case errors.As(err, &timeout) && timeout.Timeout(): // long-poll timeout: keep waiting
			case errors.As(err, &httpErr) && httpErr.Code == 429:
				delay = max(delay, retry.RetryAfter(err), 5*time.Second)
			default:
				return zero, redactedDeviceError(ctx, err)
			}
		} else if status.Status == 2 {
			break
		} else if status.Status == 1 {
			if !scanned && opt.Scanned != nil {
				opt.Scanned()
			}
			scanned = true
		} else if status.Status != 0 {
			return zero, errDeviceRejected
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	var token Token
	err = deviceRequest(ctx, opt.Client, httpx.Request{Method: http.MethodPost, URL: opt.TokenURL, Form: map[string]string{"uid": code.UID, "code_verifier": verifier}}, &token)
	if err != nil {
		return zero, redactedDeviceError(ctx, err)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.Error != "" || strings.ContainsAny(token.AccessToken+token.RefreshToken, "\r\n") {
		return zero, errors.New("auth: device exchange omitted valid tokens")
	}
	return token, nil
}

func redactedDeviceError(ctx context.Context, err error) error {
	if errors.Is(err, errDeviceRejected) {
		return errDeviceRejected
	}
	return redactedExchangeError(ctx, err)
}
