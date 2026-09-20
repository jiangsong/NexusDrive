package upload

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"cloudfs/internal/journal"
)

// Progress is the upload queue's overall progress: one burst of work, from
// the moment the queue stopped being empty to the moment it is empty again.
// A person who copied a tree in wants one bar for the queue, not the count
// of rows in a table that changes every second.
//
// It lives here, beside the counters it is made of, because every surface
// needs the same answer: the console's bar, `cloudfs status`, `cloudfs
// uploads watch`, the upload_progress MCP tool and /metrics. Each of them
// re-deriving it from a queue reading is how the console came to be the only
// place a copy's progress could be seen at all.
//
// Totals are recomputed at every reading: rows still queued plus rows
// finished since the batch began, in files and in bytes. A row that leaves
// the queue without going up — dropped, dead-lettered, superseded — shrinks
// the total rather than counting as done.
//
// Nothing here is persisted, and deliberately so. journal.Stats.Done is not
// monotonic: finished rows are trimmed, so a bar computed from it would jump
// *backwards* halfway through a copy. A batch a restart found already
// running says Resumed instead of pretending to know how much of it had gone
// up before this process existed.
//
// One more thing it cannot do: two people copying two trees at once are one
// batch, because the queue has no column saying where a row came from.
// Everything worded around Progress therefore says "the upload queue", never
// "your copy".
type Progress struct {
	Active bool
	// Seq counts the batches this process has opened. A reader that sees it
	// change knows the figures it was drawing belong to a burst that has
	// ended and do not continue into this one.
	Seq int64
	// Resumed marks a batch that was already queued when this process
	// started: FilesDone counts what has gone up since the restart, not
	// since the copy began.
	Resumed    bool
	StartedAt  time.Time
	FinishedAt time.Time
	FilesTotal int64
	FilesDone  int64
	BytesTotal int64
	BytesDone  int64
	// Rate is bytes per second over roughly the last ten seconds and ETA
	// what is left at that rate. Both are zero on a batch that is not
	// moving bytes, and on a finished one.
	Rate float64
	ETA  time.Duration
	// Blocked counts queued rows that cannot run at all: they wait on a
	// directory creation that is dead, cancelled or gone. A queue that is
	// busy and going nowhere looks exactly like a slow one without it.
	Blocked int
	// InFlightBytes is the size of the rows being transferred right now.
	InFlightBytes int64
	// FilesDoneTotal and BytesDoneTotal are this process's lifetime counts,
	// which is what a Prometheus counter has to be fed: the batch delta
	// returns to zero at every boundary, and a counter may not.
	FilesDoneTotal int64
	BytesDoneTotal int64
}

// Percent is how far along the batch is, by the rule every surface follows.
func (p Progress) Percent() float64 {
	return PercentOf(p.Active, p.FilesTotal, p.FilesDone, p.BytesTotal, p.BytesDone)
}

// PercentOf is the one percentage formula in the system. It is by bytes when
// the batch has any and by files otherwise — a batch of directory creations
// and deletes has no bytes and still has progress — and an active batch is
// capped just short of 100% so a full bar never sits there while files are
// still going out.
//
// internal/control/web/transfer_progress.js is the same rule in JavaScript,
// for the console's bar; a terminal and a browser disagreeing by a digit
// about the same queue makes one of them wrong. The CLI's test pins the
// values against this.
func PercentOf(active bool, filesTotal, filesDone, bytesTotal, bytesDone int64) float64 {
	if !active && filesTotal == 0 {
		return 0
	}
	if !active {
		return 100
	}
	pct := 0.0
	switch {
	case bytesTotal > 0:
		pct = float64(bytesDone) / float64(bytesTotal) * 100
	case filesTotal > 0:
		pct = float64(filesDone) / float64(filesTotal) * 100
	}
	return math.Max(0, math.Min(99.5, pct))
}

// progressTracker turns queue readings into Progress snapshots. Only the
// uploader's supervisor tick feeds it; everything else reads snapshot.
type progressTracker struct {
	mu       sync.Mutex
	stats    func(context.Context) (journal.Stats, error)
	totals   func() (files, bytes int64)
	inflight func() int64
	now      func() time.Time

	// first is true until a reading has been taken, which is how a queue
	// found non-empty at startup becomes a resumed batch.
	first bool
	// empty counts consecutive readings that found nothing queued; see the
	// two-reading rule in observe.
	empty                int
	active               bool
	seq                  int64
	resumed              bool
	started              time.Time
	baseFiles, baseBytes int64
	rate                 rateMeter
	last                 Progress
}

func newProgressTracker(
	stats func(context.Context) (journal.Stats, error),
	totals func() (files, bytes int64),
	inflight func() int64,
	now func() time.Time,
) *progressTracker {
	if now == nil {
		now = time.Now
	}
	return &progressTracker{stats: stats, totals: totals, inflight: inflight, now: now, first: true}
}

// snapshot returns the last reading without asking the journal anything. A
// status page polling once a second must not turn into a second queue scan.
func (p *progressTracker) snapshot() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// sample folds one reading in. busy is what the supervisor's own listing of
// remotes with queued work found: when it found none and no batch is open,
// a queue reading can say nothing new, and the Stats query — which walks the
// queued directories recursively — is not made at all. An idle daemon
// therefore costs exactly what it cost before this existed.
func (p *progressTracker) sample(ctx context.Context, busy bool) Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !busy && !p.active {
		p.first = false
		return p.last
	}
	st, err := p.stats(ctx)
	if err != nil {
		// Keep the last reading rather than inventing an empty one: a
		// momentary failure must not close a batch that is still running.
		return p.last
	}
	startup := p.first
	p.first = false
	return p.observe(p.now(), st, startup)
}

func (p *progressTracker) observe(now time.Time, st journal.Stats, startup bool) Progress {
	queued := int64(st.Pending + st.Uploading)
	doneFiles, doneBytes := p.totals()
	if queued > 0 && !p.active {
		p.active = true
		p.seq++
		p.started = now
		p.resumed = startup
		p.baseFiles, p.baseBytes = doneFiles, doneBytes
		p.rate = rateMeter{}
		p.empty = 0
		slog.Info("upload: queue batch started",
			"batch", p.seq, "files", queued, "bytes", st.Bytes, "resumed", p.resumed)
	}
	if !p.active {
		// Between batches there is no bar, but the figures that are not part
		// of one stay current: /metrics reads its counters from them.
		p.last.FilesDoneTotal, p.last.BytesDoneTotal = doneFiles, doneBytes
		p.last.Blocked, p.last.InFlightBytes = st.Blocked, p.inFlightBytes()
		return p.last
	}
	cur := Progress{
		Active: true, Seq: p.seq, Resumed: p.resumed, StartedAt: p.started,
		FilesDone:      doneFiles - p.baseFiles,
		BytesDone:      doneBytes - p.baseBytes,
		Blocked:        st.Blocked,
		InFlightBytes:  p.inFlightBytes(),
		FilesDoneTotal: doneFiles,
		BytesDoneTotal: doneBytes,
	}
	cur.FilesTotal = queued + cur.FilesDone
	cur.BytesTotal = st.Bytes + cur.BytesDone
	cur.Rate = p.rate.observe(now, cur.BytesDone)
	if cur.Rate > 0 && cur.BytesTotal > cur.BytesDone {
		cur.ETA = time.Duration(float64(cur.BytesTotal-cur.BytesDone) / cur.Rate * float64(time.Second))
	}
	if queued == 0 {
		p.empty++
	} else {
		p.empty = 0
	}
	// A batch closes on the second consecutive empty reading, not the first.
	// A worker marks its row done in the journal and only then adds to the
	// lifetime counters, so a reading taken in that window sees an empty
	// queue and a done count one short — and the last thing a person sees of
	// a copy of 3434 files would be "3433 of 3433 files". One more reading,
	// a tick later, has the real figure. It also stops a batch splitting in
	// two when the queue happens to be empty for an instant between two
	// commits of the same copy.
	if queued == 0 && p.empty > 1 {
		p.active = false
		p.empty = 0
		cur.Active, cur.FinishedAt = false, now
		// A finished batch has no live rate, which is also what the console
		// shows (transfer_progress.js).
		cur.Rate, cur.ETA = 0, 0
		slog.Info("upload: queue batch drained",
			"batch", p.seq, "files", cur.FilesDone, "bytes", cur.BytesDone,
			"took", now.Sub(p.started).Round(time.Millisecond).String(), "dead", st.Dead)
	}
	p.last = cur
	return cur
}

func (p *progressTracker) inFlightBytes() int64 {
	if p.inflight == nil {
		return 0
	}
	return p.inflight()
}

// rateMeter is a 10 s exponentially weighted moving average of the transfer
// rate, which is what makes an ETA stop jumping between chunks. It is the
// meter the export queue uses (internal/export/admin.go), kept identical on
// purpose: two estimators disagreeing about the same machine's throughput is
// worse than either of them being slightly wrong.
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
