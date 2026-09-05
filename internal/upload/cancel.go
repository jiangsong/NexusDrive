package upload

import (
	"context"
	"time"

	"cloudfs/internal/journal"
)

// Cancel durably prevents future attempts, then interrupts a live transfer.
// StateCancelling means the provider call has not yet returned. Even a stopped
// transfer may have changed the remote: cancellation is not remote rollback.
func (u *Uploader) Cancel(ctx context.Context, id string) (journal.State, error) {
	u.transferMu.Lock()
	defer u.transferMu.Unlock()
	state, err := u.opt.Journal.RequestCancel(ctx, id)
	if err != nil {
		return state, err
	}
	if cancel := u.transfers[id]; cancel != nil {
		cancel()
	} else if state == journal.StateCancelling {
		// No transfer has registered (or it has already exited). Holding the
		// registration lock proves it cannot start while we acknowledge the stop.
		if err := u.opt.Journal.FinishCancel(ctx, id); err != nil {
			return state, err
		}
		state = journal.StateCancelled
	}
	return state, nil
}

// registerTransfer closes the claim-to-start gap with Cancel. A stop that won
// before registration never makes a provider call.
func (u *Uploader) registerTransfer(ctx context.Context, up journal.Upload) (context.Context, func(), bool) {
	u.transferMu.Lock()
	defer u.transferMu.Unlock()
	check, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	row, err := u.opt.Journal.Get(check, up.ID)
	if err != nil {
		return ctx, func() {}, false
	}
	if row.State == journal.StateCancelling {
		_ = u.opt.Journal.FinishCancel(check, up.ID)
		return ctx, func() {}, false
	}
	if row.State != journal.StateUploading {
		return ctx, func() {}, false
	}
	if ctx.Err() != nil {
		_ = u.opt.Journal.Defer(check, up.ID, 0, "worker cancelled before transfer registration")
		return ctx, func() {}, false
	}
	if u.transfers == nil {
		u.transfers = map[string]context.CancelFunc{}
	}
	if u.transfers[up.ID] != nil {
		return ctx, func() {}, false
	}
	ctx, cancel := context.WithCancel(ctx)
	u.transfers[up.ID] = cancel
	return ctx, func() {
		cancel()
		u.transferMu.Lock()
		defer u.transferMu.Unlock()
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if row, err := u.opt.Journal.Get(cleanup, up.ID); err == nil {
			if row.State == journal.StateCancelling {
				_ = u.opt.Journal.FinishCancel(cleanup, up.ID)
			} else if row.State == journal.StateUploading {
				// A cancelled final bookkeeping context must not strand a claim.
				_ = u.opt.Journal.Defer(cleanup, up.ID, 0, "worker ended before completing bookkeeping")
			}
		}
		delete(u.transfers, up.ID)
	}, true
}
