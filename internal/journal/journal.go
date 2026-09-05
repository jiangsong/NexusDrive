// Package journal is the durable write-ahead log for uploads. A close()
// returns once the staged blob is fsynced and its row committed here, so an
// interrupted upload always resumes: nothing is acknowledged to the
// application that is not already on local disk (docs/DESIGN.md §4.5).
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"

	_ "modernc.org/sqlite"
)

// State is an upload's lifecycle state.
type State string

const (
	StatePending   State = "pending"
	StateUploading State = "uploading"
	StateDone      State = "done"
	StateDead      State = "dead"
)

// Upload is one queued write.
type Upload struct {
	ID string
	// StagingID transfers a live CommitStaging reservation to this durable
	// row. It is process-local, not persisted or exposed in JSON responses.
	StagingID      string `json:"-"`
	Remote         string
	RemoteParentID string
	Name           string
	BlobPath       string
	Size           int64
	Hashes         provider.Hashes
	State          State
	Attempt        int
	NextRetryAt    time.Time
	LastError      string
	// ExpectedVersion is the remote version observed at open time. A mismatch
	// at upload time means someone else changed the file: write a conflict copy.
	ExpectedVersion string
	// Session carries provider upload state (upload_id, part urls) so a
	// resumed upload continues instead of restarting.
	Session map[string]string
	// Ino links the upload back to the metadata node so the tree can be
	// updated when it completes.
	Ino       uint64
	CreatedAt time.Time
	// Tombstone marks an upload whose file was deleted locally while the
	// transfer was already in flight. It must finish — a half-written remote
	// file is worse than a whole one — and then be deleted from the backend
	// rather than announced as a success.
	Tombstone bool
	// NeedsPublish prevents workers from racing the VFS's local publication.
	// The durable blob exists, but metadata/cache may still describe the old
	// version. Startup recovery completes this step before allowing upload.
	NeedsPublish bool
	// These opaque local bindings fence queued bytes to the metadata database,
	// mount and credential generation that accepted the write. They contain no
	// provider secret or cloud account identifier.
	MetaIdentity   string
	MountPrefix    string
	MountRootID    string
	AccountBinding string
}

// Part is one uploaded chunk.
type Part struct {
	Index int
	ETag  string
	State string
}

// ErrNotFound is returned for unknown upload ids.
var ErrNotFound = errors.New("journal: upload not found")

// ErrInFlight is DropPending's answer for a row an uploader has already
// claimed: the caller must tombstone it instead.
var ErrInFlight = errors.New("journal: upload is in flight")

// ErrNotDead prevents an administrative retry from duplicating active or
// already completed work, or bypassing the backoff on a pending upload.
var ErrNotDead = errors.New("journal: upload is not dead-lettered")

// Journal is the SQLite-backed upload queue. It lives in its own database file
// so metadata corruption never costs unuploaded data.
type Journal struct {
	durability Durability
	// stmts caches prepared statements; the commit path runs its INSERT
	// for every closed file.
	stmts   sync.Map
	db      *sql.DB
	dir     string
	now     func() time.Time
	writeMu sync.Mutex
	// lock and owner record whether this process owns the queue. See
	// acquireOwnership.
	lock  *os.File
	owner bool
	// committer batches Commit calls into shared transactions.
	committer           *committer
	reserveSpace        func(string, int64) (func(), error)
	legacyPublication   bool // read-only inspection of pre-v4 databases
	legacyUploadBinding bool // read-only inspection of pre-v11 upload rows
	legacyCopyAdmin     bool // read-only inspection of pre-v6 copy records
	copyMu              sync.Mutex
	copyOpen            map[string]bool
	copyRetryFault      func()             // test boundary after prefix validation, before CAS
	dropSnapshotFault   func()             // test boundary after superseded IDs are selected
	uploadCleanupFault  func(string) error // after payload unlink / before final SQL
	// objectMu serializes staging renames and garbage collection. Reservations
	// bridge the gap before a staged content-addressed object has a journal row.
	// Lock order: objectMu then writeMu; never acquire objectMu inside tx.
	objectMu      sync.Mutex
	stagedObjects map[string]string // staging ID -> object path
	stagedRefs    map[string]int    // object path -> live staging owners
}

// Options configures Open.
type Options struct {
	// Dir holds journal.db plus the staging and objects directories.
	Dir string
	Now func() time.Time
	// Durability selects what a committed row promises; zero means power.
	Durability Durability
	// ReserveSpace admits a payload write on the filesystem holding dir.
	// Its release function runs after the syscall even on failure. Nil disables
	// application-level headroom checks (the filesystem still enforces ENOSPC).
	ReserveSpace func(dir string, bytes int64) (release func(), err error)
}

// Durability is the strength of the promise close(2) makes.
type Durability string

const (
	// DurabilityPower: the staging file, the objects directory and the WAL
	// are fsynced before close() returns; a power loss loses nothing that
	// was acknowledged.
	DurabilityPower Durability = "power"
	// DurabilityCrash: nothing is fsynced on the write path. The data sits
	// in the kernel's page cache, so a daemon crash or kill -9 loses
	// nothing; a power loss may lose the last seconds of writes, and
	// Recover reports each one it can prove torn as a dead letter.
	DurabilityCrash Durability = "crash"
)

// prep returns a cached prepared statement for q.
func (j *Journal) prep(ctx context.Context, q string) (*sql.Stmt, error) {
	if st, ok := j.stmts.Load(q); ok {
		return st.(*sql.Stmt), nil
	}
	st, err := j.db.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	if prev, loaded := j.stmts.LoadOrStore(q, st); loaded {
		st.Close()
		return prev.(*sql.Stmt), nil
	}
	return st, nil
}

// Durability reports the configured write durability.
func (j *Journal) Durability() Durability { return j.durability }

// Open creates or opens the journal.
func Open(opt Options) (*Journal, error) {
	if opt.Dir == "" {
		return nil, errors.New("journal: Dir is required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	for _, d := range []string{opt.Dir, filepath.Join(opt.Dir, "staging"), filepath.Join(opt.Dir, "objects"), filepath.Join(opt.Dir, "copies")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
	}
	if opt.Durability == "" {
		opt.Durability = DurabilityPower
	}
	// synchronous=FULL: an acknowledged close must survive a power loss.
	// In crash mode NORMAL is enough: WAL with NORMAL is consistent after a
	// crash and only the last transactions may be lost after a power cut.
	sync := "FULL"
	if opt.Durability == DurabilityCrash {
		sync = "NORMAL"
	}
	dsn := "file:" + filepath.Join(opt.Dir, "journal.db") +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(" + sync + ")&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	db.SetMaxOpenConns(4)
	lock, owner, err := acquireOwnership(opt.Dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	j := &Journal{durability: opt.Durability, db: db, dir: opt.Dir, now: opt.Now, lock: lock, owner: owner, reserveSpace: opt.ReserveSpace}
	if owner {
		err = j.migrate()
	} else {
		// Never rebuild a schema underneath a daemon owned by another build.
		var current int
		err = db.QueryRow(`PRAGMA user_version`).Scan(&current)
		if err == nil && current != journalSchemaVersion {
			err = fmt.Errorf("journal: schema v%d requires its owner for migration; connect to the running daemon", current)
		}
	}
	if err != nil {
		releaseOwnership(lock)
		db.Close()
		return nil, err
	}
	j.committer = newCommitter(j)
	return j, nil
}

// Owner reports whether this process holds the queue. Only the owner may run
// recovery; other processes can read the queue but must not rewrite it.
func (j *Journal) Owner() bool { return j.owner }

// SetSpaceReserver wires storage admission before any staging handles are
// created. Existing handles retain their original reserver.
func (j *Journal) SetSpaceReserver(reserve func(string, int64) (func(), error)) {
	j.reserveSpace = reserve
}

// OpenReadOnly inspects an existing queue without taking ownership, migrating,
// recovering, or starting a committer. It never creates an absent journal.
func OpenReadOnly(dir string) (*Journal, error) {
	p, err := filepath.Abs(filepath.Join(dir, "journal.db"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	return &Journal{db: db, dir: dir, now: time.Now, legacyPublication: version < 4, legacyUploadBinding: version < 11, legacyCopyAdmin: version < 6}, nil
}

// Close releases the database and the ownership lock.
func (j *Journal) Close() error {
	if j.committer != nil {
		j.committer.close()
	}
	releaseOwnership(j.lock)
	j.lock = nil
	return j.db.Close()
}

// StagingDir is where in-progress writes are buffered.
func (j *Journal) StagingDir() string { return filepath.Join(j.dir, "staging") }

// ObjectsDir holds committed blobs awaiting upload.
func (j *Journal) ObjectsDir() string { return filepath.Join(j.dir, "objects") }

const journalSchema = `
CREATE TABLE IF NOT EXISTS uploads (
  id               TEXT PRIMARY KEY,
  remote           TEXT NOT NULL,
  remote_parent_id TEXT NOT NULL,
  name             TEXT NOT NULL,
  blob_path        TEXT NOT NULL,
  size             INTEGER NOT NULL,
  hashes           TEXT NOT NULL DEFAULT '{}',
  state            TEXT NOT NULL,
  attempt          INTEGER NOT NULL DEFAULT 0,
  next_retry_at    INTEGER NOT NULL DEFAULT 0,
  last_error       TEXT NOT NULL DEFAULT '',
  expected_version TEXT NOT NULL DEFAULT '',
  session          TEXT NOT NULL DEFAULT '{}',
  ino              INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL,
  tombstone        INTEGER NOT NULL DEFAULT 0,
  needs_publish    INTEGER NOT NULL DEFAULT 0,
  meta_identity    TEXT NOT NULL DEFAULT '',
  mount_prefix     TEXT NOT NULL DEFAULT '',
  mount_root_id    TEXT NOT NULL DEFAULT '',
  account_binding  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS uploads_state ON uploads(state, next_retry_at);

CREATE TABLE IF NOT EXISTS upload_parts (
  upload_id TEXT NOT NULL,
  idx       INTEGER NOT NULL,
  etag      TEXT NOT NULL DEFAULT '',
  state     TEXT NOT NULL DEFAULT 'done',
  PRIMARY KEY (upload_id, idx)
);

CREATE TABLE IF NOT EXISTS dead_letter (
  upload_id TEXT PRIMARY KEY,
  reason    TEXT NOT NULL,
  moved_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS upload_cancellation (
  upload_id TEXT PRIMARY KEY,
  revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS upload_resume_history (
  upload_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  snapshot TEXT NOT NULL,
  PRIMARY KEY(upload_id,revision)
);
`

// journalSchemaVersion tracks the meaning of the stored columns.
//
//	1: next_retry_at holds Unix seconds
//	2: next_retry_at holds Unix milliseconds, so a sub-second settle window or
//	   backoff is not rounded away to "due immediately"
//	3: tombstone records delete-during-upload compensation
//	4: needs_publish gates uploads until their local version is published
//	5: checkpointed copy preparations, kept independently of uncommitted writes
//	6: cancelled preparations and revision-guarded explicit retry
//	7: durable cleanup intents for copy records and their private payloads
//
// 8: cancellation states are durable and cannot be reset by stale workers.
// 9: cancellation revisions and preserved session history for explicit resume.
// 10: fenced upload cleanup and minimal permanent discard receipts.
// 11: original metadata, mount and account-generation bindings on uploads.
const journalSchemaVersion = 11

func (j *Journal) migrate() error {
	if _, err := j.db.Exec(journalSchema); err != nil {
		return fmt.Errorf("journal: migrate: %w", err)
	}
	var current int
	if err := j.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("journal: read user_version: %w", err)
	}
	if current > journalSchemaVersion {
		return fmt.Errorf("journal: database schema v%d is newer than this build (v%d)", current, journalSchemaVersion)
	}
	if _, err := j.db.Exec(copySchema); err != nil {
		return fmt.Errorf("journal: migrate copy preparations: %w", err)
	}
	if _, err := j.db.Exec(copyCleanupSchema); err != nil {
		return fmt.Errorf("journal: migrate copy cleanup: %w", err)
	}
	if _, err := j.db.Exec(uploadCleanupSchema); err != nil {
		return fmt.Errorf("journal: migrate upload cleanup: %w", err)
	}
	var copyRevision int
	if err := j.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('copy_jobs') WHERE name='revision'`).Scan(&copyRevision); err != nil {
		return err
	}
	if copyRevision == 0 {
		// Rebuild atomically because SQLite cannot extend an existing CHECK.
		tx, err := j.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, query := range []string{
			`DROP INDEX IF EXISTS copy_destination`,
			`ALTER TABLE copy_jobs RENAME TO copy_jobs_v5`,
			copySchema,
			`INSERT INTO copy_jobs(id,spec,want,state,checkpoint,crc32c,target_remote,target_parent,target_name,last_error) SELECT id,spec,want,state,checkpoint,crc32c,target_remote,target_parent,target_name,last_error FROM copy_jobs_v5`,
			`DROP TABLE copy_jobs_v5`,
			`PRAGMA user_version = 6`,
		} {
			if _, err := tx.Exec(query); err != nil {
				return fmt.Errorf("journal: migrate copy administration: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if current == 1 {
		if _, err := j.db.Exec(`UPDATE uploads SET next_retry_at = next_retry_at * 1000 WHERE next_retry_at > 0`); err != nil {
			return fmt.Errorf("journal: migrate retry times to milliseconds: %w", err)
		}
	}
	if current < 3 && current > 0 {
		// v3: a tombstone flag for uploads deleted while in flight.
		if _, err := j.db.Exec(`ALTER TABLE uploads ADD COLUMN tombstone INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("journal: migrate tombstone: %w", err)
		}
	}
	if current < journalSchemaVersion {
		if _, err := j.db.Exec(`ALTER TABLE uploads ADD COLUMN needs_publish INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("journal: migrate publication barrier: %w", err)
		}
		for _, column := range []string{
			"meta_identity TEXT NOT NULL DEFAULT ''",
			"mount_prefix TEXT NOT NULL DEFAULT ''",
			"mount_root_id TEXT NOT NULL DEFAULT ''",
			"account_binding TEXT NOT NULL DEFAULT ''",
		} {
			if _, err := j.db.Exec(`ALTER TABLE uploads ADD COLUMN ` + column); err != nil && !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("journal: migrate upload binding: %w", err)
			}
		}
		if _, err := j.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, journalSchemaVersion)); err != nil {
			return fmt.Errorf("journal: set user_version: %w", err)
		}
	}
	return nil
}

// unixMilli and fromUnixMilli convert between the stored representation and
// time.Time. Retry scheduling needs sub-second resolution.
func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (j *Journal) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	j.writeMu.Lock()
	defer j.writeMu.Unlock()
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("journal: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("journal: commit: %w", err)
	}
	return nil
}

// Commit durably records an upload. The blob must already be fsynced. It is
// the point after which close() may return: the data is on local disk and the
// queue will retry until it lands remotely.
func (j *Journal) Commit(ctx context.Context, u Upload) error {
	if u.ID == "" {
		return errors.New("journal: upload needs an id")
	}
	if err := j.committer.submit(ctx, u); err != nil {
		return err // keep the staging reservation for a retry
	}
	j.releaseStagedObject(u.StagingID, u.BlobPath)
	return nil
}

func (j *Journal) releaseStagedObject(id, blob string) {
	if id == "" {
		return
	}
	absBlob, err := filepath.Abs(blob)
	if err != nil {
		return
	}
	j.objectMu.Lock()
	defer j.objectMu.Unlock()
	if owned, ok := j.stagedObjects[id]; !ok || owned != absBlob {
		return
	}
	delete(j.stagedObjects, id)
	j.stagedRefs[absBlob]--
	if j.stagedRefs[absBlob] == 0 {
		delete(j.stagedRefs, absBlob)
	}
}

const legacyUploadCols = `id, remote, remote_parent_id, name, blob_path, size, hashes, state,
                    attempt, next_retry_at, last_error, expected_version, session, ino, created_at, tombstone`
const uploadCols = legacyUploadCols + `, needs_publish, meta_identity, mount_prefix, mount_root_id, account_binding`

func (j *Journal) readUploadCols() string {
	cols := legacyUploadCols
	if j.legacyPublication {
		cols += `, 0`
	} else {
		cols += `, needs_publish`
	}
	if j.legacyUploadBinding {
		return cols + `, '', '', '', ''`
	}
	return cols + `, meta_identity, mount_prefix, mount_root_id, account_binding`
}

func scanUpload(sc interface{ Scan(...any) error }) (Upload, error) {
	var u Upload
	var hashes, session, state string
	var nextRetry, created int64
	var tomb, unpublished int
	err := sc.Scan(&u.ID, &u.Remote, &u.RemoteParentID, &u.Name, &u.BlobPath, &u.Size, &hashes,
		&state, &u.Attempt, &nextRetry, &u.LastError, &u.ExpectedVersion, &session, &u.Ino, &created, &tomb, &unpublished,
		&u.MetaIdentity, &u.MountPrefix, &u.MountRootID, &u.AccountBinding)
	if err != nil {
		return Upload{}, err
	}
	u.Tombstone = tomb != 0
	u.NeedsPublish = unpublished != 0
	u.State = State(state)
	u.NextRetryAt = fromUnixMilli(nextRetry)
	u.CreatedAt = time.Unix(created, 0)
	if err := json.Unmarshal([]byte(hashes), &u.Hashes); err != nil {
		u.Hashes = provider.Hashes{}
	}
	if err := json.Unmarshal([]byte(session), &u.Session); err != nil {
		u.Session = map[string]string{}
	}
	return u, nil
}

// Get returns one upload.
func (j *Journal) Get(ctx context.Context, id string) (Upload, error) {
	row := j.db.QueryRowContext(ctx, `SELECT `+j.readUploadCols()+` FROM uploads WHERE id = ?`, id)
	u, err := scanUpload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if err != nil {
		return Upload{}, fmt.Errorf("journal: get: %w", err)
	}
	return u, nil
}

// Claim atomically takes up to n due uploads for a remote and marks them
// uploading, so multiple workers never pick the same row.
func (j *Journal) Claim(ctx context.Context, remote string, n int) ([]Upload, error) {
	var out []Upload
	err := j.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(
			`SELECT `+uploadCols+` FROM uploads
			 WHERE remote = ? AND state = ? AND next_retry_at <= ? AND needs_publish = 0
			 ORDER BY rowid LIMIT ?`,
			remote, string(StatePending), unixMilli(j.now()), n)
		if err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		for rows.Next() {
			u, err := scanUpload(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("journal: claim: %w", err)
			}
			out = append(out, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		for i := range out {
			out[i].State = StateUploading
			if _, err := tx.Exec(`UPDATE uploads SET state = ? WHERE id = ?`, string(StateUploading), out[i].ID); err != nil {
				return fmt.Errorf("journal: claim: %w", err)
			}
		}
		return nil
	})
	return out, err
}

// Superseded reports whether a newer upload of the same file has been
// committed since u was. Two uploads of one file are two versions of its
// content and the workers run them in parallel, so without this the small one
// queued first — the truncate a rewrite starts with — can finish after the
// large one that replaced it, and the backend keeps the wrong version.
// Skipping the older one is what the same-handle supersede already does; this
// covers the ones that were already claimed or came from another handle.
func (j *Journal) Superseded(ctx context.Context, u Upload) (bool, error) {
	if u.Ino == 0 || u.Tombstone {
		return false, nil
	}
	var newer int
	err := j.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM uploads
		  WHERE remote = ? AND ino = ? AND tombstone = 0
		    AND rowid > (SELECT rowid FROM uploads WHERE id = ?)`,
		u.Remote, u.Ino, u.ID).Scan(&newer)
	if err != nil {
		return false, fmt.Errorf("journal: superseded: %w", err)
	}
	return newer > 0, nil
}

// DropSuperseded removes the still-queued uploads of one file that a newer
// commit has replaced, and reports how many went. Rewriting a file is two
// commits — the truncate the kernel does on a handle of its own, then the
// content — and only the second one is worth sending; the handle that wrote
// the content cannot drop the first, because it never saw it.
func (j *Journal) DropSuperseded(ctx context.Context, remote string, ino uint64, keepID string) (int, error) {
	if ino == 0 {
		return 0, nil
	}
	rows, err := j.db.QueryContext(ctx,
		`SELECT id FROM uploads
		  WHERE remote = ? AND ino = ? AND state = ? AND tombstone = 0
		    AND rowid < (SELECT rowid FROM uploads WHERE id = ?)`,
		remote, ino, string(StatePending), keepID)
	if err != nil {
		return 0, fmt.Errorf("journal: drop superseded: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("journal: drop superseded: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("journal: drop superseded: %w", err)
	}
	dropped := 0
	if j.dropSnapshotFault != nil {
		j.dropSnapshotFault()
	}
	for _, id := range ids {
		err := j.dropIdle(ctx, id, true)
		switch {
		case err == nil:
			dropped++
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrInFlight), errors.Is(err, errNotPending), errors.Is(err, ErrCancelled):
			// A worker or administrator changed this snapshot row. Its
			// current owner must finish bookkeeping; do not erase the intent.
		default:
			return dropped, err
		}
	}
	return dropped, nil
}

// OlderInFlight reports whether an earlier upload of the same file is still
// on its way out. The newer one has to wait: both write the same remote path,
// and the conflict check reads the version the older one is about to change.
func (j *Journal) OlderInFlight(ctx context.Context, u Upload) (bool, error) {
	if u.Ino == 0 || u.Tombstone {
		return false, nil
	}
	var older int
	err := j.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM uploads
		  WHERE remote = ? AND ino = ? AND tombstone = 0
		    AND state IN (?, ?, ?)
		    AND rowid < (SELECT rowid FROM uploads WHERE id = ?)`,
		u.Remote, u.Ino, string(StatePending), string(StateUploading), string(StateCancelling), u.ID).Scan(&older)
	if err != nil {
		return false, fmt.Errorf("journal: older in flight: %w", err)
	}
	return older > 0, nil
}

// Pending lists queued uploads for `cloudfs uploads list`.
func (j *Journal) Pending(ctx context.Context) ([]Upload, error) {
	return j.list(ctx, `WHERE state IN (?, ?) ORDER BY created_at`, string(StatePending), string(StateUploading))
}

// Dead lists dead-lettered uploads.
func (j *Journal) Dead(ctx context.Context) ([]Upload, error) {
	return j.list(ctx, `WHERE state = ? ORDER BY created_at`, string(StateDead))
}

// All lists every upload row.
func (j *Journal) All(ctx context.Context) ([]Upload, error) {
	return j.list(ctx, `ORDER BY created_at`)
}

func (j *Journal) list(ctx context.Context, where string, args ...any) ([]Upload, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT `+j.readUploadCols()+` FROM uploads `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("journal: list: %w", err)
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, fmt.Errorf("journal: list: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Remotes lists the remotes with queued work, so the uploader knows which
// worker pools to run.
func (j *Journal) Remotes(ctx context.Context) ([]string, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT DISTINCT remote FROM uploads WHERE state IN (?, ?)`, string(StatePending), string(StateUploading))
	if err != nil {
		return nil, fmt.Errorf("journal: remotes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("journal: remotes: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ByIno returns the queued uploads for a metadata inode, newest last. The VFS
// uses it to retarget or cancel a write that has not been sent yet.
func (j *Journal) ByIno(ctx context.Context, ino uint64) ([]Upload, error) {
	return j.list(ctx, `WHERE ino = ? AND state IN (?, ?, ?, ?, ?) ORDER BY created_at`,
		ino, string(StatePending), string(StateUploading), string(StateCancelling), string(StateCancelled), string(StatePurging))
}

// Retarget changes where a queued upload will land. Renaming or moving a file
// that has not been uploaded yet must move the pending write with it, not
// leave it pointing at the old name.
func (j *Journal) Retarget(ctx context.Context, id, parentID, name string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		res, err := tx.Exec(
			`UPDATE uploads SET remote_parent_id = ?, name = ? WHERE id = ? AND state = ?`,
			parentID, name, id, string(StatePending))
		if err != nil {
			return fmt.Errorf("journal: retarget: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Already uploading or gone: the caller falls back to a server-side
			// rename of the finished file.
			return ErrNotFound
		}
		return nil
	})
}

// SetSession persists provider upload state so a resumed upload continues.
func (j *Journal) SetSession(ctx context.Context, id string, session map[string]string) error {
	b, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("journal: marshal session: %w", err)
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotPurging(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE uploads SET session = ? WHERE id = ?`, string(b), id)
		if err != nil {
			return fmt.Errorf("journal: set session: %w", err)
		}
		return nil
	})
}

// RecordPart marks one chunk uploaded.
func (j *Journal) RecordPart(ctx context.Context, id string, p Part) error {
	if p.State == "" {
		p.State = "done"
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotPurging(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO upload_parts (upload_id, idx, etag, state) VALUES (?,?,?,?)
			 ON CONFLICT(upload_id, idx) DO UPDATE SET etag=excluded.etag, state=excluded.state`,
			id, p.Index, p.ETag, p.State)
		if err != nil {
			return fmt.Errorf("journal: record part: %w", err)
		}
		return nil
	})
}

// Parts returns the recorded chunks of an upload, ordered by index. A resumed
// upload skips these.
func (j *Journal) Parts(ctx context.Context, id string) ([]Part, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT idx, etag, state FROM upload_parts WHERE upload_id = ? ORDER BY idx`, id)
	if err != nil {
		return nil, fmt.Errorf("journal: parts: %w", err)
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.Index, &p.ETag, &p.State); err != nil {
			return nil, fmt.Errorf("journal: parts: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Succeed marks an upload done and removes its parts. The blob stays on disk;
// the caller adopts it into the read cache first.
func (j *Journal) Succeed(ctx context.Context, id string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE uploads SET state = ?, last_error = '' WHERE id = ?`, string(StateDone), id); err != nil {
			return fmt.Errorf("journal: succeed: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM upload_parts WHERE upload_id = ?`, id); err != nil {
			return fmt.Errorf("journal: succeed: %w", err)
		}
		return nil
	})
}

// Retry reschedules an upload after a failure.
func (j *Journal) Retry(ctx context.Context, id string, cause error, delay time.Duration) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE uploads SET state = ?, attempt = attempt + 1, next_retry_at = ?, last_error = ? WHERE id = ?`,
			string(StatePending), unixMilli(j.now().Add(delay)), msg, id)
		if err != nil {
			return fmt.Errorf("journal: retry: %w", err)
		}
		return nil
	})
}

// Defer puts an upload back in the queue without counting an attempt: it is
// waiting its turn, not failing. Attempts are the retry budget that ends in
// the dead-letter queue, and standing aside must never spend it.
func (j *Journal) Defer(ctx context.Context, id string, delay time.Duration, why string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE uploads SET state = ?, next_retry_at = ?, last_error = ? WHERE id = ?`,
			string(StatePending), unixMilli(j.now().Add(delay)), why, id)
		if err != nil {
			return fmt.Errorf("journal: defer: %w", err)
		}
		return nil
	})
}

// Fail dead-letters an upload. The blob is kept so `cloudfs uploads retry`
// can requeue it.
func (j *Journal) Fail(ctx context.Context, id string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE uploads SET state = ?, last_error = ? WHERE id = ?`, string(StateDead), msg, id); err != nil {
			return fmt.Errorf("journal: fail: %w", err)
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO dead_letter (upload_id, reason, moved_at) VALUES (?,?,?)`,
			id, msg, j.now().Unix())
		if err != nil {
			return fmt.Errorf("journal: fail: %w", err)
		}
		return nil
	})
}

// Requeue moves a dead-lettered upload back to pending.
func (j *Journal) Requeue(ctx context.Context, id string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotPurging(tx, id); err != nil {
			return err
		}
		var state State
		if err := tx.QueryRow(`SELECT state FROM uploads WHERE id = ?`, id).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state == StateUploading {
			return ErrInFlight
		}
		if state == StateCancelled || state == StateCancelling {
			return ErrCancelled
		}
		if state != StateDead {
			return ErrNotDead
		}
		res, err := tx.Exec(
			`UPDATE uploads SET state = ?, attempt = 0, next_retry_at = 0, last_error = '' WHERE id = ?`,
			string(StatePending), id)
		if err != nil {
			return fmt.Errorf("journal: requeue: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`DELETE FROM dead_letter WHERE upload_id = ?`, id); err != nil {
			return fmt.Errorf("journal: requeue: %w", err)
		}
		return nil
	})
}

// DropPending removes a queued upload only while it is still pending. Once
// an uploader has claimed the row, deleting it would let the transfer finish
// and leave the file on the backend with nothing recording that its local
// copy was deleted — a delete that resurrects its target after the next
// listing. That row must be tombstoned instead, and ErrInFlight says so.
func (j *Journal) DropPending(ctx context.Context, id string) error {
	return j.dropIdle(ctx, id, false)
}

var errNotPending = errors.New("journal: upload is no longer pending")

// dropIdle reads state and removes the row in the same writer transaction.
// In particular, a dead-letter retry cannot sneak between an idle-state
// check and an unconditional delete. Supersession only removes pending rows;
// explicit local deletion may also remove terminal dead/done rows.
func (j *Journal) dropIdle(ctx context.Context, id string, pendingOnly bool) error {
	var blob string
	err := j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotPurging(tx, id); err != nil {
			return err
		}
		var state State
		if err := tx.QueryRow(`SELECT state,blob_path FROM uploads WHERE id=?`, id).Scan(&state, &blob); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("journal: drop state: %w", err)
		}
		if state == StateUploading || state == StateCancelling {
			return ErrInFlight
		}
		if state == StateCancelled {
			return ErrCancelled
		}
		if pendingOnly && state != StatePending {
			return errNotPending
		}
		if state != StatePending && state != StateDead && state != StateDone {
			return fmt.Errorf("journal: cannot discard upload state %q", state)
		}
		for _, q := range []string{
			`DELETE FROM uploads WHERE id = ?`,
			`DELETE FROM upload_parts WHERE upload_id = ?`,
			`DELETE FROM dead_letter WHERE upload_id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return fmt.Errorf("journal: drop: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	j.removeBlobIfUnreferenced(ctx, blob)
	return nil
}

// Drop removes an upload and its blob. Used by `cloudfs uploads drop`.
func (j *Journal) Drop(ctx context.Context, id string) error {
	u, err := j.Get(ctx, id)
	if err != nil {
		return err
	}
	err = j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM uploads WHERE id = ?`,
			`DELETE FROM upload_parts WHERE upload_id = ?`,
			`DELETE FROM dead_letter WHERE upload_id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return fmt.Errorf("journal: drop: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	j.removeBlobIfUnreferenced(ctx, u.BlobPath)
	return nil
}

// removeBlobIfUnreferenced deletes a committed blob only when no other row
// still needs it.
//
// Blobs are content-addressed, so two writes of identical bytes share one
// file. Deleting it with the first row that finishes would destroy the data
// behind every other row pointing at it — which is the whole payload of, say,
// unpacking an archive that contains the same file twice.
func (j *Journal) removeBlobIfUnreferenced(ctx context.Context, blob string) {
	if blob == "" {
		return
	}
	copyDir, err := filepath.Abs(filepath.Join(j.dir, "copies"))
	if err != nil {
		return
	}
	absBlob, err := filepath.Abs(blob)
	if err != nil {
		return
	}
	objectDir, err := filepath.Abs(j.ObjectsDir())
	if err != nil {
		return
	}
	var copyID string
	if filepath.Dir(absBlob) == copyDir {
		id := strings.TrimSuffix(filepath.Base(absBlob), ".part")
		p, err := j.copyPath(id)
		if err != nil || p != absBlob {
			return
		}
		copyID = id
	} else if filepath.Dir(absBlob) != objectDir {
		return // a persisted row is not authority to unlink arbitrary paths
	}
	j.objectMu.Lock()
	defer j.objectMu.Unlock()
	if j.stagedRefs[absBlob] > 0 {
		return
	}
	// Hold the writer transaction through unlink: a new row or copy handoff
	// cannot acquire this pathname after the reference check but before removal.
	_ = j.tx(ctx, func(tx *sql.Tx) error {
		info, err := os.Stat(absBlob)
		if err != nil {
			return err
		}
		shared, err := uploadBlobReferenced(tx, "", absBlob, info)
		if err != nil {
			return err
		}
		if shared {
			return nil
		}
		if copyID != "" {
			var n int
			if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM copy_jobs WHERE id=? AND state != 'submitted') + (SELECT COUNT(*) FROM copy_cleanup WHERE id=?)`, copyID, copyID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
		}
		return os.Remove(absBlob)
	})
}

// Tombstone marks an in-flight upload as deleted locally. The uploader lets
// the transfer finish, then removes the file from the backend and drops the
// row, so a delete that raced an upload does not resurrect the file.
func (j *Journal) Tombstone(ctx context.Context, id string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE uploads SET tombstone = 1 WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("journal: tombstone: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Purge deletes finished rows older than age and their blobs.
func (j *Journal) Purge(ctx context.Context, age time.Duration) (int, error) {
	cutoff := j.now().Add(-age).Unix()
	done, err := j.list(ctx, `WHERE state = ? AND created_at < ?`, string(StateDone), cutoff)
	if err != nil {
		return 0, err
	}
	for _, u := range done {
		if err := j.Drop(ctx, u.ID); err != nil {
			return 0, err
		}
	}
	return len(done), nil
}

// Recovery is what Recover found at startup.
type Recovery struct {
	// Requeued are uploads that were mid-flight and are queued again.
	Requeued []string
	// Lost are uploads whose blob disappeared; they are dead-lettered.
	Lost []string
	// OrphanStaging are staging files with no journal row; they are removed.
	OrphanStaging []string
	// Skipped is true when another process owns the queue, so nothing was
	// changed.
	Skipped bool
}

// Recover runs at startup: every uploading row goes back to pending, rows
// whose blob vanished are dead-lettered, and staging files with no row are
// deleted. This is what makes a kill -9 during a write safe.
func (j *Journal) Recover(ctx context.Context) (Recovery, error) {
	var rec Recovery
	if !j.owner {
		// Another process is running this queue. Recovery here would push its
		// in-flight rows back to pending, delete the staging file a write is
		// filling, and remove the data behind its dead letters.
		rec.Skipped = true
		return rec, nil
	}
	// Every row that still exists owns its blob, whatever its state: a dead
	// letter keeps its data precisely so `cloudfs uploads retry` can work.
	all, err := j.list(ctx, ``)
	if err != nil {
		return rec, err
	}
	live := map[string]bool{}
	for _, u := range all {
		if u.State == StateCancelling {
			if err := j.FinishCancel(ctx, u.ID); err != nil {
				return rec, err
			}
		}
		if u.BlobPath != "" {
			live[filepath.Base(u.BlobPath)] = true
		}
	}
	copyLive, err := j.copyRetention(ctx, all)
	if err != nil {
		return rec, err
	}
	rows, err := j.list(ctx, `WHERE state IN (?, ?)`, string(StatePending), string(StateUploading))
	if err != nil {
		return rec, err
	}
	for _, u := range rows {
		if cause := j.verifyBlob(u); cause != nil {
			if ferr := j.Fail(ctx, u.ID, cause); ferr != nil {
				return rec, ferr
			}
			rec.Lost = append(rec.Lost, u.ID)
			continue
		}
		if u.State == StateUploading {
			if err := j.Retry(ctx, u.ID, errors.New("interrupted by restart"), 0); err != nil {
				return rec, err
			}
			rec.Requeued = append(rec.Requeued, u.ID)
		}
	}
	// Staging files never reached a commit: they are incomplete by definition.
	entries, err := os.ReadDir(j.StagingDir())
	if err != nil && !os.IsNotExist(err) {
		return rec, fmt.Errorf("journal: recover staging: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(j.StagingDir(), e.Name())
		if err := os.Remove(p); err == nil {
			rec.OrphanStaging = append(rec.OrphanStaging, e.Name())
		}
	}
	if err := j.cleanCopyOrphans(copyLive); err != nil {
		return rec, err
	}
	// Objects with no live row are leftovers from dropped uploads.
	objs, err := os.ReadDir(j.ObjectsDir())
	if err != nil && !os.IsNotExist(err) {
		return rec, fmt.Errorf("journal: recover objects: %w", err)
	}
	for _, e := range objs {
		if e.IsDir() || live[e.Name()] {
			continue
		}
		os.Remove(filepath.Join(j.ObjectsDir(), e.Name()))
	}
	return rec, nil
}

// verifyBlob checks that the object a row points at is the object the row
// describes. Existence and size are checked always: a blob that vanished or
// came up short is not something to upload. The checksum is only read in
// crash mode, where a power loss can leave a blob that is the right length
// but not the right bytes; in power mode the blob was fsynced before its
// row existed, so the read would be wasted I/O on every start.
func (j *Journal) verifyBlob(u Upload) error {
	info, err := os.Stat(u.BlobPath)
	if err != nil {
		return fmt.Errorf("blob missing after restart: %s", u.BlobPath)
	}
	if info.Size() != u.Size {
		return fmt.Errorf("blob truncated after restart: %d of %d bytes on disk", info.Size(), u.Size)
	}
	if j.durability != DurabilityCrash {
		return nil
	}
	want, ok := u.Hashes[provider.HashCRC32C]
	if !ok || want == "" {
		return nil
	}
	f, err := os.Open(u.BlobPath)
	if err != nil {
		return fmt.Errorf("blob unreadable after restart: %v", err)
	}
	defer f.Close()
	got, err := crc32cOf(f)
	if err != nil {
		return fmt.Errorf("blob unreadable after restart: %v", err)
	}
	if got != want {
		return fmt.Errorf("blob corrupt after restart: crc32c %s, expected %s (torn by a power loss?)", got, want)
	}
	return nil
}

// Stats summarises the queue for /metrics.
type Stats struct {
	Pending       int
	Uploading     int
	Dead          int
	Done          int
	Cancelling    int
	Cancelled     int
	Purging       int
	RetainedBytes int64
	Bytes         int64
	OldestAge     time.Duration
}

// Stats reads queue counters.
func (j *Journal) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	rows, err := j.db.QueryContext(ctx, `SELECT state, COUNT(*), COALESCE(SUM(size),0), COALESCE(MIN(created_at),0) FROM uploads GROUP BY state`)
	if err != nil {
		return s, fmt.Errorf("journal: stats: %w", err)
	}
	defer rows.Close()
	now := j.now()
	for rows.Next() {
		var state string
		var count int
		var bytes, oldest int64
		if err := rows.Scan(&state, &count, &bytes, &oldest); err != nil {
			return s, fmt.Errorf("journal: stats: %w", err)
		}
		switch State(state) {
		case StatePending:
			s.Pending = count
			s.Bytes += bytes
			if oldest > 0 {
				if age := now.Sub(time.Unix(oldest, 0)); age > s.OldestAge {
					s.OldestAge = age
				}
			}
		case StateUploading:
			s.Uploading = count
			s.Bytes += bytes
			if oldest > 0 {
				if age := now.Sub(time.Unix(oldest, 0)); age > s.OldestAge {
					s.OldestAge = age
				}
			}
		case StateDead:
			s.Dead = count
		case StateDone:
			s.Done = count
		case StateCancelling:
			s.Cancelling = count
			s.RetainedBytes += bytes
		case StateCancelled:
			s.Cancelled = count
			s.RetainedBytes += bytes
		case StatePurging:
			s.Purging = count
			s.RetainedBytes += bytes
		}
	}
	return s, rows.Err()
}
