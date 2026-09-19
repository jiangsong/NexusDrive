package upload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloudfs/internal/journal"
)

// ErrDeadLetters means a flush reached a terminal queue containing failures.
// The corresponding blobs are retained for inspection and manual retry.
var ErrDeadLetters = errors.New("upload: failed uploads remain")
var ErrCancelledUploads = errors.New("upload: cancelled uploads retain local content; remote results are not reconciled")
var ErrCleanupPending = errors.New("upload: local upload cleanup remains unfinished; remote results are not reconciled")

// Flush waits for all committed uploads to finish, including delayed retries
// and work already claimed by another worker. It does not flush open VFS
// handles, override backoff, or retry dead letters. Concurrent new commits may
// extend the wait; success describes the queue snapshot at return time.
//
// When Start has been called, only the existing worker pools perform uploads,
// preserving their per-remote concurrency. An unstarted uploader drains
// synchronously. The caller must supply a cancellation or deadline policy.
func (u *Uploader) Flush(ctx context.Context) (journal.Stats, error) {
	var st journal.Stats
	interval := min(u.opt.PollInterval, 100*time.Millisecond)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		u.mu.Lock()
		started, stopped, runCtx := u.runCtx != nil, u.stopped, u.runCtx
		u.mu.Unlock()
		if stopped {
			return st, errors.New("upload: uploader is stopped")
		}
		if started && runCtx.Err() != nil {
			return st, fmt.Errorf("upload: workers stopped: %w", runCtx.Err())
		}
		if !started {
			if _, err := u.DrainAll(ctx); err != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				return st, err
			}
		}
		var err error
		st, err = u.opt.Journal.Stats(ctx)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return st, err
		}
		if st.Purging > 0 {
			return st, ErrCleanupPending
		}
		if st.Cancelled > 0 || st.Cancelling > 0 {
			return st, ErrCancelledUploads
		}
		if st.Pending == st.Blocked && st.Uploading == 0 {
			// Nothing is running and nothing can: every pending row waits
			// on a directory creation that is dead or gone.
			if st.Dead > 0 {
				return st, fmt.Errorf("%w: %d; use uploads list or uploads retry", ErrDeadLetters, st.Dead)
			}
			if st.Blocked > 0 {
				return st, fmt.Errorf("%w: %d rows wait on a directory creation that no longer exists", ErrDeadLetters, st.Blocked)
			}
			return st, nil
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-timer.C:
		}
	}
}
