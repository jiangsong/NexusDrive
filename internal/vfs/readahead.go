package vfs

import (
	"context"
	"errors"
	"sync"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// coalesceCap is the largest number of blocks a single readahead range
// request may merge, regardless of how generous ReadaheadRequest is: a
// window this wide already amortises the request overhead, and merging
// further only makes a bad guess (a run that turns out unwanted, or one that
// comes back short) more expensive to discard.
const coalesceCap = 4

// coalesceBlocks returns how many contiguous missing blocks one readahead
// range request should cover for h's mount: derived from opt.ReadaheadRequest
// (or, when that is zero, from the provider's capabilities), clamped to
// [1, coalesceCap], and forced to 1 once this remote has been marked
// short-request-only by fetchReadaheadRun.
func (f *FS) coalesceBlocks(h *Handle) int {
	if _, disabled := f.readaheadDisabled.Load(h.Mount.Remote); disabled {
		return 1
	}
	bs := f.cache.BlockSize()
	req := f.opt.ReadaheadRequest
	if req <= 0 {
		caps := h.Mount.Provider.Capabilities()
		if caps.RangeRead && caps.QPS.Download <= 16 {
			req = 16 << 20
		} else {
			req = bs
		}
	}
	n := req / bs
	if n < 1 {
		return 1
	}
	if n > coalesceCap {
		return coalesceCap
	}
	return int(n)
}

// maybeReadAhead grows a sequential window and prefetches following blocks in
// the background. A read counts as sequential when it lands in the same or
// the next block and starts within seqSlack of where the previous read ended:
// the kernel's own read-ahead reorders requests inside its window, so exact
// contiguity is too strict, while block adjacency alone is too loose. The
// window only opens after seqArm such reads in a row.
//
// Once open, the window's missing, unclaimed blocks are grouped into runs of
// up to coalesceBlocks(h) contiguous blocks, and each run is fetched with one
// range request instead of one per block — see launchReadaheadRun.
func (f *FS) maybeReadAhead(ctx context.Context, h *Handle, key cache.FileKey, off, n int64) {
	// The sequential run is tracked even when prefetching is disabled:
	// wantsSubBlock relies on it to hand a sequential reader whole blocks.
	maxWindow := f.opt.ReadAheadBlocks
	bs := f.cache.BlockSize()
	idx := off / bs
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	adjacent := idx == h.lastBlock+1 || idx == h.lastBlock
	slack := f.seqSlack()
	near := off-h.lastEnd <= slack && h.lastEnd-off <= slack
	var staleCancel context.CancelFunc
	if adjacent && near {
		h.seqRun++
	} else {
		staleCancel = h.readAheadCancel
		h.readAheadCtx, h.readAheadCancel = nil, nil
		h.seqRun = 0
		h.window = 1
	}
	sequential := h.seqRun >= seqArm
	if sequential && maxWindow > 0 && h.window < maxWindow {
		h.window *= 2
		if h.window > maxWindow {
			h.window = maxWindow
		}
	}
	h.lastBlock = idx
	h.lastEnd = off + n
	window := h.window
	var readAheadCtx context.Context
	if sequential && window > 1 {
		if h.readAheadCtx == nil {
			h.readAheadCtx, h.readAheadCancel = context.WithCancel(context.WithoutCancel(ctx))
		}
		readAheadCtx = h.readAheadCtx
	}
	h.mu.Unlock()
	if staleCancel != nil {
		staleCancel()
	}
	if !sequential || window <= 1 {
		return
	}
	last := f.cache.BlockCount(h.Node.Size) - 1
	end := idx + int64(window)
	if end > last {
		end = last
	}
	node, mount := h.Node, h.Mount
	coalesce := f.coalesceBlocks(h)

	var run []int64
	flush := func() {
		if len(run) == 0 {
			return
		}
		f.launchReadaheadRun(readAheadCtx, h.Ino, node, mount, key, run)
		run = nil
	}
	for i := idx + 1; i <= end; i++ {
		if f.cache.Has(key, i) {
			flush()
			continue
		}
		if len(run) >= coalesce {
			flush()
		}
		bk := blockKey{ino: h.Ino, idx: i}
		// One claim per block in flight, whichever run it ends up in. The
		// kernel sends a READ every 128 KiB–1 MiB, and claiming without this
		// let overlapping windows from successive reads pile up duplicate
		// goroutines on the same blocks.
		if !f.prefetching.claim(bk) {
			flush()
			continue
		}
		run = append(run, i)
	}
	flush()
}

// launchReadaheadRun fetches one contiguous run of blocks, sharing the fetch
// of each with any other caller (foreground or background) already working
// on it via blockFlight.Reserve. A block Reserve reports as already in flight
// elsewhere is excluded from the run's own fetch — its prefetching claim is
// released immediately since this goroutine no longer owns it — which can
// leave gaps in the run; fetchReadaheadRun re-groups what remains into
// contiguous sub-runs before issuing range requests.
func (f *FS) launchReadaheadRun(ctx context.Context, ino uint64, node meta.Node, mount Mount, key cache.FileKey, run []int64) {
	keys := make([]blockKey, len(run))
	for i, idx := range run {
		keys[i] = blockKey{ino: ino, idx: idx}
	}
	owned, resolve := f.blockFlight.Reserve(keys)
	ownedSet := make(map[int64]struct{}, len(owned))
	for _, k := range owned {
		ownedSet[k.idx] = struct{}{}
	}
	for _, idx := range run {
		if _, ok := ownedSet[idx]; !ok {
			f.prefetching.release(blockKey{ino: ino, idx: idx})
		}
	}
	if len(owned) == 0 {
		return
	}
	ownedIdx := make([]int64, len(owned))
	for i, k := range owned {
		ownedIdx[i] = k.idx
	}
	go func() {
		defer func() {
			for _, idx := range ownedIdx {
				f.prefetching.release(blockKey{ino: ino, idx: idx})
			}
		}()
		// The run outlives the triggering read, but a seek/close cancels it
		// and the timeout still bounds a stationary reader.
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		ph := &Handle{Ino: ino, Node: node, Mount: mount, lastBlock: -1, window: 1}
		vals, err := f.fetchReadaheadRun(pctx, ph, key, mount.Remote, ownedIdx)
		out := make(map[blockKey][]byte, len(vals))
		for idx, b := range vals {
			out[blockKey{ino: ino, idx: idx}] = b
		}
		resolve(out, err)
	}()
}

// fetchReadaheadRun fetches a (possibly gapped) set of ascending block
// indices, grouping each maximal contiguous stretch into one range request.
// A stretch of more than one block that comes back short — a server that
// caps request size below what was asked — is retried as single-block
// requests, and remote is marked so this mount never coalesces again for the
// life of the process.
func (f *FS) fetchReadaheadRun(ctx context.Context, h *Handle, key cache.FileKey, remote string, idx []int64) (map[int64][]byte, error) {
	vals := make(map[int64][]byte, len(idx))
	for _, group := range contiguousGroups(idx) {
		if ctx.Err() != nil {
			return vals, ctx.Err()
		}
		if len(group) == 1 {
			b, err := f.fetchBlock(ctx, h, key, group[0])
			if err != nil {
				return vals, err
			}
			vals[group[0]] = b
			continue
		}
		blocks, err := f.fetchBlockRun(ctx, h, key, group)
		if err != nil {
			if !errors.Is(err, errShortRange) {
				return vals, err
			}
			// The server cannot serve a request this size: never ask it to
			// coalesce again, and recover this run one block at a time.
			f.readaheadDisabled.Store(remote, struct{}{})
			blocks = make(map[int64][]byte, len(group))
			for _, i := range group {
				b, err := f.fetchBlock(ctx, h, key, i)
				if err != nil {
					return vals, err
				}
				blocks[i] = b
			}
		}
		for i, b := range blocks {
			vals[i] = b
		}
	}
	return vals, nil
}

func (f *FS) cancelReadAheadOnSeek(h *Handle, off int64) {
	idx := off / f.cache.BlockSize()
	h.mu.Lock()
	if h.lastBlock < 0 {
		h.mu.Unlock()
		return
	}
	adjacent := idx == h.lastBlock+1 || idx == h.lastBlock
	slack := f.seqSlack()
	near := off-h.lastEnd <= slack && h.lastEnd-off <= slack
	var cancel context.CancelFunc
	if !adjacent || !near {
		cancel = h.readAheadCancel
		h.readAheadCtx, h.readAheadCancel = nil, nil
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// inflight is a set of blocks a prefetch goroutine is already working on.
type inflight struct {
	mu sync.Mutex
	m  map[blockKey]struct{}
}

func (s *inflight) claim(k blockKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[blockKey]struct{}{}
	}
	if _, ok := s.m[k]; ok {
		return false
	}
	s.m[k] = struct{}{}
	return true
}

func (s *inflight) release(k blockKey) {
	s.mu.Lock()
	delete(s.m, k)
	s.mu.Unlock()
}
