package provider

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// HealthState is what a backend looks like from here.
//
//	up        answering
//	degraded  recent failures, still answering some calls
//	down      not answering (or refusing every call): stop relying on it
//	out       down for longer than OutAfter: treat what it holds as lost
//	          until it returns (a pool rebuilds replicas elsewhere)
//	disabled  taken out of service by the operator
//	draining  being emptied before removal
type HealthState string

const (
	HealthUp       HealthState = "up"
	HealthDegraded HealthState = "degraded"
	HealthDown     HealthState = "down"
	HealthOut      HealthState = "out"
	HealthDisabled HealthState = "disabled"
	HealthDraining HealthState = "draining"
)

// HealthOptions tunes when failures add up to "down".
type HealthOptions struct {
	// Threshold is how many consecutive outage-class failures, with no
	// success between them, mark the backend down. Zero means 3.
	Threshold int
	// OutAfter is how long a backend stays down before it is out. Zero
	// means never.
	OutAfter time.Duration
	Now      func() time.Time
}

// Health tracks whether a backend can be reached, from the outcome of the
// calls made to it. The circuit breaker answers a different question — "is
// this account about to be banned" — and trips only on risk control; a
// drive whose API is unreachable never tripped anything and looked healthy
// while every call failed. This is the missing signal: a status page shows
// it for every remote, and a pool places and reads around it.
type Health struct {
	opt HealthOptions

	mu        sync.Mutex
	lastOK    time.Time
	lastFail  time.Time
	lastErr   string
	failures  int
	downSince time.Time
	disabled  bool
	draining  bool
}

// NewHealth returns a tracker that starts up.
func NewHealth(opt HealthOptions) *Health {
	if opt.Threshold <= 0 {
		opt.Threshold = 3
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Health{opt: opt}
}

// HealthSnapshot is what Health reports.
type HealthSnapshot struct {
	State     HealthState
	LastOK    time.Time
	LastError string
	LastFail  time.Time
	DownSince time.Time
	Failures  int
}

// isOutage reports whether err says the backend could not be reached or
// would not serve, as opposed to answering about the request: a 404, a
// version conflict or a 429 are answers from a working backend.
func isOutage(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrNotFound), errors.Is(err, ErrExists),
		errors.Is(err, ErrConflict), errors.Is(err, ErrUnsupported), errors.Is(err, ErrLinkExpired),
		errors.Is(err, ErrCursorReset):
		return false
	case errors.Is(err, ErrTransient), errors.Is(err, ErrAuth), errors.Is(err, ErrRiskControl), errors.Is(err, ErrUnavailable):
		return true
	}
	var hs interface{ HTTPStatus() int }
	if errors.As(err, &hs) {
		s := hs.HTTPStatus()
		return s >= 500 || s == 401
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// Note records the outcome of one call. A success clears the failure run;
// an outage-class failure lengthens it, and Threshold of them in a row mark
// the backend down. Authentication failure is down at once: retrying does
// not fix a revoked token. Cancellation says nothing about the backend.
func (h *Health) Note(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.opt.Now()
	if err == nil {
		h.lastOK = now
		h.failures = 0
		h.downSince = time.Time{}
		h.lastErr = ""
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if !isOutage(err) {
		// An answer, even a refusal: the backend is reachable.
		h.lastOK = now
		h.failures = 0
		h.downSince = time.Time{}
		h.lastErr = ""
		return
	}
	h.lastFail = now
	h.lastErr = err.Error()
	h.failures++
	if errors.Is(err, ErrAuth) && h.failures < h.opt.Threshold {
		h.failures = h.opt.Threshold
	}
	if h.failures >= h.opt.Threshold && h.downSince.IsZero() {
		h.downSince = now
	}
}

// State reports the current state.
func (h *Health) State() HealthState { return h.Snapshot().State }

// Snapshot reports the state with the facts behind it.
func (h *Health) Snapshot() HealthSnapshot {
	if h == nil {
		return HealthSnapshot{State: HealthUp}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := HealthSnapshot{State: HealthUp, LastOK: h.lastOK, LastError: h.lastErr, LastFail: h.lastFail, DownSince: h.downSince, Failures: h.failures}
	switch {
	case h.draining:
		s.State = HealthDraining
	case h.disabled:
		s.State = HealthDisabled
	case !h.downSince.IsZero():
		s.State = HealthDown
		if h.opt.OutAfter > 0 && h.opt.Now().Sub(h.downSince) >= h.opt.OutAfter {
			s.State = HealthOut
		}
	case h.failures > 0:
		s.State = HealthDegraded
	}
	return s
}

// SetDisabled and SetDraining are the operator's states; they override what
// the calls say until cleared.
func (h *Health) SetDisabled(on bool) {
	h.mu.Lock()
	h.disabled = on
	h.mu.Unlock()
}

func (h *Health) SetDraining(on bool) {
	h.mu.Lock()
	h.draining = on
	h.mu.Unlock()
}

// Usable reports whether calls should be sent to the backend now.
func (s HealthSnapshot) Usable() bool {
	return s.State == HealthUp || s.State == HealthDegraded
}
