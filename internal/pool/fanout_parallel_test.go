package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// rendezvous is a member whose tree operations refuse to finish until every
// member has been asked. A pool that fans a mutation out one member after
// another never gets there: the first call waits for a second that has not
// been made, times out, and the test fails without a stopwatch. A pool that
// asks every member at once passes at once.
type rendezvous struct {
	provider.Provider
	arrive func() error
}

// RootID is what provider.RootOf looks for; the embedded fake's would be
// hidden behind the wrapper otherwise, and the pool would mkdir under "".
func (r *rendezvous) RootID() string { return r.Provider.(*fakeprovider.Fake).RootID() }

func (r *rendezvous) Mkdir(ctx context.Context, parent, name string) (provider.Entry, error) {
	if err := r.arrive(); err != nil {
		return provider.Entry{}, err
	}
	return r.Provider.Mkdir(ctx, parent, name)
}

func (r *rendezvous) Delete(ctx context.Context, id string) error {
	if err := r.arrive(); err != nil {
		return err
	}
	return r.Provider.Delete(ctx, id)
}

// barrier hands out arrive functions that all return once n of them have
// been called, and an error once one of them has waited longer than grace.
func barrier(n int, grace time.Duration) func() error {
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
			return errors.New("the other members were never asked: the fan-out is sequential")
		}
	}
}

// TestTreeMutationsReachEveryMemberAtOnce: a mkdir on a two-member pool over
// two Drive accounts took 2.7 s — 1.3 s per member, one after the other —
// and mkdir is synchronous under the mount, so a copy of a repository with
// hundreds of directories spent most of its time in it. Every member is now
// asked in parallel; the slowest member sets the latency, not the sum.
func TestTreeMutationsReachEveryMemberAtOnce(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	arrive := barrier(2, 2*time.Second)
	p := newPoolOf(t, config.Pool{Replicas: 2, MinReplicas: 1},
		&rendezvous{Provider: a, arrive: arrive}, &rendezvous{Provider: b, arrive: arrive})
	ctx := context.Background()

	d, err := p.Mkdir(ctx, rootID, "photos")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if got := f.Tree(); len(got) != 1 || got[0] != "/photos/" {
			t.Fatalf("%s tree = %v", f.Name(), got)
		}
	}

	arrive2 := barrier(2, 2*time.Second)
	for _, m := range p.members {
		m.p.(*rendezvous).arrive = arrive2
	}
	if err := p.Delete(ctx, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if got := f.Tree(); len(got) != 0 {
			t.Fatalf("%s tree after delete = %v", f.Name(), got)
		}
	}
}
