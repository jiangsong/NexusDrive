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
	// The directory sync is part of the durability promise, not best effort.
	// If it fails, no row in this batch may be acknowledged as committed.
	if err := c.syncObjects(); err != nil {
		for _, r := range batch {
			r.done <- err
		}
		return
	}
	err := c.j.tx(context.Background(), func(tx *sql.Tx) error {
		for _, r := range batch {
			if err := c.j.insertUploadTx(tx, r.u); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && len(batch) > 1 {
		// One bad row must not sink the others: retry them one at a time so
		// each caller gets its own verdict.
		for _, r := range batch {
			r.done <- c.j.tx(context.Background(), func(tx *sql.Tx) error {
				return c.j.insertUploadTx(tx, r.u)
			})
		}
		return
	}
	c.mu.Lock()
	c.batches++
	c.rows += int64(len(batch))
	c.mu.Unlock()
	for _, r := range batch {
		r.done <- err
	}
}

func (c *committer) syncObjects() error {
	if c.j.durability == DurabilityCrash {
		return nil
	}
	d, err := os.Open(c.j.ObjectsDir())
	if err != nil {
		return fmt.Errorf("journal: open objects for durability: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("journal: sync objects: %w", err)
	}
	return nil
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
	if u.IsMkdir() && (u.BlobPath != "" || u.Size != 0) {
		return errors.New("journal: a directory creation carries no bytes")
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
		   meta_identity, mount_prefix, mount_root_id, account_binding, kind)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   remote=excluded.remote, remote_parent_id=excluded.remote_parent_id, name=excluded.name,
		   blob_path=excluded.blob_path, size=excluded.size, hashes=excluded.hashes,
		   state=excluded.state, expected_version=excluded.expected_version, ino=excluded.ino,
		   needs_publish=excluded.needs_publish, meta_identity=excluded.meta_identity,
		   mount_prefix=excluded.mount_prefix, mount_root_id=excluded.mount_root_id,
		   account_binding=excluded.account_binding, kind=excluded.kind`)
	if err != nil {
		return fmt.Errorf("journal: prepare: %w", err)
	}
	_, err = tx.Stmt(st).Exec(
		u.ID, u.Remote, u.RemoteParentID, u.Name, u.BlobPath, u.Size, string(hashes),
		string(u.State), u.Attempt, unixMilli(u.NextRetryAt), u.LastError, u.ExpectedVersion,
		string(session), u.Ino, u.CreatedAt.Unix(), u.NeedsPublish,
		u.MetaIdentity, u.MountPrefix, u.MountRootID, u.AccountBinding, string(u.Kind))
	if err != nil {
		return fmt.Errorf("journal: commit: %w", err)
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
