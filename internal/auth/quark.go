package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider/httpx"
)

const (
	quarkTokenURL   = "https://uop.quark.cn/cas/ajax/getTokenForQrcodeLogin"
	quarkPollURL    = "https://uop.quark.cn/cas/ajax/getServiceTicketByQrcodeToken"
	quarkLoginURL   = "https://su.quark.cn/4_eMHBJ"
	quarkAccountURL = "https://pan.quark.cn/account/info"
)

// QuarkDeviceOptions describes Quark's web QR login. The endpoints are
// injectable because this is an unofficial interface and its complete state
// machine must be tested without a real account.
type QuarkDeviceOptions struct {
	Client                      *httpx.Client
	TokenURL, PollURL, LoginURL string
	AccountURL                  string
	PollInterval                time.Duration
	Show                        func(context.Context, string) error
}

var errQuarkQRExpired = errors.New("auth: Quark rejected or expired the QR authorization; request a new code")

type quarkEnvelope struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Members struct {
			Token         string `json:"token"`
			ServiceTicket string `json:"service_ticket"`
		} `json:"members"`
	} `json:"data"`
}

func quarkJSON(ctx context.Context, client *httpx.Client, address string) (quarkEnvelope, error) {
	var out quarkEnvelope
	resp, err := client.Do(ctx, httpx.Request{Method: http.MethodGet, URL: address, Class: ratelimit.Meta})
	if err != nil {
		return out, redactedExchangeError(ctx, err)
	}
	if len(resp.Bytes) > 1<<20 {
		return out, errors.New("auth: Quark authorization response exceeds 1 MiB")
	}
	if err := json.Unmarshal(resp.Bytes, &out); err != nil {
		return out, errors.New("auth: invalid Quark authorization response")
	}
	return out, nil
}

// AuthorizeQuark drives the QR flow used by Quark's current web login. The
// service ticket and cookies stay inside this process; the caller receives
// only a Cookie header suitable for the Quark provider and immediately saves
// it through the secret store.
func AuthorizeQuark(ctx context.Context, opt QuarkDeviceOptions) (string, error) {
	if opt.Client == nil || opt.Show == nil {
		return "", errors.New("auth: Quark requires an HTTP client and QR presenter")
	}
	if opt.TokenURL == "" {
		opt.TokenURL = quarkTokenURL
	}
	if opt.PollURL == "" {
		opt.PollURL = quarkPollURL
	}
	if opt.LoginURL == "" {
		opt.LoginURL = quarkLoginURL
	}
	if opt.AccountURL == "" {
		opt.AccountURL = quarkAccountURL
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = 2 * time.Second
	}
	for _, address := range []string{opt.TokenURL, opt.PollURL, opt.LoginURL, opt.AccountURL} {
		if _, err := endpoint(address); err != nil {
			return "", err
		}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", errors.New("auth: cannot initialize Quark authorization cookies")
	}
	opt.Client.SetCookieJar(jar)

	tokenURL, _ := url.Parse(opt.TokenURL)
	q := tokenURL.Query()
	q.Set("client_id", "532")
	q.Set("v", "1.2")
	tokenURL.RawQuery = q.Encode()
	env, err := quarkJSON(ctx, opt.Client, tokenURL.String())
	if err != nil {
		return "", err
	}
	if env.Status != 2000000 || env.Data.Members.Token == "" || len(env.Data.Members.Token) > 4096 || strings.ContainsAny(env.Data.Members.Token, "\r\n") {
		return "", errors.New("auth: Quark did not issue a valid QR token")
	}
	token := env.Data.Members.Token
	loginURL, _ := url.Parse(opt.LoginURL)
	q = loginURL.Query()
	q.Set("token", token)
	q.Set("client_id", "532")
	q.Set("ssb", "weblogin")
	q.Set("uc_param_str", "")
	q.Set("uc_biz_str", "S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0")
	loginURL.RawQuery = q.Encode()
	if err := opt.Show(ctx, loginURL.String()); err != nil {
		return "", err
	}

	var ticket string
	for ticket == "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		pollURL, _ := url.Parse(opt.PollURL)
		q = pollURL.Query()
		q.Set("client_id", "532")
		q.Set("v", "1.2")
		q.Set("token", token)
		pollURL.RawQuery = q.Encode()
		env, err = quarkJSON(ctx, opt.Client, pollURL.String())
		if err != nil {
			return "", err
		}
		switch env.Status {
		case 2000000:
			ticket = env.Data.Members.ServiceTicket
			if ticket == "" || len(ticket) > 4096 || strings.ContainsAny(ticket, "\r\n") {
				return "", errors.New("auth: Quark confirmation omitted a valid service ticket")
			}
		case 50004001:
			// Not scanned or not confirmed yet.
		case 50004002:
			return "", errQuarkQRExpired
		default:
			return "", fmt.Errorf("auth: Quark QR authorization was rejected (status %d)", env.Status)
		}
		if ticket == "" {
			timer := time.NewTimer(opt.PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
	}

	accountURL, _ := url.Parse(opt.AccountURL)
	q = accountURL.Query()
	q.Set("st", ticket)
	accountURL.RawQuery = q.Encode()
	resp, err := opt.Client.Do(ctx, httpx.Request{Method: http.MethodGet, URL: accountURL.String(), Class: ratelimit.Meta})
	if err != nil {
		return "", redactedExchangeError(ctx, err)
	}
	var account struct {
		Success bool `json:"success"`
	}
	if len(resp.Bytes) > 1<<20 || json.Unmarshal(resp.Bytes, &account) != nil || !account.Success {
		return "", errors.New("auth: Quark rejected the confirmed login")
	}
	cookies := jar.Cookies(accountURL)
	sort.SliceStable(cookies, func(i, j int) bool { return cookies[i].Name < cookies[j].Name })
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Value == "" || strings.ContainsAny(cookie.Name+cookie.Value, "\r\n;") {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	if len(parts) == 0 {
		return "", errors.New("auth: Quark login returned no usable session cookie")
	}
	return strings.Join(parts, "; "), nil
}
