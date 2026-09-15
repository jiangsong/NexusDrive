package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/textract"
	"cloudfs/internal/vfs"
)

// This file is the extraction side of the indexer: taking the queue in
// batches, standing aside for foreground IO, fetching bytes from the cache
// or the FS, and recording what the extractor made of them.

// drain takes the queue in batches until it is empty or a pause begins.
// Each batch holds runMu, so a pass and another drain interleave between
// batches rather than waiting out a long queue.
func (x *Indexer) drain(ctx context.Context, rep *ReconcileReport) {
	for ctx.Err() == nil {
		more, err := x.batch(ctx, rep)
		if err != nil || !more {
			return
		}
	}
}

// batch processes up to pendingBatch queued files and reports whether the
// queue may hold more.
func (x *Indexer) batch(ctx context.Context, rep *ReconcileReport) (bool, error) {
	x.runMu.Lock()
	defer x.runMu.Unlock()
	if why, _ := x.pausedUntil(); why != "" && why != PausedTextBudget {
		return false, errPaused
	}
	items, err := x.store.Pending(ctx, pendingBatch)
	if err != nil || len(items) == 0 {
		return false, err
	}
	st, err := x.store.Stats(ctx)
	if err != nil {
		return false, err
	}
	if limit := int64(x.opt.Config.MaxTotalText); limit > 0 && st.TextBytes >= limit {
		// Full. The queue keeps what it has; doctor reports the state and
		// the operator raises the limit or narrows the rules.
		x.pause(PausedTextBudget, time.Time{})
		return false, errPaused
	}
	if why, _ := x.pausedUntil(); why == PausedTextBudget {
		x.pause("", time.Time{})
	}
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		x.waitIdle(ctx)
		if err := x.process(ctx, it, rep); err != nil {
			if errors.Is(err, errPaused) || ctx.Err() != nil {
				x.refreshPending(ctx)
				return false, err
			}
			// A store or read error on one file must not stall the
			// queue behind it; the next pass queues the file again.
			_ = x.store.Dequeue(ctx, it.Ino)
		}
	}
	x.refreshPending(ctx)
	return len(items) == pendingBatch, nil
}

// waitIdle stands aside while foreground IO is in flight, for at most
// YieldMax, and counts the yield.
func (x *Indexer) waitIdle(ctx context.Context) {
	if !x.fs.Busy() {
		return
	}
	x.yields.Add(1)
	x.update(func(p *Progress) { p.Yields++; p.Paused = PausedBusy })
	defer x.update(func(p *Progress) {
		if p.Paused == PausedBusy {
			p.Paused = ""
		}
	})
	deadline := time.Now().Add(x.opt.YieldMax)
	for x.fs.Busy() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(busyPoll):
		}
	}
}

// process extracts one queued file. A file that is no longer there or no
// longer selected is dropped from the queue; an extraction failure is
// recorded on the document; a fetch refused by the budget or by risk
// control pauses the worker and leaves the file queued.
func (x *Indexer) process(ctx context.Context, it PendingItem, rep *ReconcileReport) error {
	n, err := x.fs.Meta().Get(ctx, it.Ino)
	if errors.Is(err, meta.ErrNotFound) || err == nil && n.IsDir() {
		return x.store.Dequeue(ctx, it.Ino)
	}
	if err != nil {
		return err
	}
	p, err := x.fs.Meta().Path(ctx, n.Ino)
	if err != nil {
		p = it.Path
	}
	pinned := x.pinnedSelects(n, p)
	_, ruled := x.matcher.Load().Match(p, n.Size)
	if !pinned && !ruled {
		x.update(func(p *Progress) { p.Skipped++ })
		if rep != nil {
			rep.Skipped++
		}
		return x.store.Dequeue(ctx, it.Ino)
	}
	existing, found, err := x.store.DocumentByRemote(ctx, n.Remote, n.RemoteID)
	if err != nil {
		return err
	}
	if found && existing.Version == n.Version && existing.State == DocOK {
		if existing.Path != p {
			if err := x.store.SetPath(ctx, existing.ID, p); err != nil {
				return err
			}
		}
		x.update(func(p *Progress) { p.Skipped++ })
		if rep != nil {
			rep.Skipped++
		}
		return x.store.Dequeue(ctx, it.Ino)
	}
	x.update(func(p *Progress) { p.Extracting = 1 })
	defer x.update(func(p *Progress) { p.Extracting = 0 })

	data, err := x.readFile(ctx, n, p, !ruled)
	switch {
	case errors.Is(err, errNotCached):
		return x.store.Dequeue(ctx, it.Ino)
	case errors.Is(err, provider.ErrRiskControl):
		// The account is being watched; every further call makes it
		// worse. Sleep, then retry this same file, since nothing about
		// it was wrong.
		// UNVERIFIED: whether a real quark risk-control response reaches
		// here as provider.ErrRiskControl through the driver's error
		// mapping rather than as ErrTransient; the fake provider does.
		x.pause(PausedRiskControl, x.now().Add(x.opt.RiskSleep))
		return errPaused
	case errors.Is(err, errPaused):
		return err
	case errors.Is(err, vfs.ErrNotFound):
		return x.store.Dequeue(ctx, it.Ino)
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return x.fail(ctx, n, p, "", err, rep)
	}
	d := Document{
		Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version, Path: p, Ino: n.Ino,
		Size: n.Size, MTimeNS: n.MTime.UnixNano(),
	}
	head := data
	if len(head) > headBytes {
		head = head[:headBytes]
	}
	kind := textract.KindOf(p, head)
	if kind == textract.KindUnsupported {
		return x.fail(ctx, n, p, "", textract.ErrUnsupported, rep)
	}
	d.Kind = string(kind)
	doc, err := safeExtract(ctx, kind, data, x.extract)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return x.fail(ctx, n, p, string(kind), err, rep)
	}
	d.Truncated = doc.Truncated
	if err := x.dropReplaced(ctx, p, n.RemoteID); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(doc.Text))
	if found && bytes.Equal(sum[:], existing.TextHash) {
		// Same text at a new version: move the version, keep the chunks.
		if err := x.store.TouchVersion(ctx, existing.ID, n.Version); err != nil {
			return err
		}
		if existing.Path != p {
			if err := x.store.SetPath(ctx, existing.ID, p); err != nil {
				return err
			}
		}
		x.update(func(p *Progress) { p.Skipped++ })
		if rep != nil {
			rep.Skipped++
		}
		return x.store.Dequeue(ctx, it.Ino)
	}
	chunks := textract.ChunkDoc(doc, kind, textract.DefaultChunkOptions())
	if _, err := x.store.UpsertDocument(ctx, d, doc.Text, chunks); err != nil {
		return err
	}
	// The upsert queued the new chunks for embedding; the embed worker
	// takes them from here.
	x.kickEmbed()
	x.update(func(p *Progress) { p.Extracted++ })
	if rep != nil {
		rep.Extracted++
	}
	return x.store.Dequeue(ctx, it.Ino)
}

// fail records an extraction failure and drops the file from the queue.
func (x *Indexer) fail(ctx context.Context, n meta.Node, p, kind string, cause error, rep *ReconcileReport) error {
	d := Document{
		Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version, Path: p, Ino: n.Ino,
		Kind: kind, Size: n.Size, MTimeNS: n.MTime.UnixNano(),
	}
	if err := x.dropReplaced(ctx, p, n.RemoteID); err != nil {
		return err
	}
	if err := x.store.MarkFailed(ctx, d, cause); err != nil {
		return err
	}
	x.update(func(p *Progress) { p.Failed++ })
	if rep != nil {
		rep.Failed++
	}
	return x.store.Dequeue(ctx, n.Ino)
}

// dropReplaced removes a document that sits at p under another remote id:
// the file was replaced, or a local write was published and took its
// remote identity. A path holds one file, so the index holds one document
// for it.
func (x *Indexer) dropReplaced(ctx context.Context, p, remoteID string) error {
	old, ok, err := x.store.DocumentByPath(ctx, p)
	if err != nil || !ok || old.RemoteID == remoteID {
		return err
	}
	return x.store.DeleteDocument(ctx, old.ID)
}

// safeExtract runs the extractor and turns a panic into a failure of that
// one document. The extractors guard against hostile input themselves; this
// is the last line, so a file can at worst fail its own indexing.
func safeExtract(ctx context.Context, kind textract.Kind, data []byte, opt textract.Options) (doc textract.Doc, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = textract.Doc{}, fmt.Errorf("textract: %s extractor panicked: %v", kind, r)
		}
	}()
	return textract.Extract(ctx, kind, bytes.NewReader(data), int64(len(data)), opt)
}

// readFile returns the bytes of the file behind n. A file the cache holds
// completely is read from the cache and costs the remote nothing; pinnedOnly
// files are never fetched. Otherwise the file is fetched through the FS in
// pieces, charged against the budget unless it is a local write that has
// not been uploaded yet.
func (x *Indexer) readFile(ctx context.Context, n meta.Node, p string, pinnedOnly bool) ([]byte, error) {
	key := fileKey(n)
	if x.fs.Cache().Complete(key) {
		if data, ok := x.readCached(key, n.Size); ok {
			return data, nil
		}
	}
	if pinnedOnly {
		return nil, errNotCached
	}
	if !vfs.IsLocalOnly(n.RemoteID) {
		if ok, resumeAt := x.budget.Take(n.Size, x.unofficial(n.Remote)); !ok {
			x.pause(PausedBudget, resumeAt)
			return nil, errPaused
		}
	}
	data := make([]byte, 0, n.Size)
	for off := int64(0); off < n.Size; {
		piece, err := x.fs.ReadFileRange(ctx, p, off, min(readPiece, n.Size-off))
		if err != nil {
			return nil, err
		}
		if len(piece) == 0 {
			break
		}
		data = append(data, piece...)
		off += int64(len(piece))
	}
	if !vfs.IsLocalOnly(n.RemoteID) {
		x.fetched.Add(int64(len(data)))
	}
	return data, nil
}

// readCached assembles a file from its cached blocks. It reports false when
// a block went missing meanwhile (eviction), in which case the caller falls
// back to the FS.
func (x *Indexer) readCached(key cache.FileKey, size int64) ([]byte, bool) {
	c := x.fs.Cache()
	data := make([]byte, 0, size)
	for idx := int64(0); idx < c.BlockCount(size); idx++ {
		block, ok := c.Get(key, idx)
		if !ok {
			return nil, false
		}
		data = append(data, block...)
	}
	if int64(len(data)) != size {
		return nil, false
	}
	return data, true
}

// String renders a report for logs.
func (r ReconcileReport) String() string {
	return fmt.Sprintf("walked %d queued %d extracted %d skipped %d failed %d deleted %d renamed %d",
		r.Walked, r.Queued, r.Extracted, r.Skipped, r.Failed, r.Deleted, r.Renamed)
}
