package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cloudfs/internal/cache"
)

// completeBlockLater fetches the rest of a block in the background.
func (f *FS) completeBlockLater(ctx context.Context, h *Handle, key cache.FileKey, idx int64) {
	node, mount := h.Node, h.Mount
	go func() {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		ph := &Handle{Ino: h.Ino, Node: node, Mount: mount, lastBlock: -1, window: 1}
		_, _ = f.blockFlight.Do(blockKey{ino: h.Ino, idx: idx}, func() ([]byte, error) {
			if f.cache.Has(key, idx) {
				return nil, nil
			}
			return f.fetchBlock(pctx, ph, key, idx)
		})
	}()
}

// blockAt returns one block, from cache or provider.
func (f *FS) blockAt(ctx context.Context, h *Handle, key cache.FileKey, idx int64) ([]byte, error) {
	if b, ok := f.cache.Get(key, idx); ok {
		return b, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		block, err := f.blockFlight.Do(blockKey{ino: h.Ino, idx: idx}, func() ([]byte, error) {
			// Another goroutine may have filled it while we waited.
			if b, ok := f.cache.Get(key, idx); ok {
				return b, nil
			}
			return f.fetchBlock(ctx, h, key, idx)
		})
		// A foreground read can arrive on a block owned by a stale read-ahead
		// run just as a seek cancels that run. Retry once with the foreground
		// context instead of leaking the background cancellation to the app.
		if !errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return block, err
		}
	}
	return nil, context.Canceled
}

func (f *FS) fetchBlock(ctx context.Context, h *Handle, key cache.FileKey, idx int64) ([]byte, error) {
	off, n := f.cache.BlockRange(idx, h.Node.Size)
	if n <= 0 {
		return nil, io.EOF
	}
	if IsLocalOnly(h.Node.RemoteID) {
		// The file exists only as the committed blob behind the cache. The
		// caller retries once under the node's current identity, because the
		// usual cause is the upload landing underneath this read; if the node
		// is still local-only the entry really is lost, and that surfaces
		// rather than becoming a provider request for an id the remote never
		// had.
		return nil, fmt.Errorf("%w: %q", errLocalOnlyGone, h.Node.Name)
	}
	// Not pooled: the cache keeps this buffer as the block's in-memory
	// copy while readers copy out of it, so it has no safe moment to be
	// recycled.
	buf := make([]byte, n)
	read, err := f.readRange(ctx, h, off, buf)
	if err != nil {
		return nil, err
	}
	if err := shortRead(int64(read), n, off, h.Node.Size); err != nil {
		return nil, err
	}
	buf = buf[:read]
	// A cache that is full degrades to uncached reads rather than failing.
	// The block is handed to the reader before it reaches the disk.
	if err := f.cache.PutAsync(key, idx, buf, h.Node.Size); err != nil && !errors.Is(err, cache.ErrNoSpace) {
		return nil, err
	}
	return buf, nil
}

// fetchBlockRun fetches a contiguous stretch of blocks with a single range
// request and splits the bytes into per-block slices, caching each the same
// way a single-block fetch does. idx must be sorted ascending and
// consecutive (idx[i+1] == idx[i]+1); callers form these groups themselves
// (see contiguousGroups) so a Reserve exclusion or an already-cached block in
// the middle of a window ends one run and starts another rather than being
// silently skipped over here.
func (f *FS) fetchBlockRun(ctx context.Context, h *Handle, key cache.FileKey, idx []int64) (map[int64][]byte, error) {
	bs := f.cache.BlockSize()
	first, last := idx[0], idx[len(idx)-1]
	off, firstLen := f.cache.BlockRange(first, h.Node.Size)
	if firstLen <= 0 {
		return nil, io.EOF
	}
	if IsLocalOnly(h.Node.RemoteID) {
		return nil, fmt.Errorf("%w: %q", errLocalOnlyGone, h.Node.Name)
	}
	_, lastLen := f.cache.BlockRange(last, h.Node.Size)
	total := (last-first)*bs + lastLen
	// Not pooled: pieces of this buffer are handed to the cache as each
	// block's own slice.
	buf := make([]byte, total)
	read, err := f.readRange(ctx, h, off, buf)
	if err != nil {
		return nil, err
	}
	if err := shortRead(int64(read), total, off, h.Node.Size); err != nil {
		return nil, err
	}
	buf = buf[:read]
	out := make(map[int64][]byte, len(idx))
	pos := int64(0)
	for _, i := range idx {
		if pos >= int64(len(buf)) {
			break // a short read at EOF leaves nothing for the trailing blocks
		}
		_, n := f.cache.BlockRange(i, h.Node.Size)
		end := pos + n
		if end > int64(len(buf)) {
			end = int64(len(buf))
		}
		block := buf[pos:end]
		if err := f.cache.PutAsync(key, i, block, h.Node.Size); err != nil && !errors.Is(err, cache.ErrNoSpace) {
			return out, err
		}
		out[i] = block
		pos += n
	}
	return out, nil
}

// contiguousGroups splits an ascending, deduplicated list of block indices
// into maximal runs of consecutive integers.
func contiguousGroups(idx []int64) [][]int64 {
	if len(idx) == 0 {
		return nil
	}
	var groups [][]int64
	start := 0
	for i := 1; i <= len(idx); i++ {
		if i == len(idx) || idx[i] != idx[i-1]+1 {
			groups = append(groups, idx[start:i])
			start = i
		}
	}
	return groups
}
