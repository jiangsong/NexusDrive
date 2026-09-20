package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Group commit.
//
// Every close() of a written file used to pay its own SQLite transaction with
// synchronous=FULL and its own fsync of the objects directory: three to four
// disk flushes per file, which held a batch of small files to ~90 creates per
// second while the disk could do thousands. Commits now queue for a few
// milliseconds and land in one transaction with one directory fsync. The
// promise to each caller is unchanged — close() returns only once its row is
// durable — it is just kept for several callers at once.

const (
	// groupWait is how long a batch stays open for more arrivals once it
	// has at least two: a lone commit must not pay it, or a single-threaded
	// writer would lose the window on every file. The window only pays off
	// when several writers are closing at once, and that is detectable from
	// the batch itself.
	groupWait = 2 * time.Millisecond
	groupMax  = 64
)

type commitReq struct {
	u    Upload
	done chan error
}

type committer struct {
	j     *Journal
	queue chan commitReq
	stop  chan struct{}
	wg    sync.WaitGroup
	// batches and rows count what the committer did, for tests and status.
	mu      sync.Mutex
	batches int64
	rows    int64
	waits   int64
}

func newCommitter(j *Journal) *committer {
	c := &committer{j: j, queue: make(chan commitReq, groupMax*4), stop: make(chan struct{})}
	c.wg.Add(1)
	go c.run()
	return c
}

func (c *committer) close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	c.wg.Wait()
}

// submit queues one row and waits for the batch that carries it.
func (c *committer) submit(ctx context.Context, u Upload) error {
	req := commitReq{u: u, done: make(chan error, 1)}
	select {
	case c.queue <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stop:
		return errors.New("journal: closed")
	}
	select {
	case err := <-req.done:
		return err
	case <-c.stop:
		// The committer drains the queue before exiting, so a queued request
		// still gets its answer; only an unqueued one is refused above.
		return <-req.done
	}
}

func (c *committer) run() {
	defer c.wg.Done()
	for {
		var batch []commitReq
		select {
		case r := <-c.queue:
			batch = append(batch, r)
		case <-c.stop:
			c.drain()
			return
		}
		// Take whatever is already waiting without pausing.
	drainNow:
		for len(batch) < groupMax {
			select {
			case r := <-c.queue:
				batch = append(batch, r)
			default:
				break drainNow
			}
		}
		// Only when writers are evidently concurrent is a short wait for
		// more of them worth the latency.
		if len(batch) > 1 && len(batch) < groupMax {
			c.mu.Lock()
			c.waits++
			c.mu.Unlock()
			timer := time.NewTimer(groupWait)
		gather:
			for len(batch) < groupMax {
				select {
				case r := <-c.queue:
					batch = append(batch, r)
				case <-timer.C:
					break gather
				case <-c.stop:
					break gather
				}
			}
			timer.Stop()
		}
		c.commitBatch(batch)
	}
}

// drain commits whatever is queued at shutdown so no caller is left hanging.
func (c *committer) drain() {
	for {
		var batch []commitReq
	collect:
		for len(batch) < groupMax {
			select {
			case r := <-c.queue:
				batch = append(batch, r)
			default:
				break collect
			}
		}
		if len(batch) == 0 {
			return
		}
		c.commitBatch(batch)
	}
}

func (c *committer) commitBatch(batch []commitReq) {
	// Only a batch that references a staged blob needs the objects directory
	// on the medium. A mkdir or a delete row carries no BlobPath — insertUploadTx
	// refuses one — so there is no new entry in objects/ for a row to point at,
	// and the row's own durability comes from the synchronous=FULL insert. The
	// fsync skipped here would only have made durable a directory entry nothing
	// names. It is not a small saving: on darwin os.File.Sync is F_FULLFSYNC,
	// measured at 4.07 ms on this project's development machine (APFS on
	// internal NVMe) against 74 us for a plain fsync, so every directory a
	// recursive copy creates used to pay one for nothing. A `cp -r` of a source
	// tree with 389 directories spent 1.58 s here. Measured from inside this
	// package on the same machine, a blob-less commit as it used to be — one
	// synchronous=FULL insert plus this fsync — cost 4.08-4.82 ms per row, of
	// which the insert was 70-90 us: almost all of what a queued mkdir cost was
	// this line. One machine's numbers, but the ratio is the point. F_FULLFSYNC
	// flushes the device write cache and an ordinary fsync does not, which is
	// also why the saving is smaller on Linux, where fsync(2) already does.
	//
	// Eliding it holds in the other direction too: a directory fsync flushes
	// every entry pending in it, so a rename that a blob-less batch skips over
	// is still made durable by the next batch that does carry a blob — which
	// runs before any row naming that object exists.
	//
	// The sync is part of the durability promise, not best effort. If it fails,
	// no row in this batch may be acknowledged as committed.
	needsObjects := referencesBlob(batch)
	// Where the flush goes is what separates the two durable levels.
	//
	// power flushes here, before the row exists. That makes the rename durable
	// and leaves the row itself covered by nothing: SQLite runs a plain fsync
	// unless PRAGMA fullfsync is set and nothing here sets it, so on darwin
	// the row reaches the drive's cache and no further. See TODO.md T-62; the
	// ordering is kept as it is because changing what power means is a
	// separate decision from adding a level that does it differently.
	//
	// barrier flushes after the insert instead, where one flush covers the
	// staging bytes, the rename and the row together — fewer flushes and a
	// stronger promise, which is why it is the default.
	if needsObjects && c.j.durability == DurabilityPower {
		if err := c.j.flushObjects(); err != nil {
			for _, r := range batch {
				r.done <- err
			}
			return
		}
	}
	results := make([]error, len(batch))
	txErr := c.j.tx(context.Background(), func(tx *sql.Tx) error {
		for _, r := range batch {
			if err := c.j.insertUploadTx(tx, r.u); err != nil {
				return err
			}
		}
		return nil
	})
	retried := false
	if txErr != nil {
		if len(batch) == 1 {
			results[0] = txErr
		} else {
			// One bad row must not sink the others: retry them one at a time so
			// each caller gets its own verdict. Only a transaction that failed
			// may be retried this way. Nothing was written, so re-inserting is
			// free of consequence — which is not true once the transaction has
			// committed, see below.
			retried = true
			for i, r := range batch {
				results[i] = c.j.tx(context.Background(), func(tx *sql.Tx) error {
					return c.j.insertUploadTx(tx, r.u)
				})
			}
		}
	}
	if needsObjects && c.j.durability == DurabilityBarrier && anyCommitted(results) {
		// A failure here leaves the rows committed but not on the medium. The
		// callers whose rows went in are told, because close(2) must not report
		// a durability it did not get; the rows stay and the files upload
		// anyway, so the error is spurious rather than lossy — the safe
		// direction of the two.
		//
		// What must not happen is a retry of the insert. The rows are already
		// committed, and insertUploadTx is an upsert: re-running it would reset
		// state to pending under a worker that has since claimed the row —
		// uploading the file twice — or resurrect a row that has completed and
		// been trimmed. It would also make every caller's verdict nil, which is
		// the opposite of what the paragraph above promises.
		if err := c.j.flushObjects(); err != nil {
			for i := range results {
				if results[i] == nil {
					results[i] = err
				}
			}
		}
	}
	if !retried {
		c.mu.Lock()
		c.batches++
		c.rows += int64(len(batch))
		c.mu.Unlock()
	}
	for i, r := range batch {
		r.done <- results[i]
	}
}

// anyCommitted reports whether any row of the batch reached the database, and
// so whether the objects directory still owes it a flush.
func anyCommitted(results []error) bool {
	for _, err := range results {
		if err == nil {
			return true
		}
	}
	return false
}

// referencesBlob reports whether any row in the batch names a staged object.
func referencesBlob(batch []commitReq) bool {
	for _, r := range batch {
		if r.u.BlobPath != "" {
			return true
		}
	}
	return false
}

// insertUploadTx writes one row.
func (j *Journal) insertUploadTx(tx *sql.Tx, u Upload) error {
	if err := guardNotCancelled(tx, u.ID); err != nil {
		return err
	}
	if u.State == StatePurging {
		return ErrUploadCleanupState // Only Begin may pair this state with an intent.
	}
	if u.Kind == "" {
		u.Kind = KindFile
	}
	if (u.IsMkdir() || u.IsDelete()) && (u.BlobPath != "" || u.Size != 0) {
		return errors.New("journal: a directory creation or a delete carries no bytes")
	}
	if u.IsDelete() && (u.RemoteID == "" || u.Ino != 0) {
		return errors.New("journal: a delete names a backend id and no node")
	}
	if u.BlobPath != "" {
		if err := j.checkCopyUploadOwner(tx, u.BlobPath); err != nil {
			return err
		}
		if err := j.checkDiscardedPayload(tx, u.BlobPath); err != nil {
			return err
		}
	}
	now := j.now()
	if u.ID == "" {
		return errors.New("journal: upload needs an id")
	}
	if u.State == "" {
		u.State = StatePending
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	bindings := []string{u.MetaIdentity, u.MountPrefix, u.MountRootID, u.AccountBinding}
	bound := 0
	for _, value := range bindings {
		if value != "" {
			bound++
		}
		if len(value) > 8192 || strings.ContainsRune(value, 0) {
			return errors.New("journal: invalid upload binding")
		}
	}
	if bound != 0 && bound != len(bindings) {
		return errors.New("journal: incomplete upload binding")
	}
	hashes, err := json.Marshal(u.Hashes)
	if err != nil {
		return fmt.Errorf("journal: marshal hashes: %w", err)
	}
	session, err := json.Marshal(u.Session)
	if err != nil {
		return fmt.Errorf("journal: marshal session: %w", err)
	}
	st, err := j.prep(context.Background(), `INSERT INTO uploads (id, remote, remote_parent_id, name, blob_path, size, hashes,
		   state, attempt, next_retry_at, last_error, expected_version, session, ino, created_at, needs_publish,
		   meta_identity, mount_prefix, mount_root_id, account_binding, kind, remote_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   remote=excluded.remote, remote_parent_id=excluded.remote_parent_id, name=excluded.name,
		   blob_path=excluded.blob_path, size=excluded.size, hashes=excluded.hashes,
		   state=excluded.state, expected_version=excluded.expected_version, ino=excluded.ino,
		   needs_publish=excluded.needs_publish, meta_identity=excluded.meta_identity,
		   mount_prefix=excluded.mount_prefix, mount_root_id=excluded.mount_root_id,
		   account_binding=excluded.account_binding, kind=excluded.kind, remote_id=excluded.remote_id`)
	if err != nil {
		return fmt.Errorf("journal: prepare: %w", err)
	}
	_, err = tx.Stmt(st).Exec(
		u.ID, u.Remote, u.RemoteParentID, u.Name, u.BlobPath, u.Size, string(hashes),
		string(u.State), u.Attempt, unixMilli(u.NextRetryAt), u.LastError, u.ExpectedVersion,
		string(session), u.Ino, u.CreatedAt.Unix(), u.NeedsPublish,
		u.MetaIdentity, u.MountPrefix, u.MountRootID, u.AccountBinding, string(u.Kind), u.RemoteID)
	if err != nil {
		return fmt.Errorf("journal: commit: %w", err)
	}
	return nil
}

// DurabilityStats reports what the write path has spent on durability:
// fsyncs of staging files, fsyncs of the objects directory, and write
// transactions. A commit's cost is a count of disk flushes, not a duration,
// so this is what a regression test asserts against. The first two are the
// expensive kind — a real device flush on darwin — and the third is not;
// they are reported apart because on this machine they differ by 50x.
func (j *Journal) DurabilityStats() (stagingSyncs, objectSyncs, writeTxs int64) {
	return j.stagingSyncs.Load(), j.objectSyncs.Load(), j.writeTxs.Load()
}

// DeviceFlushes counts how many of those syncs asked the drive to empty its
// write cache. That is the number close(2)'s cost is made of: one is 4.07 ms
// on this project's development machine against 74 us for handing the device
// the bytes, so a level is fast or slow almost entirely by this count.
func (j *Journal) DeviceFlushes() int64 { return j.deviceFlushes.Load() }

// flushObjects makes the objects directory's entries durable on the medium.
// Separated from syncObjects so the counters can tell a real device flush
// from the cheaper kinds.
func (j *Journal) flushObjects() error {
	if j.durability == DurabilityCrash {
		return nil
	}
	if j.flushFault != nil {
		if err := j.flushFault(); err != nil {
			return err
		}
	}
	d, err := os.Open(j.ObjectsDir())
	if err != nil {
		return fmt.Errorf("journal: open objects for durability: %w", err)
	}
	defer d.Close()
	j.objectSyncs.Add(1)
	j.deviceFlushes.Add(1)
	if j.onDeviceFlush != nil {
		j.onDeviceFlush()
	}
	if err := flushDevice(d); err != nil {
		return fmt.Errorf("journal: sync objects: %w", err)
	}
	return nil
}

// CommitStats reports how many batches and rows the group committer wrote.
func (j *Journal) CommitStats() (batches, rows int64) {
	if j.committer == nil {
		return 0, 0
	}
	j.committer.mu.Lock()
	defer j.committer.mu.Unlock()
	return j.committer.batches, j.committer.rows
}
