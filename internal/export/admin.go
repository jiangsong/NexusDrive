package export

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cloudfs/internal/provider"
)

// Job reads one job.
func (m *Manager) Job(ctx context.Context, id string) (Job, error) { return m.store.Job(ctx, id) }

// Jobs lists jobs newest first.
func (m *Manager) Jobs(ctx context.Context, limit int, after string) ([]Job, string, error) {
	return m.store.Jobs(ctx, limit, after)
}

// Items lists one job's plan.
func (m *Manager) Items(ctx context.Context, id string) ([]Item, error) {
	return m.store.Items(ctx, id)
}

// Pause stops a job at its last checkpoint. Only the user can undo it.
func (m *Manager) Pause(ctx context.Context, id string) error {
	j, err := m.store.Job(ctx, id)
	if err != nil {
		return err
	}
	if j.State.Terminal() {
		return fmt.Errorf("export: job %s already finished", id)
	}
	if err := m.store.SetState(ctx, id, j.Revision, StatePaused, PauseUser); err != nil {
		return err
	}
	m.stopJob(id)
	return nil
}

// Resume puts a paused job back on the queue and clears the per-item
// back-offs, because a resume is the user saying the reason is gone.
func (m *Manager) Resume(ctx context.Context, id string) error {
	j, err := m.store.Job(ctx, id)
	if err != nil {
		return err
	}
	if j.State != StatePaused {
		return fmt.Errorf("export: job %s is %s, not paused", id, j.State)
	}
	if err := m.store.ClearBackoff(ctx, id); err != nil {
		return err
	}
	if err := m.store.SetState(ctx, id, j.Revision, StateRunning, PauseNone); err != nil {
		return err
	}
	m.Wake()
	return nil
}

// Cancel stops a job for good. The part files stay until Forget, so a
// cancelled job can still be inspected — and so a mistaken cancel has not
// thrown away a day of downloading.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	j, err := m.store.Job(ctx, id)
	if err != nil {
		return err
	}
	if j.State.Terminal() {
		return nil
	}
	if err := m.store.SetState(ctx, id, j.Revision, StateCancelled, PauseNone); err != nil {
		return err
	}
	m.stopJob(id)
	return nil
}

// Forget removes a finished job and the part files it left behind. Finished
// files stay: they are the user's copies now.
func (m *Manager) Forget(ctx context.Context, id string) error {
	j, err := m.store.Job(ctx, id)
	if err != nil {
		return err
	}
	if !j.State.Terminal() && j.State != StatePurging {
		return fmt.Errorf("export: job %s is still %s; cancel it first", id, j.State)
	}
	if j.State != StatePurging {
		if err := m.store.SetState(ctx, id, j.Revision, StatePurging, PauseNone); err != nil {
			return err
		}
		j, err = m.store.Job(ctx, id)
		if err != nil {
			return err
		}
	}
	return m.purge(ctx, j)
}

func (m *Manager) purge(ctx context.Context, j Job) error {
	items, err := m.store.Items(ctx, j.ID)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Kind == provider.KindDir || it.State == ItemDone || it.State == ItemSkipped {
			continue
		}
		_ = os.Remove(filepath.Join(j.Dest, filepath.FromSlash(it.Rel)) + partSuffix)
	}
	m.mu.Lock()
	delete(m.rates, j.ID)
	delete(m.probes, j.ID)
	m.mu.Unlock()
	return m.store.DeleteJob(ctx, j.ID)
}

// Progress reports a job's live rate and estimate.
func (m *Manager) Progress(ctx context.Context, id string) (Progress, error) {
	j, err := m.store.Job(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	return m.progressOf(j), nil
}

func (m *Manager) progressOf(j Job) Progress {
	p := Progress{BytesDone: j.BytesDone, BytesTotal: j.BytesTotal}
	m.mu.Lock()
	r := m.rates[j.ID]
	if r == nil {
		r = &rateMeter{}
		m.rates[j.ID] = r
	}
	p.Rate = r.observe(m.now(), j.BytesDone)
	m.mu.Unlock()
	if p.Rate > 0 && j.BytesTotal > j.BytesDone {
		p.ETA = time.Duration(float64(j.BytesTotal-j.BytesDone) / p.Rate * float64(time.Second))
	}
	return p
}

// rateMeter is a 10 s exponentially weighted moving average of the transfer
// rate, which is what makes an ETA stop jumping between chunks.
type rateMeter struct {
	last  time.Time
	bytes int64
	ewma  float64
}

func (r *rateMeter) observe(now time.Time, done int64) float64 {
	if r.last.IsZero() || done < r.bytes {
		r.last, r.bytes = now, done
		return r.ewma
	}
	dt := now.Sub(r.last).Seconds()
	if dt <= 0 {
		return r.ewma
	}
	sample := float64(done-r.bytes) / dt
	alpha := dt / (10 + dt)
	r.ewma += alpha * (sample - r.ewma)
	r.last, r.bytes = now, done
	return r.ewma
}
