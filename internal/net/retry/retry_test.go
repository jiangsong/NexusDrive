package retry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

type httpErr int

func (h httpErr) Error() string   { return fmt.Sprintf("http %d", int(h)) }
func (h httpErr) HTTPStatus() int { return int(h) }

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Class
	}{
		{nil, ClassOK},
		{context.Canceled, ClassCanceled},
		{provider.ErrRiskControl, ClassRiskControl},
		{fmt.Errorf("wrap: %w", provider.ErrRateLimited), ClassRetryable},
		{&provider.RetryAfterError{Err: provider.ErrRateLimited, RetryAfter: time.Second}, ClassRetryable},
		{provider.ErrAuth, ClassAuth},
		{provider.ErrLinkExpired, ClassLinkExpired},
		{provider.ErrConflict, ClassConflict},
		{provider.ErrNotFound, ClassTerminal},
		{httpErr(401), ClassAuth},
		{httpErr(403), ClassLinkExpired},
		{httpErr(409), ClassConflict},
		{httpErr(429), ClassRetryable},
		{httpErr(503), ClassRetryable},
		{httpErr(400), ClassTerminal},
		{&net.DNSError{IsTimeout: true}, ClassRetryable},
		{errors.New("something odd"), ClassRetryable},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Rand: func() float64 { return 1 }}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}
	for i, w := range want {
		if got := b.Delay(i, 0); got != w {
			t.Errorf("attempt %d: got %v want %v", i, got, w)
		}
	}
	if got := b.Delay(0, 5*time.Second); got != 5*time.Second {
		t.Errorf("Retry-After should floor the delay, got %v", got)
	}
	// Jitter never exceeds the ceiling.
	b.Rand = nil
	for i := 0; i < 100; i++ {
		if d := b.Delay(3, 0); d > 800*time.Millisecond {
			t.Fatalf("jitter above ceiling: %v", d)
		}
	}
}

func TestPolicyDo(t *testing.T) {
	p := Policy{Backoff: Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 4}
	calls := 0
	err := p.Do(context.Background(), func() error {
		calls++
		if calls < 3 {
			return provider.ErrTransient
		}
		return nil
	}, nil)
	if err != nil || calls != 3 {
		t.Fatalf("Do = %v after %d calls", err, calls)
	}

	calls = 0
	var retries int
	err = p.Do(context.Background(), func() error { calls++; return provider.ErrTransient }, func(error, Class, time.Duration) { retries++ })
	if !errors.Is(err, provider.ErrTransient) || calls != 4 || retries != 3 {
		t.Fatalf("exhausted: err=%v calls=%d retries=%d", err, calls, retries)
	}

	calls = 0
	err = p.Do(context.Background(), func() error { calls++; return provider.ErrNotFound }, nil)
	if !errors.Is(err, provider.ErrNotFound) || calls != 1 {
		t.Fatalf("terminal error should not retry: err=%v calls=%d", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = Policy{Backoff: Backoff{Base: time.Hour, Max: time.Hour}, MaxAttempts: 3}.Do(ctx, func() error { return provider.ErrTransient }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx should abort the sleep, got %v", err)
	}
}
