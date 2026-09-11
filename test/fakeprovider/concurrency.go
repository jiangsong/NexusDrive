package fakeprovider

import "context"

// admit applies Faults.MaxConcurrent to one call and counts it as being
// served, for PeakConcurrent. The returned func ends the call's turn. The
// caller holds no lock.
func (f *Fake) admit(ctx context.Context, max int) (func(), error) {
	f.mu.Lock()
	if max > 0 && cap(f.sem) != max {
		f.sem = make(chan struct{}, max)
	}
	sem := f.sem
	f.mu.Unlock()
	if max <= 0 {
		sem = nil
	}
	if sem != nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.active++
	if f.active > f.peak {
		f.peak = f.active
	}
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
		if sem != nil {
			<-sem
		}
	}, nil
}

// PeakConcurrent returns the most calls the fake has served at once since
// creation: a call counts from when MaxConcurrent admits it until its
// injected latency has elapsed. A test asserting "never two ranges of this
// file at once" reads it with Latency set, so overlapping calls overlap.
func (f *Fake) PeakConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}
