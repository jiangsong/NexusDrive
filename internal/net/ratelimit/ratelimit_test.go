package ratelimit

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestTokenBucket(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(Options{Rate: 2, Burst: 2, Now: c.now})
	if d := l.Reserve(); d != 0 {
		t.Fatalf("first reserve should be free, got %v", d)
	}
	if d := l.Reserve(); d != 0 {
		t.Fatalf("second reserve within burst should be free, got %v", d)
	}
	if d := l.Reserve(); d != 500*time.Millisecond {
		t.Fatalf("third reserve should wait 500ms at 2 rps, got %v", d)
	}
	c.advance(2 * time.Second)
	if d := l.Reserve(); d != 0 {
		t.Fatalf("after refill reserve should be free, got %v", d)
	}
}

func TestAIMD(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(Options{Rate: 8, MinRate: 1, RecoverAfter: 2, Step: 0.25, Now: c.now})
	l.Throttled(0)
	if l.Rate() != 4 {
		t.Fatalf("after one throttle rate = %v, want 4", l.Rate())
	}
	for i := 0; i < 10; i++ {
		l.Throttled(0)
	}
	if l.Rate() != 1 {
		t.Fatalf("rate should floor at MinRate, got %v", l.Rate())
	}
	l.Succeeded()
	if l.Rate() != 1 {
		t.Fatalf("one success should not recover yet, got %v", l.Rate())
	}
	l.Succeeded()
	if l.Rate() != 3 {
		t.Fatalf("after RecoverAfter successes rate = %v, want 1+0.25*8=3", l.Rate())
	}
	for i := 0; i < 20; i++ {
		l.Succeeded()
	}
	if l.Rate() != 8 {
		t.Fatalf("rate should cap at initial Rate, got %v", l.Rate())
	}
}

func TestRetryAfterPause(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := New(Options{Rate: 100, Now: c.now})
	l.Throttled(3 * time.Second)
	if d := l.Reserve(); d < 3*time.Second {
		t.Fatalf("Retry-After should pause the bucket, got %v", d)
	}
	c.advance(3 * time.Second)
	if d := l.Reserve(); d > 100*time.Millisecond {
		t.Fatalf("after pause reserve should be near-free, got %v", d)
	}
}

func TestBreaker(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := NewBreaker(BreakerOptions{Threshold: 3, Window: time.Minute, Cooldown: 10 * time.Minute, Now: c.now})
	if b.Trip() || b.Trip() {
		t.Fatal("breaker opened before threshold")
	}
	if !b.Trip() || !b.Open() {
		t.Fatal("breaker should open on third trip")
	}
	c.advance(10*time.Minute + time.Second)
	if b.Open() {
		t.Fatal("breaker should close after cooldown")
	}
	// Old events fall out of the window.
	b.Trip()
	c.advance(2 * time.Minute)
	b.Trip()
	if b.Trip() {
		t.Fatal("events outside the window should not count")
	}
	b.Reset()
	if b.Open() || len(b.events) != 0 {
		t.Fatal("reset should clear state")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(func(k Key) Options {
		if k.Class == Meta {
			return Options{Rate: 1}
		}
		return Options{Rate: 5}
	}, BreakerOptions{})
	a := r.Limiter(Key{"p115", "acc", Meta})
	if a != r.Limiter(Key{"p115", "acc", Meta}) {
		t.Fatal("registry should return the same limiter for the same key")
	}
	if r.Limiter(Key{"p115", "acc", Download}).Rate() != 5 || a.Rate() != 1 {
		t.Fatal("defaults not applied per class")
	}
	if r.Breaker("p115", "acc") != r.Breaker("p115", "acc") {
		t.Fatal("registry should return the same breaker")
	}
}

// TestTransferSharesDownloadBucketWhenRateIsZero: a Transfer class that
// resolves to rate 0 (no config override, no Caps.QPS.Transfer) has no
// dedicated bucket of its own — it draws tokens from, and is throttled
// through, the very same Download limiter for that remote/account.
func TestTransferSharesDownloadBucketWhenRateIsZero(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := NewRegistry(func(k Key) Options {
		switch k.Class {
		case Download:
			return Options{Rate: 5, Burst: 1, Now: c.now}
		case Transfer:
			return Options{Rate: 0, Now: c.now}
		}
		return Options{Rate: 5, Now: c.now}
	}, BreakerOptions{})

	down := r.Limiter(Key{"p115", "acc", Download})
	xfer := r.Limiter(Key{"p115", "acc", Transfer})
	if down != xfer {
		t.Fatal("a zero-rate Transfer limiter should be the same instance as Download")
	}

	// Drain the single Download token; a Transfer request must feel it too,
	// since it is the same bucket.
	if d := down.Reserve(); d != 0 {
		t.Fatalf("first Download reserve should be free, got %v", d)
	}
	if d := xfer.Reserve(); d == 0 {
		t.Fatal("Transfer should block on the exhausted, shared Download bucket")
	}

	// A throttle signal delivered on the Transfer key must reduce the shared
	// Download limiter's rate, since they are the same bucket.
	xfer.Throttled(0)
	if down.Rate() != 2.5 {
		t.Fatalf("throttling the aliased Transfer limiter should halve the shared rate, got %v", down.Rate())
	}
}

// TestTransferHasOwnBucketWhenRatePositive: once Transfer resolves to a
// positive rate (a config override or Caps.QPS.Transfer), it gets its own
// bucket, independent of Download.
func TestTransferHasOwnBucketWhenRatePositive(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := NewRegistry(func(k Key) Options {
		switch k.Class {
		case Download:
			return Options{Rate: 1, Burst: 1, Now: c.now}
		case Transfer:
			return Options{Rate: 16, Burst: 16, Now: c.now}
		}
		return Options{Rate: 5, Now: c.now}
	}, BreakerOptions{})

	down := r.Limiter(Key{"p115", "acc", Download})
	xfer := r.Limiter(Key{"p115", "acc", Transfer})
	if down == xfer {
		t.Fatal("a positive-rate Transfer limiter should not alias Download")
	}

	// Exhaust the Download bucket; Transfer must be unaffected.
	down.Reserve()
	if d := down.Reserve(); d == 0 {
		t.Fatal("Download bucket should be exhausted after its one token")
	}
	if d := xfer.Reserve(); d != 0 {
		t.Fatalf("Transfer should have its own tokens, got wait %v", d)
	}
}
