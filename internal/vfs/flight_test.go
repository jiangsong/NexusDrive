package vfs

import (
	"errors"
	"testing"
	"time"
)

// TestReserveWaiterJoinsReservedKey checks that Do, called for a key another
// goroutine has reserved, blocks until that reservation resolves and returns
// its value without ever running its own fn.
func TestReserveWaiterJoinsReservedKey(t *testing.T) {
	var f flight[string, int]
	owned, resolve := f.Reserve([]string{"a"})
	if len(owned) != 1 || owned[0] != "a" {
		t.Fatalf("Reserve owned = %v, want [a]", owned)
	}

	type result struct {
		val int
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f.Do("a", func() (int, error) {
			t.Error("Do ran fn for a key already reserved by another caller")
			return -1, nil
		})
		done <- result{v, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("Do returned before resolve: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	resolve(map[string]int{"a": 42}, nil)

	select {
	case r := <-done:
		if r.val != 42 || r.err != nil {
			t.Fatalf("Do returned %+v, want {42 <nil>}", r)
		}
	case <-time.After(time.Second):
		t.Fatal("Do did not wake up after resolve")
	}
}

// TestReserveResolveErrorPropagates checks that resolving a reservation with
// an error delivers that error to every waiter.
func TestReserveResolveErrorPropagates(t *testing.T) {
	var f flight[string, int]
	_, resolve := f.Reserve([]string{"a", "b"})

	wantErr := errors.New("boom")
	type result struct {
		val int
		err error
	}
	doneA := make(chan result, 1)
	doneB := make(chan result, 1)
	go func() {
		v, err := f.Do("a", func() (int, error) { return -1, nil })
		doneA <- result{v, err}
	}()
	go func() {
		v, err := f.Do("b", func() (int, error) { return -1, nil })
		doneB <- result{v, err}
	}()

	// Give both Do calls a chance to register as waiters on the reserved
	// keys before resolving: otherwise a Do that has not run yet would see
	// an already-deleted key and run its own fn fresh instead of joining.
	select {
	case r := <-doneA:
		t.Fatalf("Do(a) returned before resolve: %+v", r)
	case r := <-doneB:
		t.Fatalf("Do(b) returned before resolve: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	resolve(nil, wantErr)

	for _, done := range []chan result{doneA, doneB} {
		select {
		case r := <-done:
			if !errors.Is(r.err, wantErr) {
				t.Fatalf("Do returned err %v, want %v", r.err, wantErr)
			}
		case <-time.After(time.Second):
			t.Fatal("Do did not wake up after resolve with error")
		}
	}
}

// TestReserveExcludesAlreadyInFlight checks that a key already in flight
// (via Do) is excluded from a Reserve call's owned keys, so the reserving
// caller never double-fetches it.
func TestReserveExcludesAlreadyInFlight(t *testing.T) {
	var f flight[string, int]
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = f.Do("busy", func() (int, error) {
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started

	owned, resolve := f.Reserve([]string{"busy", "free"})
	if len(owned) != 1 || owned[0] != "free" {
		t.Fatalf("Reserve owned = %v, want [free] (busy already in flight)", owned)
	}
	resolve(map[string]int{"free": 7}, nil)
	close(release)
}

// TestDoAfterResolveRunsFresh checks that once a reservation resolves and its
// keys leave the flight table, a later Do for the same key runs fn again
// instead of replaying the old result.
func TestDoAfterResolveRunsFresh(t *testing.T) {
	var f flight[string, int]
	_, resolve := f.Reserve([]string{"a"})
	resolve(map[string]int{"a": 1}, nil)

	ran := false
	v, err := f.Do("a", func() (int, error) {
		ran = true
		return 2, nil
	})
	if err != nil {
		t.Fatalf("Do returned err %v", err)
	}
	if !ran {
		t.Fatal("Do after resolve did not run fn fresh")
	}
	if v != 2 {
		t.Fatalf("Do after resolve returned %d, want 2", v)
	}
}
