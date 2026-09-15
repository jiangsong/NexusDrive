package embed

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Fake is the Embedder tests use: every text maps to a deterministic
// L2-normalised vector derived from its hash, so the same text always gets
// the same vector and different texts get different ones, with no endpoint.
// It counts calls and records batch sizes, which is what test/perf asserts
// on, and can simulate an outage for the worker tests.
type Fake struct {
	dim int

	mu      sync.Mutex
	calls   int
	batches []int
	err     error
	until   time.Time
}

// NewFake builds a Fake producing vectors of dim elements.
func NewFake(dim int) *Fake {
	if dim <= 0 {
		dim = 8
	}
	return &Fake{dim: dim}
}

// Embed returns the deterministic vector for each text, or the injected
// error, and records the call.
func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.batches = append(f.batches, len(texts))
	if !f.until.IsZero() && time.Now().Before(f.until) {
		return nil, ErrBreakerOpen
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

// vector derives the unit vector for text from an FNV seed.
func (f *Fake) vector(text string) []float32 {
	h := fnv.New64a()
	h.Write([]byte(text))
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], h.Sum64())
	r := rand.New(rand.NewChaCha8(seed))
	v := make([]float32, f.dim)
	var norm float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

// Model names the fake model.
func (f *Fake) Model() string { return "fake" }

// Dim is fixed at construction.
func (f *Fake) Dim() int { return f.dim }

// Calls is how many times Embed was called.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Batches is the size of every Embed call so far, in order.
func (f *Fake) Batches() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.batches...)
}

// SetError makes every Embed fail with err until cleared with nil.
func (f *Fake) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// SetBreakerOpenUntil simulates an open breaker: Embed returns
// ErrBreakerOpen and Status reports it until t. Zero closes it.
func (f *Fake) SetBreakerOpenUntil(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.until = t
}

// Status mirrors Client.Status for the injected state.
func (f *Fake) Status() Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := Health{Healthy: true, Host: "fake"}
	if !f.until.IsZero() && time.Now().Before(f.until) {
		h.Healthy = false
		h.BreakerOpenUntil = f.until
	}
	if f.err != nil {
		h.Healthy = false
		h.LastError = f.err.Error()
	}
	return h
}
