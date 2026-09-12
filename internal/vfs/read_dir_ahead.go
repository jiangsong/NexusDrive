package vfs

import (
	"context"
	"io"
	"sync"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// Directory read-ahead: the sibling prefetch.
//
// Block read-ahead makes one big file fast. It does nothing for the case
// that actually hurts — a thousand small files copied out of one
// directory, where every file costs an open, one range request and a
// close, strictly one after another, and the drive's request rate, not its
// bandwidth, is the ceiling. Reading each file in order and waiting for
// the round trip each time is the slowest possible way to spend a quota of
// requests per second.
//
// So when reads walk a directory in listing order, the files ahead are
// fetched whole, a few at a time, into the cache before anyone asks for
// them. Three files in order is the signal: enough that `cp -r` and a
// photo import are recognised, few enough that a random reader, which
// lands on an ordered pair often by chance, never is.
//
// Only small files take part. A big file already has block read-ahead, and
// pulling whole large files on speculation is how a prefetcher turns into
// a bandwidth bill.

const (
	// dirAheadArm is how many in-order reads open the window.
	dirAheadArm = 3
	// dirAheadFetchTimeout bounds one sibling fetch, so a stalled member
	// cannot hold a prefetch slot forever.
	dirAheadFetchTimeout = 2 * time.Minute
	// dirAheadWait is the longest a foreground read waits for a prefetch
	// of the same file that is already in flight. Waiting beats issuing a
	// second request for the same bytes; waiting forever does not.
	dirAheadWait = 30 * time.Second
)

// dirAheadState is what one directory remembers between reads: how far the
// reader has walked it in order, and how far prefetch has already claimed.
type dirAheadState struct {
	mu sync.Mutex
	// lastName is the name of the last file read in this directory and
	// inOrder how many reads in a row have moved forward through the
	// listing.
	lastName string
	inOrder  int
	// claimedTo is the last name prefetch has already looked past, so two
	// readers walking the same directory do not plan the same window twice.
	claimedTo string
}

// noteDirRead records that a handle has started reading, and prefetches the
// files after it when the reads are walking the directory in order. It is
// called once per handle, from the first read.
func (f *FS) noteDirRead(ctx context.Context, h *Handle) {
	limit := h.Mount.Policy.DirReadahead
	if limit <= 0 || f.meta == nil {
		return
	}
	node := h.Node
	if node.ParentIno == 0 || node.Name == "" || node.IsDir() {
		return
	}
	v, _ := f.dirAhead.LoadOrStore(node.ParentIno, &dirAheadState{})
	st := v.(*dirAheadState)

	st.mu.Lock()
	switch {
	case st.lastName == "":
		st.inOrder = 1
	case node.Name > st.lastName:
		st.inOrder++
	default:
		// A step backwards is a different reader, or the same one seeking:
		// either way the window has to earn its way open again.
		st.inOrder = 1
		st.claimedTo = ""
	}
	st.lastName = node.Name
	armed := st.inOrder >= dirAheadArm
	after := node.Name
	if armed && st.claimedTo >= after {
		armed = false // already planned past here
	}
	if armed {
		st.claimedTo = after
	}
	st.mu.Unlock()
	if !armed {
		return
	}
	parent, mount, policy := node.ParentIno, h.Mount, h.Mount.Policy
	go f.dirReadAhead(context.WithoutCancel(ctx), parent, after, mount, policy, limit)
}

// dirReadAhead fetches the small files that follow `after` in one
// directory's listing. It reads the listing from the index — a directory
// being walked has just been listed, so this costs no provider call — and
// stops at the first thing that is not worth prefetching.
func (f *FS) dirReadAhead(ctx context.Context, parent uint64, after string, mount Mount, policy CachePolicy, limit int) {
	kids, err := f.meta.ChildrenPage(ctx, parent, after, 0, limit)
	if err != nil || len(kids) == 0 {
		return
	}
	threshold := policy.SmallFileThreshold
	if threshold <= 0 {
		threshold = 4 << 20
	}
	slots := f.dirAheadSlots(mount)
	var wg sync.WaitGroup
	for _, kid := range kids {
		if ctx.Err() != nil {
			break
		}
		if !f.worthPrefetching(kid, threshold) {
			continue
		}
		// One prefetch per file, whoever asks: two readers in the same
		// directory must not both pull the same sibling.
		if !f.prefetching.claim(blockKey{ino: kid.Ino, idx: wholeFileBlock}) {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			f.prefetching.release(blockKey{ino: kid.Ino, idx: wholeFileBlock})
			return
		}
		wg.Add(1)
		go func(n meta.Node) {
			defer wg.Done()
			defer func() {
				<-slots
				f.prefetching.release(blockKey{ino: n.Ino, idx: wholeFileBlock})
			}()
			f.prefetchWholeFile(ctx, n, mount)
		}(kid)
	}
	wg.Wait()
}

// worthPrefetching decides whether one sibling is worth a speculative
// whole-file fetch: a small, remote, complete file that is not already in
// the cache.
func (f *FS) worthPrefetching(n meta.Node, threshold int64) bool {
	if n.IsDir() || n.Size <= 0 || n.Size > threshold {
		return false
	}
	if n.RemoteID == "" || IsLocalOnly(n.RemoteID) {
		return false // nothing to fetch: the bytes are already here
	}
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	if _, ok := f.cache.HydratedPath(key); ok {
		return false
	}
	if have, total := f.cache.Present(key); total > 0 && have >= total {
		return false
	}
	return true
}

// prefetchWholeFile reads one small file in a single range request and
// installs it as a complete cache object, which is what makes the read
// that follows cost nothing at all.
func (f *FS) prefetchWholeFile(ctx context.Context, n meta.Node, mount Mount) {
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	done := make(chan struct{})
	if _, loaded := f.dirAheadFlight.LoadOrStore(n.Ino, done); loaded {
		return // a foreground read is already on it
	}
	defer func() {
		f.dirAheadFlight.Delete(n.Ino)
		close(done)
	}()

	fetch, cancel := context.WithTimeout(ctx, dirAheadFetchTimeout)
	defer cancel()
	rc, err := mount.Provider.ReadRange(fetch, n.RemoteID, n.Version, 0, n.Size)
	if err != nil {
		return
	}
	defer rc.Close()
	// PutWhole streams: a prefetch never holds a whole file in memory.
	_ = f.cache.PutWhole(key, io.LimitReader(rc, n.Size), n.Size)
}

// awaitDirReadAhead makes a foreground read wait for a prefetch of the same
// file that is already in flight, rather than asking the drive for the same
// bytes a second time. A prefetch that is slow enough to be worth giving up
// on stops being worth waiting for.
func (f *FS) awaitDirReadAhead(ctx context.Context, ino uint64) {
	v, ok := f.dirAheadFlight.Load(ino)
	if !ok {
		return
	}
	done, _ := v.(chan struct{})
	if done == nil {
		return
	}
	t := time.NewTimer(dirAheadWait)
	defer t.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-t.C:
	}
}

// forgetDirReadAhead drops a directory's read order when its listing
// changes. What was "the next few files" is no longer that, and guessing
// from a listing that has moved under us is how a prefetcher spends
// requests on files nobody will open.
func (f *FS) forgetDirReadAhead(dir uint64) {
	f.dirAhead.Delete(dir)
}

// dirAheadSlots is the prefetch concurrency for one remote: enough to keep
// its request budget busy, always at least one less than the connection
// budget so a foreground read never has to wait behind speculation.
func (f *FS) dirAheadSlots(mount Mount) chan struct{} {
	if v, ok := f.dirAheadSem.Load(mount.Remote); ok {
		return v.(chan struct{})
	}
	caps := mount.Provider.Capabilities()
	n := int(caps.QPS.Download + 0.5)
	if n < 2 {
		n = 2
	}
	if caps.MaxConnsPerHost > 0 && n > caps.MaxConnsPerHost {
		n = caps.MaxConnsPerHost
	}
	n--
	if n < 1 {
		n = 1
	}
	v, _ := f.dirAheadSem.LoadOrStore(mount.Remote, make(chan struct{}, n))
	return v.(chan struct{})
}

// wholeFileBlock is the block index a whole-file prefetch claims. Real
// blocks are non-negative, so it can never collide with one.
const wholeFileBlock = -1
