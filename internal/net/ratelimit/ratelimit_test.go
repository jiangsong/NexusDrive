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
