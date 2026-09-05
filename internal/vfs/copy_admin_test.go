package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

type blockedCopyProvider struct {
	provider.Provider
	block   atomic.Bool
	entered chan struct{}
}

func (p *blockedCopyProvider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if p.block.Load() && off >= 1<<20 {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.Provider.ReadRange(ctx, id, version, off, n)
}

func TestCopyCancelInterruptsLiveDownloadAndExplicitRetryResumes(t *testing.T) {
	e := publicationEnv(t, t.TempDir())
	body := copyRecoveryBody()
	e.fake.Seed("source", body)
	p := &blockedCopyProvider{Provider: e.fake, entered: make(chan struct{}, 1)}
	p.block.Store(true)
	e.fs.mounts[0].Provider = p
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); done <- err }()
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal("download did not block")
	}
	jobs, err := e.j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Checkpoint != 1<<20 {
		t.Fatalf("checkpoint: %+v %v", jobs, err)
	}
	id := jobs[0].ID
	if err := e.fs.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled copy reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not interrupt provider IO")
	}
	job, err := e.j.GetCopy(ctx, id)
	if err != nil || job.State != journal.CopyCancelled || job.Checkpoint != 1<<20 {
		t.Fatalf("cancellation lost: %+v %v", job, err)
	}
	e.fs.resumeCopyJobs(ctx)
	if _, err := e.store.Resolve(ctx, "/ali/dest"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("cancelled copy was published: %v", err)
	}
	p.block.Store(false)
	if err := e.fs.RetryCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ResumeCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("retry content: %d %v", len(got), err)
	}
	if e.fake.ReadBytes() != int64(len(body)) {
		t.Fatalf("retry redownloaded prefix: %d", e.fake.ReadBytes())
	}
	if err := e.fs.CancelCopy(ctx, id); !errors.Is(err, journal.ErrCopyState) {
		t.Fatalf("cancel accepted after handoff: %v", err)
	}
}

func TestCopyRetryWakesSleepingWorker(t *testing.T) {
	dir := killCopyPreparation(t, "download")
	e, job := recoverCopyPreparation(t, dir)
	e.fake.Seed("source", copyRecoveryBody())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.j.FailCopy(ctx, job.ID, "transient failure"); err != nil {
		t.Fatal(err)
	}
	e.fs.StartCopies(ctx, time.Hour)
	defer e.fs.StopCopies()
	for e.fs.CopyWarning() == "" {
		if ctx.Err() != nil {
			t.Fatal("worker did not finish initial scan")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.fs.RetryCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	for {
		current, err := e.j.GetCopy(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == journal.CopySubmitted {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("retry did not wake worker: %+v", current)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCancelledBoundCopyStaysReadableAndCannotReviveAfterDelete(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	if err := e.fs.CancelCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	body := copyRecoveryBody()
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("cancel lost local version: %d %v", len(got), err)
	}
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RetryCopy(ctx, job.ID); !errors.Is(err, meta.ErrCopyTargetChanged) {
		t.Fatalf("retry accepted removed binding: %v", err)
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopyCancelled {
		t.Fatalf("refused retry changed state: %+v %v", current, err)
	}
}

func TestCancelledCopyRestoresLocalVersionWithoutResuming(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	ctx := context.Background()
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	id := jobs[0].ID
	if err := j.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	j.Close()
	e := publicationEnv(t, dir)
	key := cache.FileKey{Remote: "ali", RemoteID: localRemoteID(id), Version: localVersion(id)}
	e.cache.Forget(key) // Reconstructible cache link was lost while stopped.
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	body := copyRecoveryBody()
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("cancelled local content not restored: %d %v", len(got), err)
	}
	if !e.cache.IsPinned(key) {
		t.Fatal("restored local version remains evictable")
	}
	job, err := e.j.GetCopy(ctx, id)
	if err != nil || job.State != journal.CopyCancelled || e.fake.TotalCalls() != 0 {
		t.Fatalf("restore resumed cancelled work: %+v %v calls=%d", job, err, e.fake.TotalCalls())
	}
}
