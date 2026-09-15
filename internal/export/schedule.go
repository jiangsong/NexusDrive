package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// The scheduler: one pass over the queue, the jobs it may run, the files
// those jobs owe, and what each outcome means for the two state machines of
// docs/pool-v2.md §5.5.

// RunOnce advances every runnable job by one scheduling pass: it plans what
// needs planning, transfers what is due, and finishes what has nothing left.
// The background loop calls it; so do tests, which is why it is synchronous.
func (m *Manager) RunOnce(ctx context.Context) error {
	if !m.store.Owner() {
		return ErrNotOwner
	}
	jobs, err := m.store.UnfinishedJobs(ctx)
	if err != nil {
		return err
	}
	var runnable []Job
	for _, j := range jobs {
		switch j.State {
		case StatePaused:
			m.maybeUnpause(ctx, j)
		case StatePurging:
			_ = m.purge(ctx, j)
		default:
			runnable = append(runnable, j)
		}
	}
	limit := m.cfg.JobsParallel
	if limit < 1 {
		limit = 1
	}
	if len(runnable) > limit {
		runnable = runnable[:limit]
	}
	var wg sync.WaitGroup
	for _, j := range runnable {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			m.advance(ctx, j)
		}(j)
	}
	wg.Wait()
	return ctx.Err()
}

// advance plans and then drains one job.
func (m *Manager) advance(ctx context.Context, j Job) {
	ctx, done := m.trackJob(ctx, j.ID)
	if ctx == nil {
		return // already running in another pass
	}
	defer done()
	if j.State == StatePlanning {
		if err := m.plan(ctx, j); err != nil {
			if ctx.Err() != nil {
				return
			}
			_ = m.store.SetError(ctx, j.ID, err.Error())
			if de := (diskError{}); errors.As(err, &de) {
				m.pause(ctx, j.ID, PauseDisk, err)
				return
			}
			cur, gerr := m.store.Job(ctx, j.ID)
			if gerr == nil {
				_ = m.store.SetState(ctx, j.ID, cur.Revision, StateFailed, PauseNone)
			}
			return
		}
		cur, err := m.store.Job(ctx, j.ID)
		if err != nil {
			return
		}
		if cur.State != StatePlanning {
			return // cancelled or paused while planning
		}
		if err := m.store.SetState(ctx, j.ID, cur.Revision, StateRunning, PauseNone); err != nil {
			return
		}
	}
	m.drain(ctx, j.ID)
	m.settle(ctx, j.ID)
}

// trackJob registers a per-job cancel so Pause and Cancel stop the transfers
// that are already in flight, and refuses a second runner for one job.
func (m *Manager) trackJob(ctx context.Context, id string) (context.Context, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil
	}
	if _, ok := m.running[id]; ok {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	m.running[id] = cancel
	return ctx, func() {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
		cancel()
	}
}

func (m *Manager) stopJob(id string) {
	m.mu.Lock()
	if c, ok := m.running[id]; ok {
		c()
	}
	m.mu.Unlock()
}

// drain transfers the job's due files, `transfers` at a time.
func (m *Manager) drain(ctx context.Context, id string) {
	var wg sync.WaitGroup
	defer wg.Wait()
	var jobFiles chan struct{}
	for {
		if ctx.Err() != nil {
			return
		}
		j, err := m.store.Job(ctx, id)
		if err != nil || j.State != StateRunning {
			return
		}
		limit := j.Options.Transfers
		if limit < 1 {
			limit = 1
		}
		if limit > m.cfg.Transfers {
			limit = m.cfg.Transfers
		}
		if jobFiles == nil {
			jobFiles = make(chan struct{}, limit)
		}
		items, err := m.store.DueItems(ctx, id, m.now(), j.Options.MultiRangeMin, limit)
		if err != nil {
			return
		}
		dispatched := 0
		for _, it := range items {
			ok, err := m.store.Claim(ctx, id, it.Rel)
			if err != nil || !ok {
				continue
			}
			dispatched++
			select {
			case jobFiles <- struct{}{}:
			case <-ctx.Done():
				_ = m.store.ReleaseItem(context.WithoutCancel(ctx), id, it.Rel, time.Time{}, false, "interrupted")
				return
			}
			select {
			case m.files <- struct{}{}:
			case <-ctx.Done():
				<-jobFiles
				_ = m.store.ReleaseItem(context.WithoutCancel(ctx), id, it.Rel, time.Time{}, false, "interrupted")
				return
			}
			wg.Add(1)
			go func(it Item) {
				defer wg.Done()
				defer func() { <-jobFiles }()
				defer func() { <-m.files }()
				m.handleItem(ctx, j, it)
			}(it)
		}
		if dispatched == 0 {
			// Everything due is either finished or in another worker's
			// hands. Let them settle, then look once more before declaring
			// the job drained.
			wg.Wait()
			more, err := m.store.DueItems(ctx, id, m.now(), j.Options.MultiRangeMin, 1)
			if err != nil || len(more) == 0 {
				return
			}
		}
	}
}

// handleItem runs one file and maps whatever went wrong onto the item and job
// states of docs/pool-v2.md §5.5.
func (m *Manager) handleItem(ctx context.Context, job Job, it Item) {
	err := m.exportFile(ctx, job, it)
	// A cancelled context has to persist its result through a live one, or
	// the item stays 'active' and waits for the next daemon start.
	store := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		_ = m.store.FinishItem(store, job.ID, it.Rel, ItemDone, it.Size, "")
		return
	case ctx.Err() != nil:
		_ = m.store.ReleaseItem(store, job.ID, it.Rel, time.Time{}, false, "interrupted")
		return
	}

	var de diskError
	switch {
	case errors.As(err, &de):
		// The destination, not the source. Keep the ranges: they are on the
		// disk, and the disk is usually back in a minute.
		_ = m.store.ReleaseItem(store, job.ID, it.Rel, time.Time{}, false, err.Error())
		m.pause(store, job.ID, PauseDisk, err)
	case errors.Is(err, errBindingChanged), errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrConflict):
		// The source is not what the plan described. That is this file's
		// problem, not the job's: fail it and carry on.
		_ = m.store.FinishItem(store, job.ID, it.Rel, ItemFailed, 0, err.Error())
	case errors.Is(err, errHashMismatch):
		if it.Attempts+1 >= maxAttempts {
			_ = m.store.FinishItem(store, job.ID, it.Rel, ItemFailed, 0, err.Error())
			return
		}
		_ = m.store.ReleaseItem(store, job.ID, it.Rel, time.Time{}, true, err.Error())
	case errors.Is(err, provider.ErrUnavailable):
		_ = m.store.ReleaseItem(store, job.ID, it.Rel, m.now().Add(unavailableBackoff(it.Attempts)), false, err.Error())
	default:
		switch retry.Classify(err) {
		case retry.ClassRiskControl:
			_ = m.store.ReleaseItem(store, job.ID, it.Rel, m.now().Add(riskControlRetry), false, err.Error())
			m.pause(store, job.ID, PauseRiskControl, err)
		case retry.ClassAuth:
			_ = m.store.ReleaseItem(store, job.ID, it.Rel, m.now().Add(unavailableRetry), false, err.Error())
			m.pause(store, job.ID, PauseAuth, err)
		case retry.ClassCanceled:
			_ = m.store.ReleaseItem(store, job.ID, it.Rel, time.Time{}, false, err.Error())
		case retry.ClassRetryable:
			// Unreachable is not a failure of this file and must not spend
			// its budget; it is deferred until the backend is worth asking
			// again.
			_ = m.store.ReleaseItem(store, job.ID, it.Rel, m.now().Add(unavailableBackoff(it.Attempts)), false, err.Error())
		default:
			if it.Attempts+1 >= maxAttempts {
				_ = m.store.FinishItem(store, job.ID, it.Rel, ItemFailed, 0, err.Error())
				return
			}
			_ = m.store.ReleaseItem(store, job.ID, it.Rel, m.now().Add(unavailableRetry), true, err.Error())
		}
	}
	_ = m.store.SetError(store, job.ID, err.Error())
}

// unavailableBackoff doubles from 30 s to an hour.
func unavailableBackoff(attempt int) time.Duration {
	d := unavailableRetry
	for i := 0; i < attempt && d < unavailableRetryMax; i++ {
		d *= 2
	}
	if d > unavailableRetryMax {
		d = unavailableRetryMax
	}
	return d
}

// pause moves a running job aside with a reason. A job somebody else already
// moved is left alone.
func (m *Manager) pause(ctx context.Context, id string, reason PauseReason, cause error) {
	j, err := m.store.Job(ctx, id)
	if err != nil || j.State != StateRunning {
		return
	}
	if cause != nil {
		_ = m.store.SetError(ctx, id, cause.Error())
	}
	_ = m.store.SetState(ctx, id, j.Revision, StatePaused, reason)
	m.mu.Lock()
	m.probes[id] = m.now()
	m.mu.Unlock()
}

// maybeUnpause is the automatic half of §5.5: a disk that came back and a
// backend whose back-off expired both resume on their own. A user pause does
// not.
func (m *Manager) maybeUnpause(ctx context.Context, j Job) {
	switch j.PauseReason {
	case PauseDisk:
		m.mu.Lock()
		last := m.probes[j.ID]
		due := last.IsZero() || !m.now().Before(last.Add(m.cfg.DiskProbeInterval))
		if due {
			m.probes[j.ID] = m.now()
		}
		m.mu.Unlock()
		if !due || !m.destWritable(j) {
			return
		}
	case PauseUnavailable, PauseRiskControl, PauseAuth:
		_, earliest, err := m.store.Pending(ctx, j.ID)
		if err != nil {
			return
		}
		if !earliest.IsZero() && m.now().Before(earliest) {
			return
		}
	default:
		return // a user pause waits for the user
	}
	if err := m.store.SetState(ctx, j.ID, j.Revision, StateRunning, PauseNone); err == nil {
		m.Wake()
	}
}

// destWritable probes the destination: the same filesystem as at planning
// time, and able to take a write.
func (m *Manager) destWritable(j Job) bool {
	if err := m.checkDest(j); err != nil {
		return false
	}
	probe := filepath.Join(j.Dest, markerName+partSuffix)
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	_, werr := f.Write([]byte("probe\n"))
	cerr := f.Close()
	_ = os.Remove(probe)
	return werr == nil && cerr == nil
}

// settle finishes a job that has nothing left to transfer: the mirror's
// deletions, the directory modification times, and the final state.
func (m *Manager) settle(ctx context.Context, id string) {
	store := context.WithoutCancel(ctx)
	j, err := m.store.Job(store, id)
	if err != nil || j.State != StateRunning {
		return
	}
	pending, earliest, err := m.store.Pending(store, id)
	if err != nil {
		return
	}
	if pending > 0 {
		if ctx.Err() != nil {
			return
		}
		// Everything left is waiting on a back-off, which means no source
		// can be reached right now. Say so rather than spinning.
		if !earliest.IsZero() && m.now().Before(earliest) {
			m.pause(store, id, PauseUnavailable, nil)
		}
		return
	}
	warning := ""
	if j.Options.Mirror {
		if err := m.mirrorDelete(store, j); err != nil {
			// A mirror that copied everything and failed to delete a stray
			// is a finished export with a note, not a failure.
			warning = err.Error()
		}
	}
	if err := m.stampDirs(store, j); err != nil && warning == "" {
		warning = err.Error()
	}
	if warning != "" {
		_ = m.store.SetError(store, id, warning)
	} else if j.FilesFailed == 0 {
		_ = m.store.SetError(store, id, "")
	}
	cur, err := m.store.Job(store, id)
	if err != nil || cur.State != StateRunning {
		return
	}
	_ = m.store.SetState(store, id, cur.Revision, StateDone, PauseNone)
}

// mirrorDelete removes what the destination holds and the plan does not.
func (m *Manager) mirrorDelete(ctx context.Context, j Job) error {
	extras, err := m.store.Extras(ctx, j.ID)
	if err != nil {
		return err
	}
	var failed []string
	for _, e := range extras {
		p := filepath.Join(j.Dest, filepath.FromSlash(e.Rel))
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			failed = append(failed, e.Rel)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("export: the mirror could not remove %d stray entries (first: %s)", len(failed), failed[0])
	}
	return nil
}

// stampDirs applies the source directories' modification times, deepest
// first, after everything inside them has been written.
func (m *Manager) stampDirs(ctx context.Context, j Job) error {
	if !j.Options.PreserveMTime {
		return nil
	}
	items, err := m.store.Items(ctx, j.ID)
	if err != nil {
		return err
	}
	var dirs []Item
	for _, it := range items {
		if it.Kind == provider.KindDir && it.Rel != "" {
			dirs = append(dirs, it)
		}
	}
	sort.Slice(dirs, func(a, b int) bool { return dirs[a].Rel > dirs[b].Rel })
	for _, d := range dirs {
		if d.MTime.IsZero() {
			continue
		}
		p := filepath.Join(j.Dest, filepath.FromSlash(d.Rel))
		if err := os.Chtimes(p, d.MTime, d.MTime); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("export: stamping %s: %w", d.Rel, err)
		}
	}
	return nil
}
