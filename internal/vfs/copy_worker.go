package vfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// StartCopies resumes persisted preparations on the journal-owning daemon.
// Foreground jobs keep exclusive preparation handles, so a scan cannot replay
// them. Shutdown cancels IO but preserves their last durable checkpoint.
func (f *FS) StartCopies(ctx context.Context, interval time.Duration) {
	f.copyWorkerMu.Lock()
	defer f.copyWorkerMu.Unlock()
	if f.copyClosed || f.copyCancel != nil || f.journal == nil || !f.journal.Owner() {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ctx, f.copyCancel = context.WithCancel(ctx)
	if f.copyWake == nil {
		f.copyWake = make(chan struct{}, 1)
	}
	wake := f.copyWake
	f.copyWG.Add(1)
	go func() {
		defer f.copyWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			f.resumeCopyJobs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
}

func (f *FS) StopCopies() {
	f.copyWorkerMu.Lock()
	f.copyClosed = true
	if f.copyCancel != nil {
		f.copyCancel()
	}
	f.copyWorkerMu.Unlock()
	f.copyWG.Wait()
}

func (f *FS) CopyWarning() string {
	f.copyWorkerMu.Lock()
	defer f.copyWorkerMu.Unlock()
	switch {
	case f.copyError != "" && f.serverCopyError != "":
		return f.copyError + "; " + f.serverCopyError
	case f.serverCopyError != "":
		return f.serverCopyError
	default:
		return f.copyError
	}
}

func (f *FS) resumeCopyJobs(ctx context.Context) {
	jobs, err := f.journal.CopyJobs(ctx)
	warning := ""
	if err != nil {
		warning = "copy preparation queue could not be read"
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if job.State == journal.CopyPurging {
			if err := f.ForgetCopy(ctx, job.ID); err != nil {
				warning = fmt.Sprintf("copy cleanup %s remains unfinished; content retained pending reference/storage checks", job.ID)
			}
			continue
		}
		if job.State == journal.CopySubmitted || job.State == journal.CopyCancelled {
			continue
		}
		if job.State == journal.CopyFailed {
			warning = fmt.Sprintf("copy preparation %s failed; its payload is retained for inspection", job.ID)
			continue
		}
		_, err := f.ResumeCopy(ctx, job.ID)
		if err == nil || errors.Is(err, journal.ErrCopyBusy) || errors.Is(err, journal.ErrCopyCancelled) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrCopySourceChanged) || errors.Is(err, ErrCopyBindingChanged) || errors.Is(err, meta.ErrCopyTargetChanged) || errors.Is(err, ErrExists) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrReadOnly) || errors.Is(err, journal.ErrCopyCorrupt) {
			_ = f.journal.FailCopy(ctx, job.ID, "copy recovery requires source/target reconciliation")
		}
		warning = fmt.Sprintf("copy preparation %s remains unfinished; inspect source, destination and storage", job.ID)
	}
	f.copyWorkerMu.Lock()
	f.copyError = warning
	f.copyWorkerMu.Unlock()
}
