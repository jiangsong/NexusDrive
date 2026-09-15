package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"cloudfs/internal/cache"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// fetch fills the part file from the mount, one range_size chunk at a time,
// skipping every chunk the bitmap already accounts for. This is what makes a
// resume exact: after a crash the plan is not rebuilt and only the missing
// ranges are asked for again.
func (m *Manager) fetch(ctx context.Context, job Job, it Item, part string) error {
	mt, err := m.sourceMount(job, it)
	if err != nil {
		return err
	}
	rangeSize := job.Options.RangeSize
	if rangeSize <= 0 {
		rangeSize = int64(m.cfg.RangeSize)
	}
	chunks := chunkCount(it.Size, rangeSize)

	// No truncate on open: the bytes an earlier run put here are the whole
	// point of the bitmap.
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return asDisk(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return asDisk(err)
	}
	if st.Size() != it.Size {
		if err := f.Truncate(it.Size); err != nil {
			return asDisk(err)
		}
	}
	bits := parseBitmap(it.Ranges, chunks)
	// The chunks to fetch are decided before any worker starts: once they
	// are running, the bitmap belongs to them.
	var missing []int
	for i := 0; i < chunks; i++ {
		if !bits.has(i) {
			missing = append(missing, i)
		}
	}
	w := &fileWriter{
		f: f, bits: bits, chunks: chunks, m: m, job: job, it: it,
		doneBytes: fetchedBytes(bits, chunks, it.Size, rangeSize),
	}

	streams := job.Options.Streams
	if it.Size < job.Options.MultiRangeMin {
		streams = 1
	}
	if streams < 1 {
		streams = 1
	}
	if chunks > 0 && streams > chunks {
		streams = chunks
	}

	key := cache.FileKey{Remote: it.Remote, RemoteID: it.RemoteID, Version: it.Version}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan chunkWork)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.memory.Acquire(runCtx, rangeSize); err != nil {
				fail(err)
				return
			}
			defer m.memory.Release(rangeSize)
			buf := make([]byte, rangeSize)
			for c := range work {
				if runCtx.Err() != nil {
					return
				}
				m.yield(runCtx)
				if err := m.fetchChunk(runCtx, mt, it, key, w, c, buf[:c.n]); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
	for _, i := range missing {
		off := int64(i) * rangeSize
		n := rangeSize
		if off+n > it.Size {
			n = it.Size - off
		}
		select {
		case work <- chunkWork{index: i, off: off, n: n}:
		case <-runCtx.Done():
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return asDisk(err)
	}
	// The last interval may be smaller than syncEvery. Now that it is on
	// disk, publish its bitmap as well so a failure during verification or
	// rename can resume without fetching it again.
	if err := w.checkpoint(ctx); err != nil {
		return err
	}
	return m.checkHash(ctx, job, it, part)
}

// checkHash digests the finished part file. The hash is computed from the
// disk rather than as the bytes go by, because the chunks arrive out of order
// under several streams and because a resumed file's earlier chunks were
// written by a previous process.
func (m *Manager) checkHash(ctx context.Context, job Job, it Item, part string) error {
	if it.HashType == "" || it.Hash == "" {
		st, err := os.Stat(part)
		if err != nil {
			return asDisk(err)
		}
		if st.Size() != it.Size {
			return fmt.Errorf("%w: wrote %d of %d bytes", provider.ErrTransient, st.Size(), it.Size)
		}
		return nil
	}
	sum, err := hashFile(part, it.HashType)
	if errors.Is(err, errUnknownHash) {
		return nil
	}
	if err != nil {
		return asDisk(err)
	}
	if sum == it.Hash {
		return nil
	}
	// The bytes on the disk cannot be trusted, so neither can the bitmap
	// that says they are there. Both go.
	_ = os.Remove(part)
	_ = m.store.Checkpoint(ctx, job.ID, it.Rel, "", 0)
	return fmt.Errorf("%w: %s is %s, the source reports %s", errHashMismatch, it.HashType, sum, it.Hash)
}

func (m *Manager) fetchChunk(ctx context.Context, mt vfs.Mount, it Item, key cache.FileKey, w *fileWriter, c chunkWork, buf []byte) error {
	finish := m.beginMemberRange(w.job.ID, it.Remote)
	completed := int64(0)
	defer func() { finish(completed) }()
	if err := m.fillChunk(ctx, mt, it, key, c.off, buf); err != nil {
		return err
	}
	if err := w.writeAt(buf, c.off); err != nil {
		return err
	}
	completed = int64(len(buf))
	return w.complete(ctx, c)
}

// fillChunk fills buf from the block cache where it can and from the mount
// where it cannot, so a partially cached file costs only the ranges that are
// actually missing. Nothing fetched here is put into the cache: an export is
// a one-pass read of bytes nobody is going to ask for again, and filling the
// cache with them would evict what the user is actually working on.
func (m *Manager) fillChunk(ctx context.Context, mt vfs.Mount, it Item, key cache.FileKey, off int64, buf []byte) error {
	ca := m.fs.Cache()
	total := int64(len(buf))
	filled := int64(0)
	for filled < total {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ca != nil {
			if n := servedFromCache(ca, key, off+filled, buf[filled:]); n > 0 {
				filled += n
				continue
			}
		}
		end := missingRunEnd(ca, key, off, filled, total)
		if err := m.readRange(ctx, mt, it, off+filled, buf[filled:end]); err != nil {
			return err
		}
		filled = end
	}
	return nil
}

// servedFromCache copies as much of dst as the cached block covering pos can
// supply, and reports 0 when the block is not there.
func servedFromCache(ca *cache.Cache, key cache.FileKey, pos int64, dst []byte) int64 {
	bs := ca.BlockSize()
	if bs <= 0 {
		return 0
	}
	idx, off := pos/bs, pos%bs
	if !ca.Has(key, idx) {
		return 0
	}
	want := bs - off
	if want > int64(len(dst)) {
		want = int64(len(dst))
	}
	n, ok := ca.ReadAt(key, idx, off, dst[:want])
	if !ok || n <= 0 {
		return 0
	}
	return int64(n)
}

// missingRunEnd finds how far the uncached run starting at filled reaches, so
// one request covers it all instead of one per block.
func missingRunEnd(ca *cache.Cache, key cache.FileKey, off, filled, total int64) int64 {
	if ca == nil {
		return total
	}
	bs := ca.BlockSize()
	if bs <= 0 {
		return total
	}
	end := filled
	for end < total {
		if end > filled && ca.Has(key, (off+end)/bs) {
			break
		}
		next := ((off+end)/bs+1)*bs - off
		if next > total {
			next = total
		}
		end = next
	}
	if end <= filled {
		return total
	}
	return end
}

// readRange fills buf from the provider, through the buffer-filling fast path
// when the backend offers one. It is the mount's provider and not
// vfs.FS.Read: a read through the VFS would count as foreground IO and would
// populate the block cache.
func (m *Manager) readRange(ctx context.Context, mt vfs.Mount, it Item, off int64, buf []byte) error {
	read := 0
	if ra, ok := mt.Provider.(provider.RangeReaderAt); ok {
		n, err := ra.ReadRangeAt(ctx, it.RemoteID, it.Version, off, buf)
		if !errors.Is(err, provider.ErrUnsupported) {
			if err != nil {
				return err
			}
			read = n
		}
	}
	if read == 0 {
		rc, err := mt.Provider.ReadRange(ctx, it.RemoteID, it.Version, off, int64(len(buf)))
		if err != nil {
			return err
		}
		n, err := io.ReadFull(rc, buf)
		closeErr := rc.Close()
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		read = n
	}
	// A range that came back short while the file is known to continue is a
	// server capping the request, not the end of the data. Accepting it would
	// write a hole into the middle of the exported file and call the chunk
	// done.
	if int64(read) < int64(len(buf)) && off+int64(read) < it.Size {
		return fmt.Errorf("%w: short range read: %d of %d bytes at %d (file is %d bytes)",
			provider.ErrTransient, read, len(buf), off, it.Size)
	}
	return nil
}
