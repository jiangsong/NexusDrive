// Package retry classifies backend errors and computes backoff delays. It is
// the single place that decides "retry, refresh token, refresh link, circuit
// break, or give up" (docs/DESIGN.md §4.2).
package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"time"

	"cloudfs/internal/provider"
)

// Class is what the caller should do with an error.
type Class int

const (
	// ClassOK means no error.
	ClassOK Class = iota
	// ClassRetryable: network, 5xx, 429. Back off and retry; feed AIMD.
	ClassRetryable
	// ClassAuth: refresh the token once, then retry; on second failure mark
	// the remote read-only.
	ClassAuth
	// ClassRiskControl: provider-specific ban signal. Circuit-break the account.
	ClassRiskControl
	// ClassLinkExpired: refresh the download URL and retry once.
	ClassLinkExpired
	// ClassConflict: remote changed under us. Write a conflict copy.
	ClassConflict
	// ClassTerminal: 4xx and friends. Do not retry; dead-letter uploads.
	ClassTerminal
	// ClassCanceled: context canceled or deadline exceeded. Stop quietly.
	ClassCanceled
)

func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassRetryable:
		return "retryable"
	case ClassAuth:
		return "auth"
	case ClassRiskControl:
		return "risk_control"
	case ClassLinkExpired:
		return "link_expired"
	case ClassConflict:
		return "conflict"
	case ClassTerminal:
		return "terminal"
	case ClassCanceled:
		return "canceled"
	}
	return "unknown"
}

// HTTPStatuser is implemented by backend errors that carry an HTTP status.
type HTTPStatuser interface{ HTTPStatus() int }

// Classify maps an error to a Class. Order matters: sentinel errors from the
// provider package win over generic HTTP status inspection, which wins over
// net.Error.
func Classify(err error) Class {
	switch {
	case err == nil:
		return ClassOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ClassCanceled
	case errors.Is(err, provider.ErrRiskControl):
		return ClassRiskControl
	case errors.Is(err, provider.ErrRateLimited), errors.Is(err, provider.ErrTransient):
		return ClassRetryable
	case errors.Is(err, provider.ErrAuth):
		return ClassAuth
	case errors.Is(err, provider.ErrLinkExpired):
		return ClassLinkExpired
	case errors.Is(err, provider.ErrConflict):
		return ClassConflict
	case errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrExists), errors.Is(err, provider.ErrUnsupported):
		return ClassTerminal
	}
	var hs HTTPStatuser
	if errors.As(err, &hs) {
		switch s := hs.HTTPStatus(); {
		case s == 401:
			return ClassAuth
		case s == 403 || s == 410:
			// On a CDN host this is usually an expired link; on an API host
			// it is terminal. The caller knows which host it hit and may
			// override; the default is the safer retry-once path.
			return ClassLinkExpired
		case s == 409 || s == 412:
			return ClassConflict
		case s == 429 || s >= 500:
			return ClassRetryable
		case s >= 400:
			return ClassTerminal
		}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ClassRetryable
	}
	return ClassRetryable
}

// RetryAfter extracts a server-supplied wait hint, or 0.
func RetryAfter(err error) time.Duration {
	var ra *provider.RetryAfterError
	if errors.As(err, &ra) {
		return ra.RetryAfter
	}
	return 0
}

// Backoff computes exponential delays with full jitter, capped at Max, and
// never below a server-supplied Retry-After.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	// Rand is optional; nil uses math/rand/v2.
	Rand func() float64
}

// DefaultBackoff matches docs/DESIGN.md: 500ms base, 64s cap.
var DefaultBackoff = Backoff{Base: 500 * time.Millisecond, Max: 64 * time.Second}

// Delay returns the wait before attempt n (0-based).
func (b Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 30 {
		attempt = 30
	}
	ceiling := b.Base << uint(attempt)
	if ceiling > b.Max || ceiling <= 0 {
		ceiling = b.Max
	}
	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	d := time.Duration(float64(ceiling) * r())
	if d < retryAfter {
		d = retryAfter
	}
	return d
}

// Policy bundles attempt limits with Backoff.
type Policy struct {
	Backoff     Backoff
	MaxAttempts int
}

// DefaultPolicy: 8 attempts ≈ 500ms … 64s ≈ 3 minutes of retrying.
var DefaultPolicy = Policy{Backoff: DefaultBackoff, MaxAttempts: 8}

// Do runs fn until it succeeds, is classified non-retryable, or attempts run
// out. onRetry (optional) is called before each sleep with the error and the
// delay, so the caller can feed the rate limiter.
func (p Policy) Do(ctx context.Context, fn func() error, onRetry func(err error, class Class, delay time.Duration)) error {
	var err error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		err = fn()
		class := Classify(err)
		if class != ClassRetryable {
			return err
		}
		if attempt == p.MaxAttempts-1 {
			break
		}
		delay := p.Backoff.Delay(attempt, RetryAfter(err))
		if onRetry != nil {
			onRetry(err, class, delay)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
