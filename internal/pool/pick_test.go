package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// rangeAtFake adds the buffer-filling read to a fake, so the pool's
// ReadRangeAt fans out instead of reporting ErrUnsupported.
type rangeAtFake struct{ *fakeprovider.Fake }

func (f rangeAtFake) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	rc, err := f.ReadRange(ctx, id, version, off, int64(len(buf)))
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.ReadFull(rc, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return n, err
}

// unofficialFake is a fake on the unofficial tier, the kind of drive
// read_fanout auto holds to one stream per file.
func unofficialFake(name string) *fakeprovider.Fake {
	f := fakeprovider.New(name)
	caps := f.Capabilities()
	caps.Tier = provider.TierUnofficial
	f.SetCaps(caps)
	return f
}

func rangeAtPool(t *testing.T, fakes ...*fakeprovider.Fake) *Pool {
	t.Helper()
	providers := make([]provider.Provider, len(fakes))
	for i, f := range fakes {
		providers[i] = rangeAtFake{f}
	}
	return newPoolOf(t, config.Pool{Replicas: len(fakes), MinReplicas: 1}, providers...)
}

// readBlocksAt reads every block of e through window concurrent ReadRangeAt
// calls and checks every byte.
func readBlocksAt(t *testing.T, p *Pool, e provider.Entry, want []byte, window int) {
	t.Helper()
	blocks := len(want) / fanBlock
	next := make(chan int, blocks)
	for i := 0; i < blocks; i++ {
		next <- i
	}
	close(next)
	errs := make(chan error, blocks)
	var wg sync.WaitGroup
	for w := 0; w < window; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, fanBlock)
			for i := range next {
				off := int64(i) * fanBlock
				n, err := p.ReadRangeAt(context.Background(), e.ID, e.Version, off, buf)
				switch {
				case err != nil:
					errs <- fmt.Errorf("block %d: %w", i, err)
				case n != fanBlock || !bytes.Equal(buf, want[off:off+fanBlock]):
					errs <- fmt.Errorf("block %d: %d wrong bytes", i, n)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// TestUnofficialHoldersServeOneStreamEach: a file held by two drives on an
// unofficial API is never read as several concurrent ranges from either of
// them, however many blocks the reader asks for at once.
func TestUnofficialHoldersServeOneStreamEach(t *testing.T) {
	q1, q2 := unofficialFake("q1"), unofficialFake("q2")
	want := seedBig("/movie.mkv", 8, q1, q2)
	p := rangeAtPool(t, q1, q2)
	e := fileEntry(t, p, "movie.mkv")
	setLatency(20*time.Millisecond, q1, q2)

	readBlocksAt(t, p, e, want, 8)
	for _, q := range []*fakeprovider.Fake{q1, q2} {
		if peak := q.PeakConcurrent(); peak != 1 {
			t.Errorf("%s served %d ranges of one file at once, want 1", q.Name(), peak)
		}
	}
	if n := q1.Calls("ReadRange") + q2.Calls("ReadRange"); n != 8 {
		t.Fatalf("members served %d of 8 blocks", n)
	}
}

// TestUnofficialSoleHolderMayOverflow: when one unofficial drive is the only
// holder, the reader is not throttled to one stream — there is nobody to
// share with, and waiting would only slow the read.
func TestUnofficialSoleHolderMayOverflow(t *testing.T) {
	a, q := fakeprovider.New("a"), unofficialFake("q")
	want := seedBig("/solo.bin", 4, q)
	p := rangeAtPool(t, a, q)
	e := fileEntry(t, p, "solo.bin")
	setLatency(30*time.Millisecond, q)

	readBlocksAt(t, p, e, want, 4)
	if n := q.Calls("ReadRange"); n != 4 {
		t.Fatalf("the only holder served %d of 4 blocks", n)
	}
	if peak := q.PeakConcurrent(); peak < 2 {
		t.Fatalf("the only holder served %d range at a time; overflow is allowed for a sole holder", peak)
	}
}

// TestUnofficialBesideOfficialNeverStacks: with an official holder beside
// it, the unofficial member keeps to one stream of the file and the official
// one absorbs the rest.
func TestUnofficialBesideOfficialNeverStacks(t *testing.T) {
	a, q := fakeprovider.New("a"), unofficialFake("q")
	want := seedBig("/movie.mkv", 8, a, q)
	p := rangeAtPool(t, a, q)
	e := fileEntry(t, p, "movie.mkv")
	setLatency(30*time.Millisecond, a, q)

	readBlocksAt(t, p, e, want, 8)
	if peak := q.PeakConcurrent(); peak != 1 {
		t.Fatalf("the unofficial member served %d ranges of one file at once, want 1", peak)
	}
	onA, onQ := a.Calls("ReadRange"), q.Calls("ReadRange")
	if onQ == 0 || onA+onQ != 8 || onA < onQ {
		t.Fatalf("official a served %d and unofficial q %d of 8 blocks", onA, onQ)
	}
}

// TestPickWaitingForAStreamHonoursCancel: a read waiting for one of two busy
// unofficial holders gives up as soon as its context is cancelled, asks no
// member, and leaves no reservation or waiter behind.
func TestPickWaitingForAStreamHonoursCancel(t *testing.T) {
	q1, q2 := unofficialFake("q1"), unofficialFake("q2")
	seedBig("/movie.mkv", 3, q1, q2)
	p := rangeAtPool(t, q1, q2)
	e := fileEntry(t, p, "movie.mkv")
	const busy = 400 * time.Millisecond
	setLatency(busy, q1, q2)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(block int64) {
			defer wg.Done()
			buf := make([]byte, fanBlock)
			if _, err := p.ReadRangeAt(context.Background(), e.ID, e.Version, block*fanBlock, buf); err != nil {
				t.Errorf("busy read %d: %v", block, err)
			}
		}(int64(i))
	}
	deadline := time.Now().Add(busy / 2)
	for p.byName["q1"].inflight.Load() != 1 || p.byName["q2"].inflight.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the two busy reads never held both holders")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, fanBlock)
		_, err := p.ReadRangeAt(ctx, e.ID, e.Version, 2*fanBlock, buf)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting read after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(busy / 4):
		t.Fatal("a waiting read did not return when its context was cancelled")
	}

	wg.Wait()
	if n := q1.Calls("ReadRange") + q2.Calls("ReadRange"); n != 2 {
		t.Fatalf("members were asked %d times; the cancelled read must ask none", n)
	}
	p.pickMu.Lock()
	streams, waiters := len(p.streams), len(p.streamWake)
	p.pickMu.Unlock()
	if streams != 0 || waiters != 0 {
		t.Fatalf("after every read ended: %d streams and %d waiter channels left", streams, waiters)
	}
	for _, m := range p.members {
		if n := m.inflight.Load(); n != 0 {
			t.Fatalf("member %s still counts %d reads in flight", m.name, n)
		}
	}
}
