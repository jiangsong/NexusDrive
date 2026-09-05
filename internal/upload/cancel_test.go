package upload

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
)

func TestFlushDoesNotReportSuccessWithPendingUploadCleanup(t *testing.T) {
	f := liveFixture(t, nil)
	u := f.queue(t, "discard.txt", []byte("retained"), "")
	if _, err := f.up.Cancel(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.j.BeginUploadCleanup(t.Context(), u.ID, "metadata"); err != nil {
		t.Fatal(err)
	}
	stats, err := f.up.Flush(t.Context())
	if err == nil || stats.Purging != 1 {
		t.Fatalf("unfinished cleanup reported as remote success: %+v %v", stats, err)
	}
}

func TestAdministrativeCancellationStopsLiveTransferWithoutRetry(t *testing.T) {
	g := &gateProvider{entered: make(chan struct{}), release: make(chan struct{})}
	f := liveFixture(t, func(p provider.Provider) provider.Provider { g.Provider = p; return g })
	u := f.queue(t, "cancel.txt", []byte("retained"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.up.DrainOnce(ctx, u.Remote) }()
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("not started")
	}
	if state, err := f.up.Cancel(ctx, u.ID); err != nil || state != journal.StateCancelling {
		t.Fatalf("cancel: %s %v", state, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("worker did not stop")
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateCancelled || row.Attempt != 0 {
		t.Fatalf("state: %+v %v", row, err)
	}
	if n, err := f.up.DrainAll(ctx); err != nil || n != 0 {
		t.Fatalf("restarted: %d %v", n, err)
	}
	if _, err := f.up.Flush(ctx); !errors.Is(err, ErrCancelledUploads) {
		t.Fatalf("false flush success: %v", err)
	}
	if _, err := os.Stat(u.BlobPath); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationBetweenClaimAndRegistrationMakesNoProviderCalls(t *testing.T) {
	f := liveFixture(t, nil)
	ctx := context.Background()
	u := f.queue(t, "cancel.txt", []byte("retained"), "")
	rows, err := f.j.Claim(ctx, u.Remote, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %+v %v", rows, err)
	}
	if _, err := f.up.Cancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	f.up.process(ctx, rows[0])
	if f.fake.TotalCalls() != 0 {
		t.Fatal("cancelled claim made provider calls")
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("state: %+v %v", row, err)
	}
}

type cancellationLateSuccess struct {
	provider.Provider
	entered chan struct{}
	release chan struct{}
}

func (p *cancellationLateSuccess) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	close(p.entered)
	<-p.release
	// A provider may have accepted the commit before the client cancelled.
	return p.Provider.CompleteUpload(context.WithoutCancel(ctx), s, parts)
}

func TestCancellationRetainsIntentWhenRemoteCompletesAfterCancel(t *testing.T) {
	p := &cancellationLateSuccess{entered: make(chan struct{}), release: make(chan struct{})}
	f := liveFixture(t, func(inner provider.Provider) provider.Provider { p.Provider = inner; return p })
	var published atomic.Int32
	f.up.opt.Hooks.OnSuccess = func(context.Context, journal.Upload, Result) error { published.Add(1); return nil }
	u := f.queue(t, "late.txt", []byte("possibly remote"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.up.DrainOnce(ctx, u.Remote) }()
	select {
	case <-p.entered:
	case <-ctx.Done():
		close(p.release)
		<-done
		t.Fatal("not completing")
	}
	state, err := f.up.Cancel(ctx, u.ID)
	close(p.release)
	<-done
	if err != nil || state != journal.StateCancelling {
		t.Fatalf("cancel: %s %v", state, err)
	}
	if published.Load() != 0 {
		t.Fatal("cancelled task published success")
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("lost intent: %+v %v", row, err)
	}
	if f.fake.Calls("CompleteUpload") != 1 {
		t.Fatal("fixture did not complete remotely")
	}
}

func TestCancellationCannotOverrideFinalPublication(t *testing.T) {
	f := liveFixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	f.up.opt.Hooks.OnSuccess = func(context.Context, journal.Upload, Result) error { close(entered); <-release; return nil }
	u := f.queue(t, "complete.txt", []byte("completed"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.up.DrainOnce(ctx, u.Remote) }()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		<-done
		t.Fatal("not publishing")
	}
	cancelled := make(chan error, 1)
	go func() { _, err := f.up.Cancel(ctx, u.ID); cancelled <- err }()
	close(release)
	<-done
	if err := <-cancelled; !errors.Is(err, journal.ErrCannotCancel) {
		t.Fatalf("cancelled published upload: %v", err)
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateDone {
		t.Fatalf("completion lost: %+v %v", row, err)
	}
}

func TestCancelledWorkerContextBeforeRegistrationReleasesClaim(t *testing.T) {
	f := liveFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	u := f.queue(t, "interrupted", []byte("retry normally"), "")
	rows, err := f.j.Claim(ctx, u.Remote, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %+v %v", rows, err)
	}
	cancel()
	f.up.process(ctx, rows[0])
	if row, err := f.j.Get(context.Background(), u.ID); err != nil || row.State != journal.StatePending {
		t.Fatalf("stranded claim: %+v %v", row, err)
	}
	if f.fake.TotalCalls() != 0 {
		t.Fatal("cancelled context called provider")
	}
}
