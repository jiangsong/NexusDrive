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
	"sync/atomic"
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

// Kind says what a queued row does when it runs: send a file's bytes, or
// create a directory. Everything else about a row — its place in the tree,
// retries, cancellation — is the same for both.
type Kind string

const (
	// KindFile uploads the blob at BlobPath. An empty Kind reads as KindFile,
	// which is what every row written before directories were queued has.
	KindFile Kind = "file"
	// KindMkdir creates the directory Name under RemoteParentID. It carries
	// no blob; Size is 0.
	KindMkdir Kind = "mkdir"
	// KindDelete removes the file RemoteID from the backend. Name and
	// RemoteParentID say where it was, which is what orders it before a
	// later write of the same name. It has no node: the tree forgot the
	// file when the row was queued, so Ino is 0.
	KindDelete Kind = "delete"
	// KindRmdir removes the directory RemoteID. It waits for the deletes
	// queued under it, and the backend is asked to confirm the directory is
	// empty before it goes: what another client put there since is not
	// this row's to remove.
	KindRmdir Kind = "rmdir"
)

// LocalIDPrefix marks a remote id that names a queued row rather than
// anything the backend has: "cloudfs-local:<row id>". The VFS gives it to a
// node whose only copy is local. A row whose RemoteParentID carries the
// prefix lands in a directory that is itself still queued, and the queue
// holds it back until that directory exists (Claim) and rewrites the parent
// once it does (RetargetChildren).
const LocalIDPrefix = "cloudfs-local:"

// Upload is one queued write.
type Upload struct {
	ID string
	// Kind is what the row does; see KindFile and KindMkdir.
	Kind Kind
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
	// updated when it completes. A delete has none.
	Ino uint64
	// RemoteID is the backend id a delete row removes. Empty for the other
	// kinds, whose target does not exist yet.
	RemoteID  string
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

// IsMkdir reports whether the row creates a directory rather than sending
// a file.
func (u Upload) IsMkdir() bool { return u.Kind == KindMkdir }

// IsDelete reports whether the row removes something from the backend — a
// file or a directory — rather than putting something there.
func (u Upload) IsDelete() bool { return u.Kind == KindDelete || u.Kind == KindRmdir }

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

// ErrChildrenQueued refuses to drop a directory creation while rows queued
// under it still address it.
var ErrChildrenQueued = errors.New("journal: rows are still queued under this directory")

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
	legacyKind          bool // read-only inspection of pre-v14 rows: all files
	legacyRemoteID      bool // read-only inspection of pre-v15 rows: nothing to delete
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

	// stagingSyncs, objectSyncs and writeTxs count what the write path spends
	// on durability, and they stay three numbers rather than one because the
	// two kinds cost two orders of magnitude apart. The first two are
	// os.File.Sync, which on darwin is F_FULLFSYNC — a device cache flush, 4.1
	// to 4.8 ms measured here. The third is a SQLite transaction at
	// synchronous=FULL, 70 to 90 us, because the driver issues a plain fsync(2)
	// unless PRAGMA fullfsync is set and nothing in this tree sets it (see Open).
	// Summing them would hide a 50x difference. A budget on a close() counts
	// them rather than timing it. See DurabilityStats.
	stagingSyncs atomic.Int64
	objectSyncs  atomic.Int64
	writeTxs     atomic.Int64
	// deviceFlushes counts the subset of the two sync counters that asked the
	// drive to empty its write cache, which on darwin costs fifty-five times
	// what handing it the bytes does. Kept apart for that reason: summing
	// them would hide the only number that moves close(2)'s cost.
	deviceFlushes atomic.Int64
	// onDeviceFlush observes flushes from a test, so the ordering each
	// durability level promises is asserted rather than described.
	onDeviceFlush func()
	// queuedInoScans counts scans of the upload queue for the set of inodes
	// with bytes still waiting. Every node a listing or a change feed touches
	// consults that set, so what matters is how often the set is built, not
	// how often it is read — which is what a regression test asserts on.
	queuedInoScans atomic.Int64
	// flushFault fails the objects flush from a test. A flush that fails
	// after the rows are committed is the one case where what the committer
	// may do next is sharply constrained, and a disk error is not otherwise
	// reachable from a test.
	flushFault func() error
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
	// DurabilityBarrier is the default: one device flush per commit, spent
	// after the row is written so that it covers the staging data, the rename
	// into objects/ and the row itself.
	//
	// It is both faster and a stronger promise than power, because power
	// spends its two flushes *before* the row insert and so never covers the
	// row at all — SQLite issues a plain fsync unless PRAGMA fullfsync is set,
	// and nothing here sets it (TODO.md T-62). On this project's development
	// machine a serial small-file copy costs 9.72 ms per file under power and
	// about 5.6 ms under barrier.
	//
	// What it rests on: a device cache flush persists everything already
	// issued to that device, which is how F_FULLFSYNC is implemented but is
	// not a POSIX guarantee, and APFS does not contract its issue ordering.
	// A drive that lies about its cache defeats it — as it defeats power.
	// Outside darwin there is no cheaper "issue without flushing" call, so
	// barrier costs what power costs there and only the ordering differs.
	DurabilityBarrier Durability = "barrier"
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
		// barrier, not power: it is faster and keeps a stronger promise than
		// power actually kept, because its one flush lands after the row
		// insert and so covers the row. See DurabilityBarrier.
		opt.Durability = DurabilityBarrier
	}
	// synchronous=FULL: an acknowledged close must survive a power loss.
	// In crash mode NORMAL is enough: WAL with NORMAL is consistent after a
	// crash and only the last transactions may be lost after a power cut.
	//
	// UNVERIFIED: on macOS that promise is stronger than what is delivered.
	// SQLite only issues F_FULLFSYNC when PRAGMA fullfsync is on, and nothing
	// in this tree sets it, so these commits get a plain fsync(2) — which on
	// APFS does not flush the device write cache. The staging file and the
	// objects directory beside them do get F_FULLFSYNC, through os.File.Sync.
	// So under power the payload bytes are device-durable and the row naming
	// them is not. Measurement consistent with this: the three SQLite fsyncs
	// on the close path cost about 70 us between them, against 4.07 ms for one
	// real device flush on the same disk. Turning fullfsync on would add that
	// 4 ms to every commit, so it is a durability-contract decision rather
	// than a fix; see TODO.md T-62. Not an issue on Linux, where fsync(2)
	// flushes the device cache.
	//
	// The driver half of this is no longer guesswork: modernc.org/sqlite's
	// generated darwin build compiles HAVE_FULLFSYNC=1 and reaches
	// fcntl(F_FULLFSYNC) only when the pager passes SQLITE_SYNC_FULL, which
	// PRAGMA fullfsync alone sets; opened through the DSN above, the connection
	// reports fullfsync=0 and checkpoint_fullfsync=0 with synchronous=2. What
	// stays UNVERIFIED is the consequence: that a plain fsync(2) on APFS really
	// does leave an acknowledged row behind in the device cache. To verify,
	// pull power mid-write on real hardware.
	sync := "FULL"
	if opt.Durability == DurabilityCrash {
		sync = "NORMAL"
	}
	// journal_size_limit: a queue of twenty thousand rows under eight
	// workers keeps a reader on the WAL at almost every instant, so the
	// automatic checkpoints copy pages back but rarely get to reset the
	// file, and it grew to nearly a gigabyte beside a 15 MB database. The
	// limit truncates it the next time a checkpoint does reset it, and
	// Checkpoint forces such a moment when the queue is quiet.
	dsn := "file:" + filepath.Join(opt.Dir, "journal.db") +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(" + sync + ")&_pragma=busy_timeout(5000)&_txlock=immediate" +
		fmt.Sprintf("&_pragma=journal_size_limit(%d)", walSizeLimit)
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
	return &Journal{db: db, dir: dir, now: time.Now, legacyPublication: version < 4, legacyUploadBinding: version < 11, legacyCopyAdmin: version < 6, legacyKind: version < 14, legacyRemoteID: version < 15}, nil
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

// walSizeLimit is what the write-ahead log is truncated back to after a
// checkpoint resets it. Big enough that a busy minute never truncates.
const walSizeLimit = 64 << 20

// Checkpoint folds the write-ahead log back into the database and
// truncates it, waiting up to busy_timeout for readers to step aside. It
// reports whether the log was truncated; a queue that is never quiet keeps
// the log from ever being reset, so the daemon asks at intervals rather
// than at the one moment that would be convenient.
func (j *Journal) Checkpoint(ctx context.Context) (bool, error) {
	var busy, logFrames, checkpointed int
	if err := j.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return false, fmt.Errorf("journal: checkpoint: %w", err)
	}
	return busy == 0, nil
}

// StagingDir is where in-progress writes are buffered.
func (j *Journal) StagingDir() string { return filepath.Join(j.dir, "staging") }

// ObjectsDir holds committed blobs awaiting upload.
func (j *Journal) ObjectsDir() string { return filepath.Join(j.dir, "objects") }

// CopiesDir holds the payload of a copy the backend could not do server-side,
// until the upload it is handed to finishes.
func (j *Journal) CopiesDir() string { return filepath.Join(j.dir, "copies") }

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
  account_binding  TEXT NOT NULL DEFAULT '',
  done_at          INTEGER NOT NULL DEFAULT 0,
  kind             TEXT NOT NULL DEFAULT 'file',
  remote_id        TEXT NOT NULL DEFAULT ''
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
// 12: durable intents for server-side copies, whose result an error does not
//
//	determine, so an ambiguous answer becomes a question reconciliation asks
//	the provider rather than a guess.
//
// 14: kind distinguishes queued directory creations from file uploads.
const journalSchemaVersion = 15

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
	if _, err := j.db.Exec(serverCopySchema); err != nil {
		return fmt.Errorf("journal: migrate server copy intents: %w", err)
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
			// v13: when an upload finished, so completed rows can be reclaimed.
			// Rows completed before the upgrade have 0 and are kept: their age
			// is unknown, and keeping a row costs almost nothing.
			"done_at INTEGER NOT NULL DEFAULT 0",
			// v14: queued directory creations. Every earlier row is a file.
			"kind TEXT NOT NULL DEFAULT 'file'",
			// v15: queued deletes name what they remove.
			"remote_id TEXT NOT NULL DEFAULT ''",
		} {
			if _, err := j.db.Exec(`ALTER TABLE uploads ADD COLUMN ` + column); err != nil && !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("journal: migrate upload binding: %w", err)
			}
		}
		if _, err := j.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, journalSchemaVersion)); err != nil {
			return fmt.Errorf("journal: set user_version: %w", err)
		}
	}
	// After the columns it covers exist on a database from before them.
	// Claim probes it for the delete rows that hold a row back.
	if _, err := j.db.Exec(`CREATE INDEX IF NOT EXISTS uploads_by_parent_name ON uploads(remote, kind, remote_parent_id, name)`); err != nil {
		return fmt.Errorf("journal: migrate delete ordering index: %w", err)
	}
	// Claim walks a remote's pending rows in rowid order; an index whose
	// trailing key is the rowid lets it stop at the first page instead of
	// sorting the whole queue each time.
	if _, err := j.db.Exec(`CREATE INDEX IF NOT EXISTS uploads_due ON uploads(remote, state, needs_publish)`); err != nil {
		return fmt.Errorf("journal: migrate claim index: %w", err)
	}
	// Every commit, unlink and worker asks about one inode's rows; with
	// thousands queued, each such question scanned the table.
	if _, err := j.db.Exec(`CREATE INDEX IF NOT EXISTS uploads_by_ino ON uploads(ino)`); err != nil {
		return fmt.Errorf("journal: migrate inode index: %w", err)
	}
	// Rows by the directory they go into, for Stats' walk down queued
	// directories and RetargetChildren.
	if _, err := j.db.Exec(`CREATE INDEX IF NOT EXISTS uploads_by_parent ON uploads(remote_parent_id)`); err != nil {
		return fmt.Errorf("journal: migrate parent index: %w", err)
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
	j.writeTxs.Add(1)
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
const uploadCols = legacyUploadCols + `, needs_publish, meta_identity, mount_prefix, mount_root_id, account_binding, kind, remote_id`

func (j *Journal) readUploadCols() string {
	cols := legacyUploadCols
	if j.legacyPublication {
		cols += `, 0`
	} else {
		cols += `, needs_publish`
	}
	if j.legacyUploadBinding {
		cols += `, '', '', '', ''`
	} else {
		cols += `, meta_identity, mount_prefix, mount_root_id, account_binding`
	}
	if j.legacyKind {
		cols += `, 'file'`
	} else {
		cols += `, kind`
	}
	if j.legacyRemoteID {
		return cols + `, ''`
	}
	return cols + `, remote_id`
}

func scanUpload(sc interface{ Scan(...any) error }) (Upload, error) {
	var u Upload
	var hashes, session, state string
	var nextRetry, created int64
	var tomb, unpublished int
	var kind string
	err := sc.Scan(&u.ID, &u.Remote, &u.RemoteParentID, &u.Name, &u.BlobPath, &u.Size, &hashes,
		&state, &u.Attempt, &nextRetry, &u.LastError, &u.ExpectedVersion, &session, &u.Ino, &created, &tomb, &unpublished,
		&u.MetaIdentity, &u.MountPrefix, &u.MountRootID, &u.AccountBinding, &kind, &u.RemoteID)
	if err != nil {
		return Upload{}, err
	}
	u.Kind = Kind(kind)
	if u.Kind == "" {
		u.Kind = KindFile
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
// uploading, so multiple workers never pick the same row. A row whose parent
// directory is itself still queued (RemoteParentID carries LocalIDPrefix) is
// not due: the backend has no such parent to put it in. It becomes due when
// the directory lands and RetargetChildren rewrites the parent.
//
// Deletes order the queue in two more ways, both by name and parent rather
// than by node, because the node a delete removed is gone. A write or mkdir
// is not due while a delete of the same name in the same directory is still
// out: the backends put a file by name, and on one whose ids are paths — or
// one that patches the existing file in place, which is what Drive does —
// the write would land on the very file the delete then removes. And a
// directory's delete is not due while deletes queued under it are still
// out: the backend is asked to confirm the directory is empty, and children
// that have not gone yet are not someone else's files.
func (j *Journal) Claim(ctx context.Context, remote string, n int) ([]Upload, error) {
	// Finding the candidates is a read, outside the write transaction: it
	// walks the queue in row order past every row a delete holds back, and
	// after rm -rf and a copy of the same tree that is hundreds of rows
	// ahead of the first claimable one. Done inside the transaction, that
	// walk held the journal's write lock for most of every second, and
	// every close(2) and unlink on the mount waited behind the workers.
	// The write transaction only re-checks and takes the rows it was handed.
	ids, err := j.dueRowids(ctx, remote, n)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var out []Upload
	err = j.tx(ctx, func(tx *sql.Tx) error {
		for _, rowid := range ids {
			u, err := uploadByRowidTx(tx, rowid)
			if errors.Is(err, sql.ErrNoRows) {
				continue // dropped since the read
			}
			if err != nil {
				return err
			}
			if u.State != StatePending || u.NeedsPublish || strings.HasPrefix(u.RemoteParentID, LocalIDPrefix) {
				continue // taken or held back since the read
			}
			if blocked, err := heldByDeleteTx(tx, u); err != nil {
				return err
			} else if blocked {
				continue
			}
			u.State = StateUploading
			if _, err := tx.Exec(`UPDATE uploads SET state = ? WHERE id = ?`, string(StateUploading), u.ID); err != nil {
				return fmt.Errorf("journal: claim: %w", err)
			}
			out = append(out, u)
		}
		return nil
	})
	return out, err
}

// dueRowids lists, oldest first, up to n rows a worker for remote could
// take now. The delete ordering is applied in the query (see
// heldByDeleteTx for what it says), as index probes on uploads_by_parent_name
// for each row the walk visits; uploads_due hands the rows over in rowid
// order so the walk stops at the n-th claimable one. The indexes are named:
// without statistics the planner chose the state index for the probes and
// a sort for the walk, and a claim past 1500 held-back rows took seconds.
func (j *Journal) dueRowids(ctx context.Context, remote string, n int) ([]int64, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT u.rowid FROM uploads AS u INDEXED BY uploads_due
		 WHERE u.remote = ? AND u.state = ? AND u.needs_publish = 0 AND u.next_retry_at <= ?
		   AND substr(u.remote_parent_id, 1, ?) <> ?
		   AND NOT EXISTS (
		     SELECT 1 FROM uploads AS d INDEXED BY uploads_by_parent_name
		      WHERE d.remote = u.remote AND d.kind IN (?, ?) AND d.remote_parent_id = u.remote_parent_id AND d.name = u.name
		        AND d.state IN (?, ?) AND d.id <> u.id AND (u.kind NOT IN (?, ?) OR d.rowid < u.rowid))
		   AND NOT (u.kind = ? AND EXISTS (
		     SELECT 1 FROM uploads AS d INDEXED BY uploads_by_parent_name
		      WHERE d.remote = u.remote AND d.kind IN (?, ?) AND d.remote_parent_id = u.remote_id AND d.state IN (?, ?)))
		 ORDER BY u.rowid LIMIT ?`,
		remote, string(StatePending), unixMilli(j.now()), len(LocalIDPrefix), LocalIDPrefix,
		string(KindDelete), string(KindRmdir), string(StatePending), string(StateUploading), string(KindDelete), string(KindRmdir),
		string(KindRmdir), string(KindDelete), string(KindRmdir), string(StatePending), string(StateUploading), n)
	if err != nil {
		return nil, fmt.Errorf("journal: claim: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("journal: claim: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: claim: %w", err)
	}
	return out, nil
}

// ClaimID takes one specific row, if it is due, and marks it uploading. The
// caller runs it itself. ErrInFlight says a worker has it; ErrNotFound says
// it is finished, gone, or not due (unpublished, or under a queued parent).
func (j *Journal) ClaimID(ctx context.Context, id string) (Upload, error) {
	var out Upload
	err := j.tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id)
		u, err := scanUpload(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		if u.State == StateUploading {
			return ErrInFlight
		}
		if u.State != StatePending || u.NeedsPublish || strings.HasPrefix(u.RemoteParentID, LocalIDPrefix) {
			return ErrNotFound
		}
		if blocked, err := heldByDeleteTx(tx, u); err != nil {
			return err
		} else if blocked {
			return ErrNotFound
		}
		if _, err := tx.Exec(`UPDATE uploads SET state = ? WHERE id = ?`, string(StateUploading), id); err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		u.State = StateUploading
		out = u
		return nil
	})
	return out, err
}

func uploadByRowidTx(tx *sql.Tx, rowid int64) (Upload, error) {
	u, err := scanUpload(tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE rowid = ?`, rowid))
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, err
	}
	if err != nil {
		return Upload{}, fmt.Errorf("journal: claim: %w", err)
	}
	return u, nil
}

// heldByDeleteTx reports whether an unfinished delete row holds u back:
// one for the same name in the same directory, or, when u is itself a
// directory's delete, one queued under that directory. See Claim. Both are
// probes of uploads_by_parent_name.
//
// The same-name rule is not by row order: the delete is sometimes the
// younger row. An editor saves by writing a temporary file and renaming it
// over the original, and the write was queued before the rename removed the
// original — but the original still has to go first, or the write patches
// the file the delete then removes. A delete for a name is only ever queued
// for a node the backend has, and a queued write of that name is a node the
// backend does not have, so the two never describe the same file and the
// delete is always the one to go first. Two deletes of one name — the
// backend grew another file by that name after the first was removed here
// — keep their order.
func heldByDeleteTx(tx *sql.Tx, u Upload) (bool, error) {
	var held int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM uploads INDEXED BY uploads_by_parent_name
		  WHERE remote = ? AND kind IN (?, ?) AND remote_parent_id = ? AND name = ?
		    AND state IN (?, ?) AND id <> ? AND (? OR rowid < (SELECT rowid FROM uploads WHERE id = ?))`,
		u.Remote, string(KindDelete), string(KindRmdir), u.RemoteParentID, u.Name,
		string(StatePending), string(StateUploading), u.ID, !u.IsDelete(), u.ID).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("journal: claim: %w", err)
	}
	if held > 0 {
		return true, nil
	}
	if u.Kind != KindRmdir {
		return false, nil
	}
	err = tx.QueryRow(
		`SELECT COUNT(*) FROM uploads INDEXED BY uploads_by_parent_name
		  WHERE remote = ? AND kind IN (?, ?) AND remote_parent_id = ? AND state IN (?, ?)`,
		u.Remote, string(KindDelete), string(KindRmdir), u.RemoteID,
		string(StatePending), string(StateUploading)).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("journal: claim: %w", err)
	}
	return held > 0, nil
}

// QueuedDeletes lists the delete rows still to run, for the tree to know
// which backend entries a listing must not bring back. A dead one is not
// among them: its target is staying on the backend, and the next listing
// should say so.
func (j *Journal) QueuedDeletes(ctx context.Context) ([]Upload, error) {
	return j.list(ctx, `WHERE kind IN (?, ?) AND state IN (?, ?)`,
		string(KindDelete), string(KindRmdir), string(StatePending), string(StateUploading))
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

// QueuedInos returns the inodes whose bytes are still in the journal waiting
// to go out. A directory listing uses it to keep those nodes: the backend
// cannot have told the listing about a file it has never received, so without
// this the listing prunes the node and orphans the upload row.
func (j *Journal) QueuedInos(ctx context.Context) (map[uint64]bool, error) {
	j.queuedInoScans.Add(1)
	rows, err := j.db.QueryContext(ctx,
		`SELECT DISTINCT ino FROM uploads WHERE ino != 0 AND state IN (?, ?)`,
		string(StatePending), string(StateUploading))
	if err != nil {
		return nil, fmt.Errorf("journal: queued inodes: %w", err)
	}
	defer rows.Close()
	out := map[uint64]bool{}
	for rows.Next() {
		var ino uint64
		if err := rows.Scan(&ino); err != nil {
			return nil, fmt.Errorf("journal: queued inodes: %w", err)
		}
		out[ino] = true
	}
	return out, rows.Err()
}

// QueuedInoScans counts how many times the queue has been scanned for that
// set. A caller that applies a batch of remote changes builds it once for the
// batch; one scan per changed node is the regression this counts.
func (j *Journal) QueuedInoScans() int64 { return j.queuedInoScans.Load() }

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

// RetargetParent moves one queued row to another parent, keeping its name.
// It is how a row that addressed a queued directory follows it once the
// directory has landed. Claimed or finished rows are left alone.
func (j *Journal) RetargetParent(ctx context.Context, id, parentID string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE uploads SET remote_parent_id = ? WHERE id = ? AND state = ?`,
			parentID, id, string(StatePending))
		if err != nil {
			return fmt.Errorf("journal: retarget parent: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RetargetChildren rewrites the parent of every unfinished row queued under
// oldParentID — the local id of a directory that has just been created on
// the backend — to the id the backend gave it. Finished rows are history
// and are left alone; every other state follows, a cancelled row included,
// because resuming it later must address a parent the backend has.
func (j *Journal) RetargetChildren(ctx context.Context, oldParentID, newParentID string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE uploads SET remote_parent_id = ? WHERE remote_parent_id = ? AND state <> ?`,
			newParentID, oldParentID, string(StateDone))
		if err != nil {
			return fmt.Errorf("journal: retarget children: %w", err)
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

// doneHistory and doneRetention bound what a finished upload leaves behind.
// A completed row is kept so that a strict-mode writer waiting on the upload
// can observe it finishing, and so recent history is inspectable; beyond that
// it is only a row nobody reads. Both bounds have to hold before one is
// reclaimed: the age floor means a waiter would have to be stalled for minutes
// before its row could be taken out from under it.
const (
	doneHistory   = 200
	doneRetention = 5 * time.Minute
)

// Succeed marks an upload done, removes its parts and releases the queue's
// hold on the blob.
//
// The blob is a hard link shared with the read cache, which the caller
// installs before calling this (see the upload hooks in the VFS). Keeping the
// queue's link meant the bytes of every file ever uploaded stayed on disk for
// the life of the installation: the cache could evict its own link and never
// reclaim anything, because the journal still named the object. The content is
// on the backend and in the cache by now; the queue has no further use for it.
//
// The row's blob_path is cleared in the same transaction that marks it done,
// so a crash between the two leaves an object no row names — which is exactly
// what Recover reclaims.
func (j *Journal) Succeed(ctx context.Context, id string) error {
	var blob string
	err := j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotCancelled(tx, id); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT blob_path FROM uploads WHERE id = ?`, id).Scan(&blob); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("journal: succeed: %w", err)
		}
		if _, err := tx.Exec(`UPDATE uploads SET state = ?, last_error = '', blob_path = '', done_at = ? WHERE id = ?`,
			string(StateDone), unixMilli(j.now()), id); err != nil {
			return fmt.Errorf("journal: succeed: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM upload_parts WHERE upload_id = ?`, id); err != nil {
			return fmt.Errorf("journal: succeed: %w", err)
		}
		return pruneDoneTx(tx, unixMilli(j.now().Add(-doneRetention)))
	})
	if err != nil {
		return err
	}
	// Another row can still be sharing this content — two writes of the same
	// bytes deduplicate onto one object — so this only unlinks when nothing
	// else names it.
	j.removeBlobIfUnreferenced(ctx, blob)
	return nil
}

// pruneDoneTx reclaims completed rows past both bounds, with whatever the
// upload accumulated along the way — an upload that was dead-lettered and
// retried, or cancelled and resumed, leaves rows in those side tables, and
// dropping only the upload row would move the growth rather than stop it.
//
// upload_discarded is deliberately left alone: it is the record that stops a
// discarded upload's ID coming back, not history. A row with an unfinished
// cleanup intent is not a candidate at all — that intent is recovery state.
func pruneDoneTx(tx *sql.Tx, before int64) error {
	rows, err := tx.Query(`SELECT id FROM uploads WHERE state = ? AND done_at > 0 AND done_at < ?
AND id NOT IN (SELECT id FROM uploads WHERE state = ? AND done_at > 0 ORDER BY done_at DESC, id DESC LIMIT ?)
AND id NOT IN (SELECT upload_id FROM upload_cleanup)`,
		string(StateDone), before, string(StateDone), doneHistory)
	if err != nil {
		return fmt.Errorf("journal: prune completed uploads: %w", err)
	}
	var victims []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("journal: prune completed uploads: %w", err)
		}
		victims = append(victims, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("journal: prune completed uploads: %w", err)
	}
	rows.Close()
	for _, id := range victims {
		for _, q := range []string{
			`DELETE FROM uploads WHERE id = ?`,
			`DELETE FROM upload_parts WHERE upload_id = ?`,
			`DELETE FROM dead_letter WHERE upload_id = ?`,
			`DELETE FROM upload_cancellation WHERE upload_id = ?`,
			`DELETE FROM upload_resume_history WHERE upload_id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return fmt.Errorf("journal: prune completed uploads: %w", err)
			}
		}
	}
	return nil
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
		if u.IsMkdir() {
			// Rows queued under this directory address it by this row's
			// id; without the row they could never run and nothing would
			// say why. They are the caller's to deal with first.
			var waiting int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM uploads WHERE remote_parent_id = ? AND state <> ?`,
				LocalIDPrefix+id, string(StateDone)).Scan(&waiting); err != nil {
				return fmt.Errorf("journal: drop: %w", err)
			}
			if waiting > 0 {
				return fmt.Errorf("%w: %d", ErrChildrenQueued, waiting)
			}
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
		if cause := j.verifyBlob(u); cause != nil && !u.IsMkdir() && !u.IsDelete() {
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
	// Blocked counts pending rows that cannot run: they wait on a queued
	// directory whose creation is dead, cancelled or gone, directly or
	// through pending directories in between. They stay pending because
	// their bytes are the only copy, but nothing will move them until the
	// directory's row is retried.
	Blocked int
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
	if err := rows.Err(); err != nil {
		return s, err
	}
	if s.Pending > 0 {
		// The parent is looked up by its own id (the primary key) from the
		// child's local parent id, and children by the parent index, so
		// this is a walk over the queued directories rather than a join of
		// the table to itself on a computed key: that join scanned every
		// row for every row, and a copy of a tree — twenty thousand rows
		// under queued directories — made each Stats take a minute and a
		// half, once a second, for the status page.
		err := j.db.QueryRowContext(ctx, `WITH RECURSIVE blocked(id) AS (
			SELECT u.id FROM uploads u LEFT JOIN uploads p ON p.id = substr(u.remote_parent_id, ?)
			 WHERE u.state = ? AND substr(u.remote_parent_id, 1, ?) = ?
			   AND (p.id IS NULL OR p.state NOT IN (?, ?))
			UNION
			SELECT u.id FROM blocked b JOIN uploads u INDEXED BY uploads_by_parent ON u.remote_parent_id = ? || b.id WHERE u.state = ?
		) SELECT COUNT(*) FROM blocked`,
			len(LocalIDPrefix)+1, string(StatePending), len(LocalIDPrefix), LocalIDPrefix,
			string(StatePending), string(StateUploading), LocalIDPrefix, string(StatePending)).Scan(&s.Blocked)
		if err != nil {
			return s, fmt.Errorf("journal: stats: %w", err)
		}
	}
	return s, nil
}
