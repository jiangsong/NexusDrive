package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// listRendezvous is a backend whose listing of a subdirectory refuses to
// finish until every sibling has been asked for too. A warm that walks its
// siblings one after another never gets there: the first listing waits for
// a second that has not been started, gives up, and the test fails without
// a stopwatch. A warm that asks for them at once passes at once. The root
// listing is let through: it is what produces the siblings.
type listRendezvous struct {
	provider.Provider
	root   string
	arrive func() error
}

func (r *listRendezvous) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if dirID != r.root {
		if err := r.arrive(); err != nil {
			return nil, "", err
		}
	}
	return r.Provider.List(ctx, dirID, cursor)
}

// listBarrier hands out arrive functions that all return once n of them
// have been called, and an error once one of them has waited longer than
// grace.
func listBarrier(n int, grace time.Duration) func() error {
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	return func() error {
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
			return nil
		case <-time.After(grace):
			return errors.New("the sibling directories were never listed: warm is sequential")
		}
	}
}

// TestWarmVisitsSiblingsConcurrently: Warm is what `cloudfs warm` and the
// MCP warm tool run, and a provider List is a round trip. Walking a wide
// directory one child at a time makes the caller pay the sum of those
// round trips, which on a real drive is minutes for a tree an agent asked
// to be able to search. The siblings are listed side by side, bounded by
// warmFanout, so the slowest listing sets the cost of a level, not the sum.
func TestWarmVisitsSiblingsConcurrently(t *testing.T) {
	arrive := listBarrier(3, 5*time.Second)
	e := newEnv(t, envOpt{wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		return &listRendezvous{Provider: f, root: f.RootID(), arrive: arrive}
	}})
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		e.fake.Seed(name+"/file.txt", []byte("x"))
	}

	dirs, err := e.fs.Warm(ctx, "/ali", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The mount root plus its three subdirectories.
	if dirs != 4 {
		t.Fatalf("warm visited %d directories, want 4", dirs)
	}
	calls := e.fake.TotalCalls()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := e.fs.ReadDirPath(ctx, "/ali/"+name); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.fake.TotalCalls(); n != calls {
		t.Fatalf("listing a warmed directory cost %d provider calls", n-calls)
	}
}

// TestWarmFanoutIsBounded: listing every directory at once would not be
// faster — meta serves listings through eight slots of its own and the
// provider limiter bounds the rate besides — it would only queue a whole
// tree's worth of calls up where a cancellation no longer reaches them.
func TestWarmFanoutIsBounded(t *testing.T) {
	e := newEnv(t, envOpt{})
	for i := 0; i < 24; i++ {
		e.fake.Seed(fmt.Sprintf("d%02d/file.txt", i), []byte("x"))
	}
	// Latency is what makes concurrent calls overlap long enough for
	// PeakConcurrent to see them; the assertion is an upper bound, so it
	// does not depend on how long they overlap.
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 5 * time.Millisecond })
	if _, err := e.fs.Warm(context.Background(), "/ali", -1); err != nil {
		t.Fatal(err)
	}
	if peak := e.fake.PeakConcurrent(); peak > warmFanout {
		t.Fatalf("warm had %d listings in flight at once, want at most %d", peak, warmFanout)
	}
}

// TestWarmStopsAtTheFirstError: the fan-out must not swallow a failure or
// keep listing a tree whose backend is gone. The first error is what the
// caller gets, and the count still reports what was listed.
func TestWarmStopsAtTheFirstError(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		e.fake.Seed(name+"/file.txt", []byte("x"))
	}
	// The root listing succeeds; every listing below it fails.
	if _, err := e.fs.Warm(ctx, "/ali", 0); err != nil {
		t.Fatal(err)
	}
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.Down = true })
	if _, err := e.fs.Warm(ctx, "/ali", 1); !errors.Is(err, provider.ErrTransient) {
		t.Fatalf("warm error = %v, want a transient provider error", err)
	}
}

// TestWarmHonoursCancellation: a cancelled context ends the walk rather
// than running the rest of the tree in the background.
func TestWarmHonoursCancellation(t *testing.T) {
	e := newEnv(t, envOpt{})
	for _, name := range []string{"a", "b", "c"} {
		e.fake.Seed(name+"/file.txt", []byte("x"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.fs.Warm(ctx, "/ali", -1); !errors.Is(err, context.Canceled) {
		t.Fatalf("warm on a cancelled context = %v, want context.Canceled", err)
	}
}
