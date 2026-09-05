package upload

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func liveFixture(t *testing.T, wrap func(provider.Provider) provider.Provider) *fixture {
	t.Helper()
	j, err := journal.Open(journal.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	f := &fixture{j: j, fake: fakeprovider.New("ali")}
	var p provider.Provider = f.fake
	if wrap != nil {
		p = wrap(p)
	}
	f.up, err = New(Options{
		Journal: j, Providers: func(string) (provider.Provider, bool) { return p, true },
		Workers: 1, PollInterval: time.Millisecond, MaxAttempts: 2,
		Policy:  retry.Policy{MaxAttempts: 1},
		Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.up.Stop)
	return f
}

func TestFlushWaitsForDelayedRetry(t *testing.T) {
	f := liveFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := f.queue(t, "retry.txt", []byte("durable"), "")
	if err := f.j.Retry(ctx, u.ID, errors.New("offline"), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n, err := f.up.DrainAll(ctx); err != nil || n != 0 {
		t.Fatalf("due drain = %d, %v", n, err)
	}
	st, err := f.up.Flush(ctx)
	if err != nil || st.Done != 1 || st.Pending+st.Uploading != 0 {
		t.Fatalf("flush = %+v, %v", st, err)
	}
}

func TestFlushCancellationKeepsFutureWork(t *testing.T) {
	f := liveFixture(t, nil)
	u := f.queue(t, "later.txt", []byte("keep this"), "")
	if err := f.j.Retry(context.Background(), u.ID, errors.New("backoff"), time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := f.up.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush = %v", err)
	}
	row, err := f.j.Get(context.Background(), u.ID)
	if err != nil || row.State != journal.StatePending || row.Attempt != 1 {
		t.Fatalf("row = %+v, %v", row, err)
	}
	if b, err := os.ReadFile(row.BlobPath); err != nil || string(b) != "keep this" {
		t.Fatalf("blob = %q, %v", b, err)
	}
	if f.fake.Calls("BeginUpload") != 0 {
		t.Fatal("flush bypassed retry time")
	}
}

func TestFlushReportsDeadLetters(t *testing.T) {
	f := liveFixture(t, nil)
	u := f.queue(t, "failed.txt", []byte("keep"), "")
	if err := f.j.Fail(context.Background(), u.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	st, err := f.up.Flush(context.Background())
	if !errors.Is(err, ErrDeadLetters) || st.Dead != 1 {
		t.Fatalf("flush = %+v, %v", st, err)
	}
	if _, err := os.Stat(u.BlobPath); err != nil {
		t.Fatal(err)
	}
}

type gateProvider struct {
	provider.Provider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (p *gateProvider) BeginUpload(ctx context.Context, parent, name string, size int64, hashes provider.Hashes) (provider.UploadSession, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
		return provider.UploadSession{}, ctx.Err()
	case <-p.release:
		return p.Provider.BeginUpload(ctx, parent, name, size, hashes)
	}
}

func TestFlushObservesWorkersWithoutAddingConcurrency(t *testing.T) {
	gate := &gateProvider{entered: make(chan struct{}), release: make(chan struct{})}
	f := liveFixture(t, func(p provider.Provider) provider.Provider { gate.Provider = p; return gate })
	f.queue(t, "one.txt", []byte("one"), "")
	f.queue(t, "two.txt", []byte("two"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.up.Start(ctx)
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("worker did not start")
	}
	wait, stop := context.WithTimeout(ctx, 25*time.Millisecond)
	_, err := f.up.Flush(wait)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || gate.calls.Load() != 1 {
		t.Fatalf("flush = %v, calls %d", err, gate.calls.Load())
	}
	close(gate.release)
	st, err := f.up.Flush(ctx)
	if err != nil || st.Done != 2 {
		t.Fatalf("flush = %+v, %v", st, err)
	}
}

func TestCanceledTransferReleasesClaimWithoutRestart(t *testing.T) {
	gate := &gateProvider{entered: make(chan struct{}), release: make(chan struct{})}
	f := liveFixture(t, func(p provider.Provider) provider.Provider { gate.Provider = p; return gate })
	u := f.queue(t, "cancel.txt", []byte("retained"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.up.DrainOnce(ctx, "ali") }()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("transfer did not start")
	}
	cancel()
	<-done
	row, err := f.j.Get(context.Background(), u.ID)
	if err != nil || row.State != journal.StatePending || row.Attempt != 0 {
		t.Fatalf("canceled claim = %+v, %v", row, err)
	}
	close(gate.release)
	if st, err := f.up.Flush(context.Background()); err != nil || st.Done != 1 {
		t.Fatalf("resume = %+v, %v", st, err)
	}
}
