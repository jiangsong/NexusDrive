package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
)

// Header names of the signed webhook (docs/agent-roadmap.md §5.5).
const (
	HeaderTimestamp = "X-CloudFS-Timestamp"
	HeaderSignature = "X-CloudFS-Signature"
)

// MaxSignatureSkew is how far a webhook's timestamp may be from the
// receiver's clock before Verify treats it as a replay.
const MaxSignatureSkew = 5 * time.Minute

// webhookBody is the JSON a webhook rule posts.
type webhookBody struct {
	Rule   string `json:"rule"`
	Path   string `json:"path"`
	URI    string `json:"uri"`
	Kind   string `json:"kind"`
	Origin string `json:"origin"`
	TS     int64  `json:"ts"`
	// Size is present when the path is a file the VFS can stat.
	Size *int64 `json:"size,omitempty"`
	// DownloadURL is present only with include_download_url, and only
	// when the provider hands out a link usable by third parties.
	DownloadURL string `json:"download_url,omitempty"`
}

// Sign returns the X-CloudFS-Signature value for a body sent at ts (Unix
// seconds, decimal): "sha256=" + hex(HMAC-SHA256(secret, ts + "." + body)).
func Sign(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is the receiver-side check the docs and the console show: the
// timestamp header must be within MaxSignatureSkew of now, and the
// signature header must equal Sign(secret, ts, body). The comparison is
// constant-time.
func Verify(secret []byte, r *http.Request, body []byte, now time.Time) bool {
	ts := r.Header.Get(HeaderTimestamp)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > MaxSignatureSkew {
		return false
	}
	want := Sign(secret, ts, body)
	return hmac.Equal([]byte(want), []byte(r.Header.Get(HeaderSignature)))
}

// runWebhook posts the signed body for d to rule's webhook. A non-2xx
// response is an error carrying the status; the output is the status line
// and the start of the response body either way.
func (e *Engine) runWebhook(ctx context.Context, rule config.Trigger, d agent.Delivery) (string, error) {
	a := rule.Action.Webhook
	if a == nil {
		return "", errors.New("trigger: rule has no webhook")
	}
	if e.opt.Secrets == nil {
		return "", errors.New("trigger: no secret store to resolve the webhook secret")
	}
	secret, err := e.opt.Secrets(a.Secret)
	if err != nil {
		return "", fmt.Errorf("trigger: webhook secret: %w", err)
	}
	now := e.now()
	body := webhookBody{Rule: d.Rule, Path: d.Path, Kind: d.Kind, Origin: d.Origin, TS: now.Unix()}
	if d.Path != "" {
		body.URI = e.uriFor(d.Path)
	}
	note := ""
	if e.opt.FS != nil && d.Path != "" {
		if attr, err := e.opt.FS.StatPath(ctx, d.Path); err == nil && !attr.IsDir {
			size := attr.Size
			body.Size = &size
			if a.IncludeDownloadURL {
				if link, err := e.opt.FS.DownloadURL(ctx, d.Path); err == nil {
					body.DownloadURL = link.URL
				} else {
					note = "download_url unavailable: " + err.Error() + "\n"
				}
			}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("trigger: %w", err)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("trigger: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cloudfs-trigger")
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, Sign([]byte(secret), ts, raw))

	resp, err := e.client(a).Do(req)
	if err != nil {
		return note, fmt.Errorf("trigger: webhook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, outputCap))
	out := note + "HTTP " + resp.Status + "\n" + string(respBody)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("trigger: webhook: HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// client builds the outbound client for a webhook: through the proxy
// manager (with the rule's outbound override) when there is one, plain
// otherwise. The timeout is the rule's.
func (e *Engine) client(a *config.WebhookAction) *http.Client {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = config.DefaultWebhookTimeout
	}
	if e.opt.Proxy != nil {
		return e.opt.Proxy.Client(a.Proxy, timeout)
	}
	return &http.Client{Timeout: timeout}
}
