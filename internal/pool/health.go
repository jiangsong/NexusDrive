package pool

import (
	"context"
	"time"

	"cloudfs/internal/provider"
)

// Start runs the pool's background loops until Stop or ctx ends. For now
// that is the probe: a member that is down is asked something cheap every
// probe interval, so it comes back into service the moment it answers,
// without waiting for a user operation to happen to hit it.
func (p *Pool) Start(ctx context.Context) {
	p.bgMu.Lock()
	defer p.bgMu.Unlock()
	if p.stopBG != nil {
		return
	}
	stop := make(chan struct{})
	p.stopBG = stop
	interval := p.probeInterval()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p.bg.Add(2)
	go func() {
		defer p.bg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				p.ProbeOnce(ctx)
			}
		}
	}()
	// The repair worker: the queue every few seconds, a scan for files the
	// queue does not know about now and then. Sequential, so repair never
	// competes with itself for a member.
	go func() {
		defer p.bg.Done()
		work := time.NewTicker(repairInterval)
		defer work.Stop()
		scan := time.NewTicker(scanInterval)
		defer scan.Stop()
		if p.settings.Replicas > 1 {
			_, _ = p.ScanOnce(ctx)
		}
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-work.C:
				_, _ = p.RepairOnce(ctx)
			case <-scan.C:
				if p.settings.Replicas > 1 {
					_, _ = p.ScanOnce(ctx)
				}
			}
		}
	}()
}

// Stop ends the background loops.
func (p *Pool) Stop() {
	p.bgMu.Lock()
	stop := p.stopBG
	p.stopBG = nil
	p.bgMu.Unlock()
	if stop != nil {
		close(stop)
		p.bg.Wait()
	}
}

// ProbeOnce asks every member that is not up for its pool root. One answer
// heals it; the probe is the cheapest call the member has.
func (p *Pool) ProbeOnce(ctx context.Context) {
	for _, m := range p.members {
		st := m.health.Snapshot()
		if st.State == provider.HealthUp || st.State == provider.HealthDisabled || st.State == provider.HealthDraining {
			continue
		}
		id, err := p.rootDirID(ctx, m)
		if err != nil {
			if unreachable(err) {
				m.note(err)
			}
			continue
		}
		_, err = m.p.Stat(ctx, id)
		if err == nil || !unreachable(err) {
			m.note(nil)
		} else {
			m.note(err)
		}
	}
}
