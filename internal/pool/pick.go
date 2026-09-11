package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// Read fan-out (docs/pool-v2.md §4.5). VFS readahead issues one range
// request per block, several at a time. pickReplica sends each to the holder
// that is least loaded against its connection budget and fastest per MiB, so
// a file with three replicas reads at close to three drives' speed.
// read_fanout off keeps the v1 path — sortReplicas and tryReplicas, one
// ordered stream per file.

const (
	// defaultMaxInflight is a member's read budget when it declares no
	// Caps.MaxConnsPerHost.
	defaultMaxInflight = 4
	// scoreTie is how close two scores must be to count as equal. Latency
	// averages within ~20% of each other are noise on a real network;
	// calling them equal lets load and bytes served spread the reads,
	// instead of whichever member was momentarily faster taking them all.
	scoreTie = 0.2
	// degradedPenalty is what a member that has started failing (degraded,
	// not yet down) adds to its score: as much as a full connection budget,
	// so it serves reads only once the healthy holders are at theirs. v1
	// read from it only when no healthy replica was left; this keeps that
	// preference without leaving its bandwidth idle under load.
	degradedPenalty = 1.0
)

type fanoutMode int

const (
	fanoutAuto fanoutMode = iota
	fanoutOff
	fanoutAll
)

func parseFanout(s string) (fanoutMode, error) {
	switch s {
	case "", config.ReadFanoutAuto:
		return fanoutAuto, nil
	case config.ReadFanoutOff:
		return fanoutOff, nil
	case config.ReadFanoutAll:
		return fanoutAll, nil
	}
	return fanoutAuto, fmt.Errorf("read_fanout must be off, auto or all (got %q)", s)
}

// streamKey is one member serving ranges of one file.
type streamKey struct{ member, path string }

// limitsStreams reports whether m may serve only one range of a file at a
// time: a drive on an unofficial API under read_fanout auto, where several
// concurrent ranges of one file look like a download tool to risk control.
// pickReplica lifts the limit only for a sole usable holder.
func (p *Pool) limitsStreams(m *member) bool {
	return p.readFanout == fanoutAuto && m.unofficial
}

type candidate struct {
	m        *member
	r        replicaRow
	degraded bool
	load     float64
	score    float64
}

// pickReplica chooses the replica to serve one read and reserves it before
// returning: the member's inflight count and, when limitsStreams, its stream
// on the file. The caller returns the reservation through a lease.
//
// Candidates are the usable holders; a member that is down only when no
// healthier one holds the file, so its read doubles as the probe. Under
// read_fanout auto a member on an unofficial API that is already serving
// this file is not a candidate while any other usable holder is left. When
// every holder left is such a member, the pick waits for a stream of the
// file to end and looks again — health included — and returns ctx.Err() if
// the context ends first. Only a sole usable holder may take a second
// stream: there is nobody to share the file with.
//
// Among what remains, a member below its connection budget beats a
// saturated one unless all are saturated. The score is load
// (inflight/maxInflight) plus latency per MiB relative to the fastest
// measured candidate, plus degradedPenalty for a member that has started
// failing; an unmeasured member scores as the fastest, so it gets its
// chance. Scores within scoreTie are equal, and then the less loaded member,
// then the one that has served fewer bytes, then declaration order wins —
// there is no stickiness to the first member declared.
//
// ok is false, with a nil error, when no holder can serve the read at all.
func (p *Pool) pickReplica(ctx context.Context, reps []replicaRow) (*member, replicaRow, bool, error) {
	for {
		p.pickMu.Lock()
		c, ok, wait := p.pickLocked(reps)
		if ok {
			c.m.inflight.Add(1)
			if p.limitsStreams(c.m) {
				p.streams[streamKey{c.m.name, c.r.path}]++
			}
		}
		p.pickMu.Unlock()
		if ok {
			return c.m, c.r, true, nil
		}
		if wait == nil {
			return nil, replicaRow{}, false, nil
		}
		// No lock is held while waiting: the lease that ends a stream
		// takes pickMu to close wait.
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, replicaRow{}, false, ctx.Err()
		}
	}
}

// pickLocked is one look at the candidates, under pickMu. It answers the
// candidate to reserve; or, when more than one holder is usable but every
// one is an unofficial member already streaming the file, a channel closed
// when a stream of the file ends; or neither, when nothing can serve.
func (p *Pool) pickLocked(reps []replicaRow) (candidate, bool, <-chan struct{}) {
	probe := p.probeInterval()
	var healthy, probing []candidate
	for _, r := range reps {
		m := p.byName[r.member]
		if m == nil {
			continue
		}
		st := m.health.Snapshot()
		switch {
		case st.Usable():
			healthy = append(healthy, candidate{m: m, r: r, degraded: st.State == provider.HealthDegraded})
		case m.usable(probe):
			probing = append(probing, candidate{m: m, r: r})
		}
	}
	usable := len(healthy) + len(probing)
	if usable == 0 {
		return candidate{}, false, nil
	}
	cands := healthy
	if len(cands) == 0 {
		cands = probing
	}
	if usable > 1 {
		free := p.streamFree(healthy)
		if len(free) == 0 {
			free = p.streamFree(probing)
		}
		if len(free) == 0 {
			return candidate{}, false, p.streamWaitLocked(reps[0].path)
		}
		cands = free
	}
	cands = keepIf(cands, func(c candidate) bool {
		return int(c.m.inflight.Load()) < c.m.maxInflight
	})
	return choose(cands), true, nil
}

// streamFree drops the members limitsStreams holds to one stream of a file
// they are already serving. Called under pickMu.
func (p *Pool) streamFree(cands []candidate) []candidate {
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if !p.limitsStreams(c.m) || p.streams[streamKey{c.m.name, c.r.path}] == 0 {
			out = append(out, c)
		}
	}
	return out
}

// streamWaitLocked returns the channel closed when a stream of pth ends,
// shared by every pick waiting on that file. Called under pickMu; the lease
// that ends the stream closes and forgets it.
func (p *Pool) streamWaitLocked(pth string) <-chan struct{} {
	ch, ok := p.streamWake[pth]
	if !ok {
		ch = make(chan struct{})
		p.streamWake[pth] = ch
	}
	return ch
}

// keepIf filters cands, keeping all of them when none passes: a preference
// never leaves a read with nowhere to go.
func keepIf(cands []candidate, ok func(candidate) bool) []candidate {
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if ok(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return cands
	}
	return out
}

func choose(cands []candidate) candidate {
	lat := make([]float64, len(cands))
	fastest := 0.0
	for i, c := range cands {
		c.m.mu.Lock()
		if c.m.measured {
			lat[i] = c.m.latency
		}
		c.m.mu.Unlock()
		if lat[i] > 0 && (fastest == 0 || lat[i] < fastest) {
			fastest = lat[i]
		}
	}
	best := math.Inf(1)
	for i := range cands {
		c := &cands[i]
		c.load = float64(c.m.inflight.Load()) / float64(c.m.maxInflight)
		rel := 1.0
		if lat[i] > 0 && fastest > 0 {
			rel = lat[i] / fastest
		}
		c.score = c.load + rel
		if c.degraded {
			c.score += degradedPenalty
		}
		best = math.Min(best, c.score)
	}
	var pick *candidate
	for i := range cands {
		c := &cands[i]
		if c.score > best+scoreTie {
			continue
		}
		if pick == nil || lessBusy(c, pick) {
			pick = c
		}
	}
	return *pick
}

func lessBusy(a, b *candidate) bool {
	if a.load != b.load {
		return a.load < b.load
	}
	if sa, sb := a.m.served.Load(), b.m.served.Load(); sa != sb {
		return sa < sb
	}
	return a.m.order < b.m.order
}

// lease is one reservation from pickReplica, returned exactly once: when the
// call fails, when a buffer-filling read or a link resolve returns, or when a
// range body is drained or closed.
type lease struct {
	p     *Pool
	m     *member
	r     replicaRow
	start time.Time
	once  sync.Once
}

// done returns the reservation. A read that delivered n bytes — or a call
// without a body that completed — folds its duration into the member's
// latency and its bytes into served; a failed one does not.
func (l *lease) done(n int64, ok bool) {
	l.once.Do(func() {
		l.m.inflight.Add(-1)
		if l.p.limitsStreams(l.m) {
			k := streamKey{l.m.name, l.r.path}
			l.p.pickMu.Lock()
			if l.p.streams[k] <= 1 {
				delete(l.p.streams, k)
			} else {
				l.p.streams[k]--
			}
			if ch, ok := l.p.streamWake[k.path]; ok {
				close(ch)
				delete(l.p.streamWake, k.path)
			}
			l.p.pickMu.Unlock()
		}
		if ok {
			l.m.served.Add(n)
			l.m.noteLatency(time.Since(l.start), n)
		}
	})
}

// leasedBody keeps a range read's reservation until its body is drained or
// closed: the transfer, not the call that opened it, is what occupies the
// member.
type leasedBody struct {
	io.ReadCloser
	l *lease
	n atomic.Int64
}

func (b *leasedBody) Read(buf []byte) (int, error) {
	k, err := b.ReadCloser.Read(buf)
	n := b.n.Add(int64(k))
	switch {
	case errors.Is(err, io.EOF):
		b.l.done(n, true)
	case err != nil:
		b.l.done(n, false)
	}
	return k, err
}

func (b *leasedBody) Close() error {
	err := b.ReadCloser.Close()
	n := b.n.Load()
	b.l.done(n, n > 0)
	return err
}

// fanOut serves one read through pickReplica, moving to another holder when
// the chosen one cannot be talked to or does not have the file. On success
// do returns the lease itself — at once, or through a body it hands out; on
// failure fanOut returns it. Health notes, missing replicas and conflicts
// are handled as tryReplicas handles them.
func (p *Pool) fanOut(ctx context.Context, pth string, reps []replicaRow, do func(m *member, r replicaRow, l *lease) error) error {
	if len(reps) == 0 {
		return fmt.Errorf("%w: %s has no replica", provider.ErrNotFound, pth)
	}
	left := append([]replicaRow(nil), reps...)
	var lastUnreachable error
	for {
		m, r, ok, err := p.pickReplica(ctx, left)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		left = dropMember(left, r.member)
		l := &lease{p: p, m: m, r: r, start: time.Now()}
		err = do(m, r, l)
		if err == nil {
			m.note(nil)
			return nil
		}
		l.done(0, false)
		if ctx.Err() != nil {
			return err
		}
		switch {
		case errors.Is(err, provider.ErrUnsupported):
			// Nothing was asked of the member; the caller falls back to
			// another path. Says nothing about health.
			return err
		case unreachable(err):
			m.note(err)
			lastUnreachable = err
		case errors.Is(err, provider.ErrNotFound):
			m.note(nil)
			p.confirmMissing(ctx, m, r)
		default:
			// A conflict (the content changed under us) or a refusal
			// about the data: the caller refreshes or fails, as it would
			// on a single drive.
			m.note(nil)
			return err
		}
	}
	for _, r := range left {
		if m := p.byName[r.member]; m != nil && lastUnreachable == nil {
			lastUnreachable = fmt.Errorf("member %s is %s", m.name, m.state())
		}
	}
	if lastUnreachable != nil {
		return fmt.Errorf("%w: %s (last: %v)", provider.ErrUnavailable, pth, lastUnreachable)
	}
	return fmt.Errorf("%w: every replica of %s is gone", provider.ErrNotFound, pth)
}

// dropMember removes name's replica from reps, in place.
func dropMember(reps []replicaRow, name string) []replicaRow {
	out := reps[:0]
	for _, r := range reps {
		if r.member != name {
			out = append(out, r)
		}
	}
	return out
}

// confirmMissing asks a member that answered 404 for a replica whether the
// file is really gone. A drive refreshing a direct link — or one of several
// members answering at once — can say 404 for a file it still has, so only a
// Stat that agrees marks the replica missing for repair to rebuild. A Stat
// that finds the file, or cannot be answered, leaves the replica live.
func (p *Pool) confirmMissing(ctx context.Context, m *member, r replicaRow) {
	_, err := m.p.Stat(ctx, r.remoteID)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		_, _ = p.execIndex(ctx, `UPDATE replicas SET state = 'missing' WHERE path = ? AND member = ?`, r.path, r.member)
	case err != nil && unreachable(err):
		m.note(err)
	}
}
