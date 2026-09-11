// Package ratelimit implements the per-(remote, account, class) token bucket
// with AIMD adaptation and the per-account circuit breaker described in
// docs/DESIGN.md §4.2.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Class is the request class a bucket governs.
type Class int

const (
	Meta Class = iota
	Download
	Upload
	// Transfer governs a CDN byte-stream GET (a ranged read against the
	// direct link), as opposed to the API call that resolves the link
	// itself, which stays on Download. A remote whose effective Transfer
	// rate resolves to 0 has no bucket of its own: Registry.Limiter aliases
	// it onto Download so behaviour is unchanged until a caller opts in.
	Transfer
)

func (c Class) String() string {
	switch c {
	case Meta:
		return "meta"
	case Download:
		return "download"
	case Upload:
		return "upload"
	case Transfer:
		return "transfer"
	}
	return "unknown"
}

// Options tunes one AIMD limiter.
type Options struct {
	// Rate is the initial and maximum sustained rate in requests per second.
	Rate float64
	// Burst is the bucket capacity. Zero means max(1, Rate).
	Burst float64
	// MinRate is the floor AIMD will not go below. Zero means Rate/32.
	MinRate float64
	// RecoverAfter is how many consecutive successes trigger one additive
	// step back toward Rate. Zero means 20.
	RecoverAfter int
	// Step is the additive increase per recovery, as a fraction of Rate.
	// Zero means 0.1.
	Step float64
	// Now is injectable for tests.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Burst <= 0 {
		o.Burst = o.Rate
		if o.Burst < 1 {
			o.Burst = 1
		}
	}
	if o.MinRate <= 0 {
		o.MinRate = o.Rate / 32
	}
	if o.RecoverAfter <= 0 {
		o.RecoverAfter = 20
	}
	if o.Step <= 0 {
		o.Step = 0.1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Limiter is a token bucket whose refill rate adapts: multiplicative decrease
// on throttle signals, additive increase after sustained success.
type Limiter struct {
	opt Options

	mu         sync.Mutex
	rate       float64
	tokens     float64
	last       time.Time
	successes  int
	pauseUntil time.Time
}

// New builds a limiter. Rate must be > 0.
func New(opt Options) *Limiter {
	opt = opt.withDefaults()
	return &Limiter{opt: opt, rate: opt.Rate, tokens: opt.Burst, last: opt.Now()}
}

func (l *Limiter) refillLocked(now time.Time) {
	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.opt.Burst {
			l.tokens = l.opt.Burst
		}
		l.last = now
	}
}

// Reserve returns how long the caller must wait before proceeding and
// consumes one token. It never blocks.
func (l *Limiter) Reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.opt.Now()
	l.refillLocked(now)
	var wait time.Duration
	if l.pauseUntil.After(now) {
		wait = l.pauseUntil.Sub(now)
	}
	l.tokens--
	if l.tokens < 0 {
		deficit := -l.tokens / l.rate
		if d := time.Duration(deficit * float64(time.Second)); d > wait {
			wait = d
		}
	}
	return wait
}

// Wait blocks until a token is available or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	d := l.Reserve()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Throttled records a 429 / throttle signal: halve the rate (not below
// MinRate), drop accumulated tokens, and pause for retryAfter if given.
func (l *Limiter) Throttled(retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate /= 2
	if l.rate < l.opt.MinRate {
		l.rate = l.opt.MinRate
	}
	l.tokens = 0
	l.successes = 0
	if retryAfter > 0 {
		if until := l.opt.Now().Add(retryAfter); until.After(l.pauseUntil) {
			l.pauseUntil = until
		}
	}
}

// Succeeded records a successful request; every RecoverAfter successes the
// rate steps up by Step*Rate until it reaches Rate again.
func (l *Limiter) Succeeded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rate >= l.opt.Rate {
		return
	}
	l.successes++
	if l.successes >= l.opt.RecoverAfter {
		l.successes = 0
		l.rate += l.opt.Step * l.opt.Rate
		if l.rate > l.opt.Rate {
			l.rate = l.opt.Rate
		}
	}
}

// Rate returns the current refill rate.
func (l *Limiter) Rate() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// BreakerOptions tunes a circuit breaker.
type BreakerOptions struct {
	// Threshold trips the breaker after this many risk-control events inside
	// Window. Zero means 3.
	Threshold int
	// Window is the sliding window. Zero means 10 minutes.
	Window time.Duration
	// Cooldown is how long the breaker stays open. Zero means 30 minutes.
	Cooldown time.Duration
	Now      func() time.Time
}

func (o BreakerOptions) withDefaults() BreakerOptions {
	if o.Threshold <= 0 {
		o.Threshold = 3
	}
	if o.Window <= 0 {
		o.Window = 10 * time.Minute
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 30 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Breaker opens after repeated risk-control signals so we stop hammering an
// account that is about to be banned. While open the mount treats the remote
// as read-only.
type Breaker struct {
	opt BreakerOptions

	mu        sync.Mutex
	events    []time.Time
	openUntil time.Time
}

// NewBreaker builds a breaker.
func NewBreaker(opt BreakerOptions) *Breaker {
	return &Breaker{opt: opt.withDefaults()}
}

// Trip records one risk-control event and returns true if the breaker is now
// open.
func (b *Breaker) Trip() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.opt.Now()
	cutoff := now.Add(-b.opt.Window)
	kept := b.events[:0]
	for _, e := range b.events {
		if e.After(cutoff) {
			kept = append(kept, e)
		}
	}
	b.events = append(kept, now)
	if len(b.events) >= b.opt.Threshold {
		b.openUntil = now.Add(b.opt.Cooldown)
		b.events = b.events[:0]
	}
	return b.openUntil.After(now)
}

// Open reports whether requests should be refused right now.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil.After(b.opt.Now())
}

// OpenUntil returns when the breaker closes (zero when closed).
func (b *Breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.After(b.opt.Now()) {
		return b.openUntil
	}
	return time.Time{}
}

// Reset closes the breaker (operator override via `cloudfs status --reset`).
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openUntil = time.Time{}
	b.events = b.events[:0]
}

// Key identifies one bucket.
type Key struct {
	Remote  string
	Account string
	Class   Class
}

// Registry holds limiters and breakers per key and is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	limiters map[Key]*Limiter
	breakers map[string]*Breaker // remote/account
	defaults func(Key) Options
	bopts    BreakerOptions
}

// NewRegistry builds a registry; defaults yields Options for an unseen key.
func NewRegistry(defaults func(Key) Options, bopts BreakerOptions) *Registry {
	return &Registry{limiters: map[Key]*Limiter{}, breakers: map[string]*Breaker{}, defaults: defaults, bopts: bopts}
}

// Limiter returns (creating if needed) the limiter for k.
//
// A Transfer key whose defaults resolve to rate <= 0 has no dedicated
// bucket: it is aliased onto the Download limiter for the same
// remote/account, so a request classed Transfer draws tokens from, and any
// throttle signal it receives reduces, the very same *Limiter as Download.
// This is what lets a provider adopt the Transfer class without changing
// behaviour until it also sets a positive Transfer rate.
func (r *Registry) Limiter(k Key) *Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.limiters[k]; ok {
		return l
	}
	if k.Class == Transfer {
		if opt := r.defaults(k); opt.Rate <= 0 {
			dk := Key{Remote: k.Remote, Account: k.Account, Class: Download}
			dl, ok := r.limiters[dk]
			if !ok {
				dl = New(r.defaults(dk))
				r.limiters[dk] = dl
			}
			r.limiters[k] = dl
			return dl
		}
	}
	l := New(r.defaults(k))
	r.limiters[k] = l
	return l
}

// Breaker returns (creating if needed) the breaker for remote/account.
func (r *Registry) Breaker(remote, account string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := remote + "/" + account
	b, ok := r.breakers[id]
	if !ok {
		b = NewBreaker(r.bopts)
		r.breakers[id] = b
	}
	return b
}
