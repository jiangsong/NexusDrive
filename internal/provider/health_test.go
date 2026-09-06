package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

type statusErr int

func (e statusErr) Error() string   { return fmt.Sprintf("http %d", int(e)) }
func (e statusErr) HTTPStatus() int { return int(e) }

// TestHealthStateMachineTransitions: what the outcomes of calls say about
// a backend, in order — and what they do not say. A 429 or a 404 is an
// answer, not an outage; a revoked token is down at once; a cancelled
// context says nothing; time turns down into out; one success heals.
func TestHealthStateMachineTransitions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := NewHealth(HealthOptions{Threshold: 3, OutAfter: 10 * time.Minute, Now: func() time.Time { return now }})
	step := func(name string, err error, want HealthState) {
		t.Helper()
		h.Note(err)
		if got := h.State(); got != want {
			t.Fatalf("%s: state = %s, want %s (%+v)", name, got, want, h.Snapshot())
		}
	}
	step("fresh", nil, HealthUp)
	step("a 404 is an answer", ErrNotFound, HealthUp)
	step("a 429 is an answer", ErrRateLimited, HealthUp)
	step("a conflict is an answer", ErrConflict, HealthUp)
	step("cancelled says nothing", context.Canceled, HealthUp)
	step("one transport failure", ErrTransient, HealthDegraded)
	step("second", fmt.Errorf("wrapped: %w", ErrTransient), HealthDegraded)
	step("a success heals", nil, HealthUp)
	step("first of three", statusErr(503), HealthDegraded)
	step("second of three", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, HealthDegraded)
	step("a 5xx is an outage too", statusErr(502), HealthDown)
	now = now.Add(5 * time.Minute)
	if got := h.State(); got != HealthDown {
		t.Fatalf("5 minutes down = %s", got)
	}
	now = now.Add(6 * time.Minute)
	if got := h.State(); got != HealthOut {
		t.Fatalf("11 minutes down = %s, want out", got)
	}
	step("it returns", nil, HealthUp)
	step("auth is down at once", ErrAuth, HealthDown)
	step("auth fixed", nil, HealthUp)
	h.SetDisabled(true)
	if got := h.State(); got != HealthDisabled {
		t.Fatalf("disabled = %s", got)
	}
	h.SetDraining(true)
	if got := h.State(); got != HealthDraining {
		t.Fatalf("draining wins = %s", got)
	}
	h.SetDraining(false)
	h.SetDisabled(false)
	if got := h.State(); got != HealthUp || !h.Snapshot().Usable() {
		t.Fatalf("cleared = %s", got)
	}
}

// TestInstrumentedProviderReportsHealth: every remote the daemon holds is
// instrumented, so every remote gets a reachability state from the calls it
// actually served, with no driver having to report one.
func TestInstrumentedProviderReportsHealth(t *testing.T) {
	st := NewStats()
	failing := &failingProvider{err: ErrTransient}
	p := Instrument(failing, st)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = p.Stat(ctx, "x")
	}
	if got := st.Health(); got.State != HealthDown || got.Failures != 3 || got.LastError == "" {
		t.Fatalf("after three failures: %+v", got)
	}
	failing.err = nil
	_, _ = p.Stat(ctx, "x")
	if got := st.Health(); got.State != HealthUp || got.LastOK.IsZero() {
		t.Fatalf("after a success: %+v", got)
	}
}

// failingProvider answers every call with err.
type failingProvider struct {
	Provider
	err error
}

func (s *failingProvider) Name() string       { return "stub" }
func (s *failingProvider) Capabilities() Caps { return Caps{} }
func (s *failingProvider) Stat(context.Context, string) (Entry, error) {
	return Entry{ID: "x"}, s.err
}
