package index

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"cloudfs/internal/embed"
)

// The embed worker (docs/agent-roadmap.md §3.9, TODO.md T-39) is the
// second background goroutine of the indexer. It drains embed_pending in
// batch-sized calls to the embedder, so a fresh index costs exactly
// ceil(chunks / batch) requests, and it is the one place that decides
// whether stored vectors and the configured endpoint still agree:
//
//   - a different model (or storage form) than index_meta records drops
//     every vector and queues every chunk again; chunks_fts is untouched;
//   - a different dimension than index_meta records, seen up front from
//     Dim() or after the first call, is recorded as an error and the
//     worker stops, because vectors of two sizes cannot be compared and
//     silently re-embedding a paid endpoint's whole corpus is not a call
//     to make on the endpoint's behalf;
//   - an open breaker on the endpoint puts the worker to sleep until it
//     closes, not into a retry loop.
//
// Failed batches back off exponentially per chunk and are given up after
// embedMaxAttempts; a later prepare pass queues them again.

const (
	// PausedBreaker and PausedStopped are EmbedReport.Paused values.
	PausedBreaker = "breaker"
	PausedStopped = "stopped"

	defaultEmbedBatch = 64
	embedBackoffBase  = 30 * time.Second
	embedBackoffMax   = time.Hour
	embedMaxAttempts  = 8
	// embedBreakerFallback is how long the worker sleeps on ErrBreakerOpen
	// from an embedder that does not report when it closes.
	embedBreakerFallback = time.Minute
)

// EmbedReport counts what one EmbedNow did.
type EmbedReport struct {
	// Embedded is the number of chunks whose vectors were stored; Calls the
	// number of embedder calls made; Deferred the chunks put back for a
	// later attempt.
	Embedded, Calls, Deferred int
	// Paused is "" or why the turn stopped early: breaker | stopped.
	Paused   string
	ResumeAt time.Time
}

// embedState is the worker's observable state.
type embedState struct {
	mu sync.Mutex
	// prepared is set once the model/dimension check ran in this process.
	prepared bool
	// stopped is the fatal error that ended the worker (a dimension
	// mismatch); "" while running.
	stopped string
	// lastErr is the most recent batch failure, cleared by a success.
	lastErr string
	// paused and resumeAt mirror the last turn's EmbedReport.
	paused   string
	resumeAt time.Time
	// wake asks the worker for a turn.
	wake chan struct{}
	// turnMu serialises EmbedNow and the worker's own turns.
	turnMu sync.Mutex
}

// embedBatchSize is how many chunks one call carries.
//
// UNVERIFIED: against a real openai/ollama endpoint, that a batch of 64
// chunks of up to 1200 runes each (the code-window chunk size) stays under
// the endpoint's request size and token limits; only the fake embedder has
// taken such a batch.
func (x *Indexer) embedBatchSize() int {
	if n := x.opt.Config.Embedding.Batch; n > 0 {
		return n
	}
	return defaultEmbedBatch
}

// embedQuantize is the storage form the configuration asks for.
func (x *Indexer) embedQuantize() string {
	if x.opt.Config.Embedding.Quantize == QuantizeNone {
		return QuantizeNone
	}
	return QuantizeInt8
}

// kickEmbed wakes the embed worker without blocking.
func (x *Indexer) kickEmbed() {
	if x.embedder == nil {
		return
	}
	select {
	case x.emb.wake <- struct{}{}:
	default:
	}
}

// EmbedNow runs the model check and drains the embedding queue on the
// caller's goroutine, returning when the queue is empty, every remaining
// chunk waits for a later attempt, or the worker must pause (the breaker is
// open, the worker stopped). Without an embedder it does nothing.
func (x *Indexer) EmbedNow(ctx context.Context) (EmbedReport, error) {
	if x.embedder == nil {
		return EmbedReport{}, nil
	}
	x.emb.turnMu.Lock()
	defer x.emb.turnMu.Unlock()
	return x.embedTurn(ctx)
}

// embedPrepare reconciles the recorded model with the configured one and
// queues whatever lacks a vector. It runs once per process, before the
// first batch, and again on request (a test, a rebuild).
func (x *Indexer) embedPrepare(ctx context.Context) error {
	e := x.embedder
	model, quant := e.Model(), x.embedQuantize()
	recModel, err := x.store.Meta(ctx, metaEmbeddingModel)
	if err != nil {
		return err
	}
	recQuant, err := x.store.Meta(ctx, metaQuantize)
	if err != nil {
		return err
	}
	switch {
	case recModel == "":
		if err := x.store.SetMeta(ctx, metaEmbeddingModel, model); err != nil {
			return err
		}
		if err := x.store.SetMeta(ctx, metaQuantize, quant); err != nil {
			return err
		}
	case recModel != model || recQuant != quant:
		if err := x.store.ResetEmbeddings(ctx, model, quant); err != nil {
			return err
		}
		x.emb.mu.Lock()
		x.emb.stopped = ""
		x.emb.mu.Unlock()
	}
	if _, err := x.store.QueueEmbeds(ctx); err != nil {
		return err
	}
	// An embedder that knows its size before any call lets the mismatch
	// be caught without spending a request.
	if dim := e.Dim(); dim > 0 {
		if err := x.checkDim(ctx, dim); err != nil {
			return err
		}
	}
	x.emb.mu.Lock()
	x.emb.prepared = true
	x.emb.mu.Unlock()
	return nil
}

// checkDim compares dim with the recorded dimension, recording it when
// none is recorded yet. A mismatch stops the worker.
func (x *Indexer) checkDim(ctx context.Context, dim int) error {
	rec, err := x.store.Meta(ctx, metaEmbeddingDim)
	if err != nil {
		return err
	}
	if rec == "" {
		return x.store.SetMeta(ctx, metaEmbeddingDim, strconv.Itoa(dim))
	}
	if rec == strconv.Itoa(dim) {
		return nil
	}
	x.stopEmbedding(fmt.Sprintf("embedding model %s returns %d dimensions but the index was built with %s; "+
		"restore the previous endpoint or rebuild the index to re-embed everything", x.embedder.Model(), dim, rec))
	return nil
}

func (x *Indexer) stopEmbedding(why string) {
	x.emb.mu.Lock()
	x.emb.stopped = why
	x.emb.paused, x.emb.resumeAt = PausedStopped, time.Time{}
	x.emb.mu.Unlock()
}

// embedStopped returns the fatal error, "" while the worker runs.
func (x *Indexer) embedStopped() string {
	x.emb.mu.Lock()
	defer x.emb.mu.Unlock()
	return x.emb.stopped
}

func (x *Indexer) embedPause(why string, at time.Time) {
	x.emb.mu.Lock()
	x.emb.paused, x.emb.resumeAt = why, at
	x.emb.mu.Unlock()
}

// breakerOpenUntil reports when the embedder's breaker closes, zero when it
// is closed or the embedder does not say.
func (x *Indexer) breakerOpenUntil() time.Time {
	if r, ok := x.embedder.(embed.Reporter); ok {
		if until := r.Status().BreakerOpenUntil; until.After(x.now()) {
			return until
		}
	}
	return time.Time{}
}

// embedTurn prepares once, then takes batches until the queue is empty or
// the worker must stop.
func (x *Indexer) embedTurn(ctx context.Context) (EmbedReport, error) {
	var rep EmbedReport
	if why := x.embedStopped(); why != "" {
		rep.Paused = PausedStopped
		return rep, nil
	}
	x.emb.mu.Lock()
	prepared := x.emb.prepared
	x.emb.mu.Unlock()
	if !prepared {
		if err := x.embedPrepare(ctx); err != nil {
			return rep, err
		}
		if why := x.embedStopped(); why != "" {
			rep.Paused = PausedStopped
			return rep, nil
		}
	}
	batch := x.embedBatchSize()
	for ctx.Err() == nil {
		if until := x.breakerOpenUntil(); !until.IsZero() {
			x.embedPause(PausedBreaker, until)
			rep.Paused, rep.ResumeAt = PausedBreaker, until
			return rep, nil
		}
		x.embedPause("", time.Time{})
		items, err := x.store.PendingEmbeds(ctx, batch, x.now())
		if err != nil {
			return rep, err
		}
		if len(items) == 0 {
			return rep, nil
		}
		stop, err := x.embedBatch(ctx, items, &rep)
		if err != nil || stop {
			return rep, err
		}
	}
	return rep, ctx.Err()
}

// embedBatch sends one batch and stores or defers it. stop is true when the
// turn must end (the breaker opened, the worker stopped).
func (x *Indexer) embedBatch(ctx context.Context, items []PendingEmbed, rep *EmbedReport) (stop bool, err error) {
	texts := make([]string, len(items))
	ids := make([]int64, len(items))
	var chars int64
	for i, it := range items {
		texts[i], ids[i] = embedText(it), it.ChunkID
		chars += int64(utf8.RuneCountInString(texts[i]))
	}
	rep.Calls++
	vecs, err := x.embedder.Embed(ctx, texts)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		x.emb.mu.Lock()
		x.emb.lastErr = err.Error()
		x.emb.mu.Unlock()
		if errors.Is(err, embed.ErrBreakerOpen) {
			until := x.breakerOpenUntil()
			if until.IsZero() {
				until = x.now().Add(embedBreakerFallback)
			}
			x.embedPause(PausedBreaker, until)
			rep.Paused, rep.ResumeAt = PausedBreaker, until
			return true, nil
		}
		return false, x.deferBatch(ctx, items, rep)
	}
	if len(vecs) != len(items) || len(vecs) == 0 || len(vecs[0]) == 0 {
		x.emb.mu.Lock()
		x.emb.lastErr = fmt.Sprintf("embedder returned %d vectors for %d chunks", len(vecs), len(items))
		x.emb.mu.Unlock()
		return false, x.deferBatch(ctx, items, rep)
	}
	if err := x.checkDim(ctx, len(vecs[0])); err != nil {
		return true, err
	}
	if x.embedStopped() != "" {
		rep.Paused = PausedStopped
		return true, nil
	}
	if err := x.store.PutVectors(ctx, x.embedder.Model(), x.embedQuantize(), ids, vecs, chars); err != nil {
		return true, err
	}
	x.emb.mu.Lock()
	x.emb.lastErr = ""
	x.emb.mu.Unlock()
	rep.Embedded += len(items)
	return false, nil
}

// deferBatch puts a failed batch back with backoff, dropping chunks that
// have used up their attempts.
func (x *Indexer) deferBatch(ctx context.Context, items []PendingEmbed, rep *EmbedReport) error {
	var retry, giveUp []int64
	maxAttempts := 0
	for _, it := range items {
		if it.Attempts+1 >= embedMaxAttempts {
			giveUp = append(giveUp, it.ChunkID)
			continue
		}
		retry = append(retry, it.ChunkID)
		maxAttempts = max(maxAttempts, it.Attempts)
	}
	if len(giveUp) > 0 {
		if err := x.store.MarkEmbedded(ctx, giveUp); err != nil {
			return err
		}
	}
	if len(retry) > 0 {
		wait := min(embedBackoffBase<<uint(maxAttempts), embedBackoffMax)
		if err := x.store.DeferEmbeds(ctx, retry, x.now().Add(wait)); err != nil {
			return err
		}
		rep.Deferred += len(retry)
	}
	return nil
}

// embedWork is the worker goroutine: it takes a turn whenever woken and
// sleeps out a pause or a backoff instead of polling through it.
func (x *Indexer) embedWork(ctx context.Context) {
	defer func() { x.done <- struct{}{} }()
	var timer *time.Timer
	var timerC <-chan time.Time
	arm := func(at time.Time) {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(max(at.Sub(x.now()), time.Millisecond))
		timerC = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-x.emb.wake:
		case <-timerC:
			timerC = nil
		}
		x.emb.turnMu.Lock()
		rep, err := x.embedTurn(ctx)
		x.emb.turnMu.Unlock()
		if ctx.Err() != nil {
			continue
		}
		switch {
		case err != nil:
			// A store error: try again in a while rather than spin.
			arm(x.now().Add(embedBackoffBase))
		case rep.Paused == PausedBreaker:
			arm(rep.ResumeAt)
		case rep.Paused == PausedStopped:
		default:
			if at, err := x.store.NextEmbedAt(ctx, x.now()); err == nil && !at.IsZero() {
				arm(at)
			}
		}
	}
}

// embeddingStatus fills Status.Embedding.
func (x *Indexer) embeddingStatus(ctx context.Context, st Stats) EmbeddingStatus {
	cfg := x.opt.Config.Embedding
	out := EmbeddingStatus{
		Provider: cfg.Provider, Model: cfg.Model,
		Embedded: st.Vectors, Pending: st.EmbedPending,
	}
	if out.Provider == "" {
		out.Provider = "none"
	}
	if chars, err := x.store.EmbedChars(ctx, x.now()); err == nil {
		out.CharsThisMonth = chars
	}
	if x.embedder == nil {
		return out
	}
	out.Model = x.embedder.Model()
	out.Dim = x.embedder.Dim()
	if out.Dim == 0 {
		if rec, err := x.store.Meta(ctx, metaEmbeddingDim); err == nil {
			out.Dim, _ = strconv.Atoi(rec)
		}
	}
	out.Healthy = true
	if r, ok := x.embedder.(embed.Reporter); ok {
		h := r.Status()
		out.Remote, out.Host = h.Remote, h.Host
		out.Healthy, out.LastError, out.BreakerOpenUntil = h.Healthy, h.LastError, h.BreakerOpenUntil
	}
	x.emb.mu.Lock()
	if x.emb.stopped != "" {
		out.Healthy, out.LastError = false, x.emb.stopped
	} else if out.LastError == "" && x.emb.lastErr != "" {
		out.LastError = x.emb.lastErr
	}
	if x.emb.paused == PausedBreaker && out.BreakerOpenUntil.IsZero() && x.emb.resumeAt.After(x.now()) {
		// An embedder that does not report its breaker: the worker's own
		// sleep is what the operator would otherwise not see.
		out.Healthy, out.BreakerOpenUntil = false, x.emb.resumeAt
	}
	x.emb.mu.Unlock()
	limit := x.opt.Config.MaxChunks
	out.Capped = limit > 0 && st.Vectors+st.EmbedPending >= limit && st.Chunks > st.Vectors+st.EmbedPending
	return out
}
