package journal

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"cloudfs/internal/provider"
	"github.com/google/uuid"
)

const copySchema = `
CREATE TABLE IF NOT EXISTS copy_jobs (
 id TEXT PRIMARY KEY,
 spec TEXT NOT NULL,
 want TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('preparing','ready','submitted','failed','cancelled')),

 revision INTEGER NOT NULL DEFAULT 0,
 checkpoint INTEGER NOT NULL DEFAULT 0,
 crc32c TEXT NOT NULL DEFAULT '00000000',
 target_remote TEXT NOT NULL,
 target_parent TEXT NOT NULL,
 target_name TEXT NOT NULL,
 last_error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS copy_destination ON copy_jobs(target_remote,target_parent,target_name)
 WHERE state IN ('preparing','ready');
`

type CopyState string

const (
	CopyPreparing CopyState = "preparing"
	CopyReady     CopyState = "ready"
	CopySubmitted CopyState = "submitted"
	CopyFailed    CopyState = "failed"
	CopyCancelled CopyState = "cancelled"
	CopyPurging   CopyState = "purging" // projected from the durable cleanup table
)

var ErrCopyBusy = errors.New("journal: copy is already open or its destination is reserved")
var ErrCopyCorrupt = errors.New("journal: copy checkpoint content is missing or corrupt")
var ErrCopyState = errors.New("journal: copy state changed or does not permit this operation")
var ErrCopyCancelled = errors.New("journal: copy preparation was cancelled")
var ErrCopyReferenced = errors.New("journal: an upload still references this copy or its payload")

// CopySpec records identities, never signed download URLs or credentials.
// VFS must revalidate source version and mount bindings before resuming IO.
type CopySpec struct {
	MetaIdentity         string
	TargetParentIno      uint64
	SourceMount          string
	SourceRootID         string
	SourceAccountBinding string
	TargetMount          string
	TargetRootID         string
	TargetAccountBinding string
	SourcePath           string
	SourceRemote         string
	SourceID             string
	SourceVersion        string
	Size                 int64
	TargetPath           string
	TargetRemote         string
	TargetParentID       string
	Mode                 string
}

// PayloadPath names the job-owned immutable prepared file. Callers may link
// it into a cache only when State is CopyReady; they must never write to it.
func (c *CopyStaging) PayloadPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.job.State != CopyReady {
		return "", errors.New("journal: copy payload is not ready")
	}
	return c.staging.Path, nil
}

func (s CopySpec) validate() error {
	for _, p := range []string{s.SourcePath, s.TargetPath} {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > 4096 || strings.ContainsAny(p, "\x00\\") {
			return errors.New("journal: copy requires canonical absolute paths")
		}
	}
	if s.TargetPath == "/" || s.Size < 0 || s.SourceRemote == "" || s.SourceID == "" || s.TargetRemote == "" || s.TargetParentID == "" {
		return errors.New("journal: incomplete copy identities")
	}
	if s.Mode != "writeback" && s.Mode != "strict" {
		return errors.New("journal: copy requires a writable consistency mode")
	}
	if (s.SourceAccountBinding == "") != (s.TargetAccountBinding == "") {
		return errors.New("journal: incomplete copy account binding")
	}
	if s.SourceAccountBinding != "" &&
		(s.MetaIdentity == "" || s.SourceMount == "" || s.SourceRootID == "" || s.TargetMount == "" || s.TargetRootID == "") {
		return errors.New("journal: incomplete copy account binding")
	}
	for _, v := range []string{s.MetaIdentity, s.SourceMount, s.SourceRootID, s.SourceAccountBinding,
		s.TargetMount, s.TargetRootID, s.TargetAccountBinding, s.SourceRemote, s.SourceID,
		s.SourceVersion, s.TargetRemote, s.TargetParentID} {
		if len(v) > 8192 || strings.ContainsRune(v, 0) {
			return errors.New("journal: invalid copy identity")
		}
	}
	return nil
}

type CopyJob struct {
	ID         string
	Spec       CopySpec
	Want       []provider.HashType
	State      CopyState
	Checkpoint int64
	CRC32C     string
	LastError  string
	Revision   int64
}

// CopyStaging owns one preparation handle. Its bytes are private until Submit
// transfers ownership atomically to an upload. Close preserves the checkpoint;
// it never means cancellation and never commits an upload by itself.
type CopyStaging struct {
	mu      sync.Mutex
	j       *Journal
	job     CopyJob
	staging *Staging
	closed  bool
}

func (j *Journal) copyPath(id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", errors.New("journal: invalid copy id")
	}
	return filepath.Abs(filepath.Join(j.dir, "copies", id+".part"))
}

func (j *Journal) claimCopy(id string) error {
	if !j.Owner() {
		return errors.New("journal: copy preparation requires storage ownership")
	}
	j.copyMu.Lock()
	defer j.copyMu.Unlock()
	if j.copyOpen == nil {
		j.copyOpen = map[string]bool{}
	}
	if j.copyOpen[id] {
		return ErrCopyBusy
	}
	j.copyOpen[id] = true
	return nil
}

func (j *Journal) releaseCopy(id string) {
	j.copyMu.Lock()
	defer j.copyMu.Unlock()
	delete(j.copyOpen, id)
}

func (j *Journal) BeginCopy(ctx context.Context, spec CopySpec, want []provider.HashType) (*CopyStaging, error) {
	if !j.Owner() {
		return nil, errors.New("journal: copy preparation requires storage ownership")
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	want = append([]provider.HashType(nil), withCRC(want)...)
	s, err := j.NewStaging(want)
	if err != nil {
		return nil, err
	}
	p, err := j.copyPath(s.ID)
	if err != nil {
		s.Discard()
		return nil, err
	}
	if err := os.Rename(s.Path, p); err != nil {
		s.Discard()
		return nil, err
	}
	s.Path = p
	if err := j.syncCopy(s); err != nil {
		s.Discard()
		return nil, err
	}
	if err := j.claimCopy(s.ID); err != nil {
		s.Discard()
		return nil, err
	}
	job := CopyJob{ID: s.ID, Spec: spec, Want: withCRC(want), State: CopyPreparing, CRC32C: "00000000"}
	err = j.insertCopy(ctx, job)
	if err != nil {
		j.releaseCopy(s.ID)
		s.Discard()
		return nil, err
	}
	return &CopyStaging{j: j, job: job, staging: s}, nil
}

// BeginCopyFromWhole snapshots immutable complete cache content by hard link.
// It publishes a READY intent only after the link and checksum are durable;
// a linked inode must never appear as a PREPARING file whose tail may be cut.
// The caller retains the immutable cache lease until this call returns.
func (j *Journal) BeginCopyFromWhole(ctx context.Context, spec CopySpec, src *os.File, want []provider.HashType) (*CopyStaging, error) {
	if !j.Owner() {
		return nil, errors.New("journal: copy preparation requires storage ownership")
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	want = append([]provider.HashType(nil), withCRC(want)...)
	s, err := j.LinkStaging(src, want)
	if err != nil {
		return nil, err
	}
	if s.Size() != spec.Size {
		s.Discard()
		return nil, errors.New("journal: complete copy source size differs")
	}
	id := NewID()
	p, err := j.copyPath(id)
	if err != nil {
		s.Discard()
		return nil, err
	}
	if err := os.Rename(s.Path, p); err != nil {
		s.Discard()
		return nil, err
	}
	s.ID, s.Path = id, p
	hashes, err := s.Hashes()
	if err != nil {
		s.Discard()
		return nil, err
	}
	if err := j.syncCopy(s); err != nil {
		s.Discard()
		return nil, err
	}
	if err := j.claimCopy(id); err != nil {
		s.Discard()
		return nil, err
	}
	job := CopyJob{ID: id, Spec: spec, Want: want, State: CopyReady, Checkpoint: spec.Size, CRC32C: hashes[provider.HashCRC32C]}
	if err := j.insertCopy(ctx, job); err != nil {
		j.releaseCopy(id)
		s.Discard()
		return nil, err
	}
	return &CopyStaging{j: j, job: job, staging: s}, nil
}

func (j *Journal) insertCopy(ctx context.Context, job CopyJob) error {
	specJSON, _ := json.Marshal(job.Spec)
	wantJSON, _ := json.Marshal(job.Want)
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO copy_jobs(id,spec,want,state,checkpoint,crc32c,target_remote,target_parent,target_name) VALUES (?,?,?,?,?,?,?,?,?)`, job.ID, string(specJSON), string(wantJSON), string(job.State), job.Checkpoint, job.CRC32C, job.Spec.TargetRemote, job.Spec.TargetParentID, path.Base(job.Spec.TargetPath))
		if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrCopyBusy
		}
		return err
	})
}

func scanCopy(row interface{ Scan(...any) error }) (CopyJob, error) {
	var c CopyJob
	var spec, want string
	err := row.Scan(&c.ID, &spec, &want, &c.State, &c.Checkpoint, &c.CRC32C, &c.LastError, &c.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(spec), &c.Spec); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(want), &c.Want); err != nil {
		return c, err
	}
	if err := c.Spec.validate(); err != nil {
		return c, err
	}
	if c.Checkpoint < 0 || c.Checkpoint > c.Spec.Size {
		return c, ErrCopyCorrupt
	}
	if (c.State == CopyReady || c.State == CopySubmitted) && c.Checkpoint != c.Spec.Size {
		return c, ErrCopyCorrupt
	}
	checksum, err := hex.DecodeString(c.CRC32C)
	if err != nil || len(checksum) != 4 || strings.ToLower(c.CRC32C) != c.CRC32C {
		return c, ErrCopyCorrupt
	}
	return c, nil
}

const copyCols = `id,spec,want,state,checkpoint,crc32c,last_error`

func (j *Journal) copyColumns() string {
	if j.legacyCopyAdmin {
		return copyCols + `,0 AS revision`
	}
	return copyCols + `,revision`
}

func (j *Journal) GetCopy(ctx context.Context, id string) (CopyJob, error) {
	if _, err := j.copyPath(id); err != nil {
		return CopyJob{}, err
	}
	if exists, err := j.hasCopyTable(ctx); err != nil {
		return CopyJob{}, err
	} else if !exists {
		return CopyJob{}, ErrNotFound
	}
	query, err := j.copyRowsSQL(ctx)
	if err != nil {
		return CopyJob{}, err
	}
	return scanCopy(j.db.QueryRowContext(ctx, `SELECT * FROM (`+query+`) WHERE id=?`, id))
}

// CopyJobs includes failed and submitted intents for inspection. No payload
// path or secret-bearing provider session is part of this record.
func (j *Journal) CopyJobs(ctx context.Context) ([]CopyJob, error) {
	if exists, err := j.hasCopyTable(ctx); err != nil {
		return nil, err
	} else if !exists {
		return nil, nil
	}
	query, err := j.copyRowsSQL(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := j.db.QueryContext(ctx, `SELECT * FROM (`+query+`) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CopyJob
	for rows.Next() {
		c, err := scanCopy(rows)
		if err != nil {
			return nil, err
		}
		if _, err := j.copyPath(c.ID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (j *Journal) hasCopyTable(ctx context.Context) (bool, error) {
	var n int
	err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='copy_jobs'`).Scan(&n)
	return n != 0, err
}

func (j *Journal) syncCopy(s *Staging) error {
	if j.Durability() == DurabilityCrash {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(s.Path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	parent, err := os.Open(j.dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (c *CopyStaging) Job() CopyJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.job
	out.Want = append([]provider.HashType(nil), out.Want...)
	return out
}

func (c *CopyStaging) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.job.State != CopyPreparing {
		return 0, errors.New("journal: copy is not writable")
	}
	off := c.staging.Size()
	if int64(len(p)) > c.job.Spec.Size-off {
		return 0, errors.New("journal: copy exceeds source size")
	}
	return c.staging.WriteAt(p, off)
}

func (c *CopyStaging) Checkpoint(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoint(ctx)
}

func (c *CopyStaging) checkpoint(ctx context.Context) error {
	if c.closed || c.job.State != CopyPreparing {
		return errors.New("journal: copy is not preparing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hashes, err := c.staging.Hashes()
	if err != nil {
		return err
	}
	if err := c.j.syncCopy(c.staging); err != nil {
		return err
	}
	off := c.staging.Size()
	state := CopyPreparing
	if off == c.job.Spec.Size {
		state = CopyReady
	}
	err = c.j.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE copy_jobs SET checkpoint=?,crc32c=?,state=? WHERE id=? AND state='preparing'`, off, hashes[provider.HashCRC32C], string(state), c.job.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrCopyState
		}
		return err
	})
	if err != nil {
		return err
	}
	c.job.Checkpoint, c.job.CRC32C, c.job.State = off, hashes[provider.HashCRC32C], state
	return nil
}

func (c *CopyStaging) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	defer c.j.releaseCopy(c.job.ID)
	return c.staging.f.Close()
}

// ResumeCopy validates a durable prefix before discarding any uncheckpointed
// tail. Streaming hash state is rebuilt once, so later checkpoints stay O(1).
func (j *Journal) ResumeCopy(ctx context.Context, id string) (_ *CopyStaging, err error) {
	return j.openCopy(ctx, id, copyResume)
}

type copyOpenMode int

const (
	copyResume copyOpenMode = iota
	copyRetry
	copyInspect
)

func (j *Journal) openCopy(ctx context.Context, id string, mode copyOpenMode) (_ *CopyStaging, err error) {
	if err := j.claimCopy(id); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			j.releaseCopy(id)
		}
	}()
	job, err := j.GetCopy(ctx, id)
	if err != nil {
		return nil, err
	}
	if mode == copyRetry {
		if job.State != CopyFailed && job.State != CopyCancelled {
			return nil, ErrCopyState
		}
		job.State = CopyPreparing
		if job.Checkpoint == job.Spec.Size {
			job.State = CopyReady
		}
	} else if mode == copyInspect {
		if job.Checkpoint != job.Spec.Size || (job.State != CopyReady && job.State != CopyFailed && job.State != CopyCancelled) {
			return nil, ErrCopyState
		}
		// Only the returned private reader sees Ready; no durable state is
		// changed and no mutable preparation handle escapes this operation.
		job.State = CopyReady
	} else if job.State == CopyCancelled {
		return nil, ErrCopyCancelled
	} else if job.State != CopyPreparing && job.State != CopyReady {
		return nil, ErrCopyState
	}
	p, err := j.copyPath(id)
	if err != nil {
		return nil, err
	}
	named, err := os.Lstat(p)
	if err != nil || !named.Mode().IsRegular() {
		return nil, ErrCopyCorrupt
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !os.SameFile(named, info) || info.Size() < job.Checkpoint {
		return nil, ErrCopyCorrupt
	}
	s := newStagingState(f, id, p, job.Want, j.reserveSpace)
	buf := make([]byte, 1<<20)
	for off := int64(0); off < job.Checkpoint; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := buf[:min(int64(len(buf)), job.Checkpoint-off)]
		if _, err := io.ReadFull(io.NewSectionReader(f, off, int64(len(chunk))), chunk); err != nil {
			return nil, ErrCopyCorrupt
		}
		for _, h := range s.full {
			h.Write(chunk)
		}
		for ht, h := range s.prefix {
			if off < s.prefixCaps[ht] {
				h.Write(chunk[:min(int64(len(chunk)), s.prefixCaps[ht]-off)])
			}
		}
		off += int64(len(chunk))
	}
	got := hex.EncodeToString(s.full[provider.HashCRC32C].Sum(nil))
	if got != job.CRC32C {
		return nil, ErrCopyCorrupt
	}
	if info.Size() != job.Checkpoint {
		if job.State == CopyReady {
			return nil, ErrCopyCorrupt
		}
		if err := f.Truncate(job.Checkpoint); err != nil {
			return nil, err
		}
	}
	s.size, s.pos = job.Checkpoint, job.Checkpoint
	s.immutable = job.State == CopyReady
	if mode == copyRetry {
		if j.copyRetryFault != nil {
			j.copyRetryFault()
		}
		err = j.tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.Exec(`UPDATE copy_jobs SET state=?,last_error='',revision=revision+1 WHERE id=? AND state IN ('failed','cancelled') AND revision=?`, string(job.State), id, job.Revision)
			if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return ErrCopyBusy
			}
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err == nil && n != 1 {
				return ErrCopyState
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		job.Revision++
	}
	return &CopyStaging{j: j, job: job, staging: s}, nil
}

// Submit transfers a complete payload to exactly one upload in the same
// SQLite transaction that records submission. Reopening cannot enqueue it twice.
// The VFS supplies the already-reserved inode and publication barrier policy.
func (c *CopyStaging) Submit(ctx context.Context, u Upload) (Upload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.job.State != CopyReady {
		return Upload{}, errors.New("journal: copy is not ready")
	}
	if u.Remote != c.job.Spec.TargetRemote || u.RemoteParentID != c.job.Spec.TargetParentID || u.Name != path.Base(c.job.Spec.TargetPath) {
		return Upload{}, errors.New("journal: copy upload target changed")
	}
	hashes, err := c.staging.Hashes()
	if err != nil {
		return Upload{}, err
	}
	if err := c.j.syncCopy(c.staging); err != nil {
		return Upload{}, err
	}
	u.ID, u.BlobPath, u.Size, u.Hashes = c.job.ID, c.staging.Path, c.job.Spec.Size, hashes
	u.State, u.CreatedAt = StatePending, c.j.now()
	u.Attempt, u.LastError, u.Session, u.Tombstone = 0, "", nil, false
	err = c.j.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM uploads WHERE id=?`, u.ID).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return errors.New("journal: copy upload id already exists")
		}
		res, err := tx.Exec(`UPDATE copy_jobs SET state='submitted' WHERE id=? AND state='ready'`, c.job.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCopyState
		}
		return c.j.insertUploadTx(tx, u)
	})
	if err != nil {
		return Upload{}, err
	}
	c.job.State = CopySubmitted
	return u, nil
}

// FailCopy preserves the checkpoint for inspection but prevents automatic IO.
// It cannot change a live handle or undo a submitted upload.
func (j *Journal) FailCopy(ctx context.Context, id, reason string) error {
	if err := j.claimCopy(id); err != nil {
		return err
	}
	defer j.releaseCopy(id)
	return j.tx(ctx, func(tx *sql.Tx) error {
		var state CopyState
		if err := tx.QueryRow(`SELECT state FROM copy_jobs WHERE id=?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state == CopyCancelled {
			return nil
		} // Never undo explicit cancellation.
		res, err := tx.Exec(`UPDATE copy_jobs SET state='failed',last_error=? WHERE id=? AND state IN ('preparing','ready','failed')`, reason, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrCopyState
		}
		return err
	})
}

// RetryCopy validates the persisted prefix and requeues a failed/cancelled job.
// It never restarts active work or changes the source/target identity. Cancel
// increments Revision even when already cancelled, so a concurrent retry based
// on an older snapshot cannot overwrite a newer cancellation request.
func (j *Journal) RetryCopy(ctx context.Context, id string) error {
	c, err := j.openCopy(ctx, id, copyRetry)
	if err != nil {
		return err
	}
	return c.Close()
}

// WithReadyCopy validates and leases a complete retained payload without
// retrying its job. Failed/cancelled full local versions need this at startup
// to restore their cache links; partial payloads are never published.
func (j *Journal) WithReadyCopy(ctx context.Context, id string, install func(string, int64) error) error {
	c, err := j.openCopy(ctx, id, copyInspect)
	if err != nil {
		return err
	}
	defer c.Close()
	p, err := c.PayloadPath()
	if err != nil {
		return err
	}
	return install(p, c.job.Spec.Size)
}

// CancelCopy stops preparation durably, including an open preparation handle.
// Checkpoint and Submit both require active states in their SQL transaction.
// Existing payload and any full local version remain owned, not discarded.
func (j *Journal) CancelCopy(ctx context.Context, id string) error {
	if !j.Owner() {
		return errors.New("journal: copy cancellation requires storage ownership")
	}
	if _, err := j.copyPath(id); err != nil {
		return err
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		var state CopyState
		if err := tx.QueryRow(`SELECT state FROM copy_jobs WHERE id=?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state == CopySubmitted {
			return ErrCopyState
		}
		_, err := tx.Exec(`UPDATE copy_jobs SET state='cancelled',last_error='',revision=revision+1 WHERE id=?`, id)
		return err
	})
}
