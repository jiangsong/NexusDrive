// Package agent keeps what CloudFS knows about the agents that talk to it
// over MCP: the principals allowed in, the sessions they open, and an audit
// row per tool call. It is a consumer of the VFS, never a dependency of it,
// and lives in its own SQLite file, <cache.dir>/agent/agent.db, so that the
// journal and the metadata cache never migrate on its account.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is the agent.db layout this build understands.
const schemaVersion = 3

// dbName is the database file inside the store directory.
const dbName = "agent.db"

// watchBuffer is how many events a watcher may fall behind before the oldest
// are dropped. Publishing never blocks: a slow console must not stall a tool
// call.
const watchBuffer = 64

// schemaV1 is the layout the first build shipped. Every time column is Unix
// nanoseconds; zero means "not yet".
var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS principals (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  token_hash TEXT NOT NULL DEFAULT '', token_prefix TEXT NOT NULL DEFAULT '',
  scope TEXT NOT NULL, created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS principals_live_token_name ON principals(name) WHERE kind='token' AND revoked_at=0`,
	`CREATE UNIQUE INDEX IF NOT EXISTS principals_kind_name ON principals(kind, name) WHERE kind IN ('stdio','env','loopback','console')`,
	`CREATE INDEX IF NOT EXISTS principals_token_hash ON principals(token_hash) WHERE token_hash != ''`,
	`CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, principal_id TEXT NOT NULL, conn_key TEXT NOT NULL,
  client_name TEXT NOT NULL DEFAULT '', client_version TEXT NOT NULL DEFAULT '',
  transport TEXT NOT NULL, scope TEXT NOT NULL, workspace TEXT NOT NULL DEFAULT '',
  sandbox INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
  started_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0, summary TEXT NOT NULL DEFAULT '',
  artifacts TEXT NOT NULL DEFAULT '[]')`,
	`CREATE INDEX IF NOT EXISTS sessions_conn_active ON sessions(conn_key) WHERE state='active'`,
	`CREATE INDEX IF NOT EXISTS sessions_started ON sessions(started_at DESC, id)`,
	`CREATE INDEX IF NOT EXISTS sessions_workspace ON sessions(workspace) WHERE workspace != ''`,
	`CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  principal_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
  transport TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL,
  paths TEXT NOT NULL, args TEXT NOT NULL,
  bytes_in INTEGER NOT NULL DEFAULT 0, bytes_out INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', duration_ms INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts)`,
	`CREATE INDEX IF NOT EXISTS audit_session ON audit(session_id, id)`,
}

// schemaV2 adds the two tables session rollback and triggers share, per
// docs/agent-roadmap.md §4.3. session_ops holds one row per write a tool made
// inside a session, with the state the path had before it (the preimage);
// trigger_deliveries is the durable queue of rule matches waiting to run.
// Both are additive, so a v1 file migrates in place and keeps its rows.
var schemaV2 = []string{
	`CREATE TABLE IF NOT EXISTS session_ops (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
  audit_id INTEGER NOT NULL DEFAULT 0,
  op TEXT NOT NULL, path TEXT NOT NULL, to_path TEXT NOT NULL DEFAULT '',
  pre_state TEXT NOT NULL, pre_remote TEXT NOT NULL DEFAULT '',
  pre_remote_id TEXT NOT NULL DEFAULT '', pre_version TEXT NOT NULL DEFAULT '',
  pre_size INTEGER NOT NULL DEFAULT 0, pre_hash TEXT NOT NULL DEFAULT '',
  pre_blob TEXT NOT NULL DEFAULT '', pre_reason TEXT NOT NULL DEFAULT '',
  post_version TEXT NOT NULL DEFAULT '',
  rolled_back INTEGER NOT NULL DEFAULT 0, rollback_result TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS session_ops_path ON session_ops(path)`,
	`CREATE INDEX IF NOT EXISTS session_ops_session ON session_ops(session_id, seq)`,
	`CREATE TABLE IF NOT EXISTS trigger_deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT, rule TEXT NOT NULL, path TEXT NOT NULL,
  kind TEXT NOT NULL, origin TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL, due_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '', output TEXT NOT NULL DEFAULT '',
  done_at INTEGER NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS trigger_pending ON trigger_deliveries(rule, path) WHERE state='pending'`,
}

// schemaV3 is the agent-first layout (docs/agent-first-design.md §4.3).
// audit.tokens_out keeps the token estimate of every result beside its
// bytes; session_ops.ts lets a row be placed in time without joining
// audit; changes is the durable record of every change the VFS reported,
// whatever its origin (kernel, MCP, control plane, WebDAV, remote), which
// pull_events and last_writer read; read_heat counts reads per path and
// day by actor kind, never by identity; sessions.last_change_seen is the
// cursor a session's pull_events resumes from. All additive.
var schemaV3 = []string{
	`ALTER TABLE audit ADD COLUMN tokens_out INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE session_ops ADD COLUMN ts INTEGER NOT NULL DEFAULT 0`,
	// A v2 row's time is the time of the audit row it was recorded
	// beside; the backfill only touches rows still at the default, so
	// it is repeatable.
	`UPDATE session_ops SET ts = COALESCE((SELECT audit.ts FROM audit WHERE audit.id = session_ops.audit_id), 0) WHERE ts = 0`,
	`ALTER TABLE sessions ADD COLUMN last_change_seen INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS changes (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  path TEXT NOT NULL, kind TEXT NOT NULL, origin TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '', principal TEXT NOT NULL DEFAULT '',
  reliable INTEGER NOT NULL DEFAULT 1, from_path TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS changes_path ON changes(path, id)`,
	`CREATE INDEX IF NOT EXISTS changes_ts ON changes(ts)`,
	`CREATE TABLE IF NOT EXISTS read_heat (
  path TEXT NOT NULL, day INTEGER NOT NULL, actor_kind TEXT NOT NULL,
  count INTEGER NOT NULL DEFAULT 0, last_ts INTEGER NOT NULL,
  PRIMARY KEY (path, day, actor_kind))`,
	`CREATE INDEX IF NOT EXISTS read_heat_day ON read_heat(day)`,
}

// migrations lists every layout in order; migrate applies the ones above the
// file's current version. Each step is idempotent (IF NOT EXISTS, and an
// ALTER TABLE ... ADD COLUMN is skipped when the column is there), so a
// crash between a step and the version write is repaired by the next open.
var migrations = [][]string{schemaV1, schemaV2, schemaV3}

// addColumnRE matches the one DDL form SQLite cannot make idempotent by
// itself.
var addColumnRE = regexp.MustCompile(`(?i)^ALTER TABLE (\w+) ADD COLUMN (\w+) `)

// applyStep runs one schema statement, skipping an ADD COLUMN whose column
// already exists.
func applyStep(tx *sql.Tx, q string) error {
	if m := addColumnRE.FindStringSubmatch(q); m != nil {
		rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, m[1]))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				return err
			}
			if strings.EqualFold(name, m[2]) {
				return nil
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	_, err := tx.Exec(q)
	return err
}

// Store is the open agent.db.
type Store struct {
	db   *sql.DB
	lock *os.File
	// dir is the store directory, <cache.dir>/agent, where the heartbeats
	// of stdio processes beside the owner live too.
	dir string
	// owner reports whether this process holds the flock and therefore runs
	// housekeeping. Non-owners still write: a stdio MCP process appends its
	// own audit rows while the daemon holds the lock.
	owner    bool
	readOnly bool
	now      func() time.Time

	watchMu  sync.Mutex
	watchers map[chan Event]struct{}

	// auditFailures counts rows that could not be appended, so that a
	// broken audit trail surfaces in status instead of being silent.
	auditFailures atomic.Int64
	// appendFault, when set, is a test hook that makes the next append fail.
	appendFault func() error
}

// Open creates or opens agent.db under dir, which is <cache.dir>/agent.
// The first opener takes the flock and becomes the owner; a later opener
// is not the owner but may still read and write, WAL mode serialising the
// two processes.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("agent: a store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	db, err := openDB(dir, false)
	if err != nil {
		return nil, err
	}
	lock, owner, err := acquireOwnership(dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := newStore(db, lock, owner, false)
	s.dir = dir
	if err := s.migrate(); err != nil {
		releaseOwnership(lock)
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing agent.db without taking the lock and
// without the ability to write. A missing database is reported as
// os.ErrNotExist so a CLI can say "no daemon has run yet" rather than create
// an empty store of its own.
func OpenReadOnly(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("agent: a store directory is required")
	}
	if _, err := os.Stat(filepath.Join(dir, dbName)); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	db, err := openDB(dir, true)
	if err != nil {
		return nil, err
	}
	s := newStore(db, nil, false, true)
	s.dir = dir
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("agent: %w", err)
	}
	if version > schemaVersion {
		db.Close()
		return nil, newerSchemaError(version)
	}
	return s, nil
}

func openDB(dir string, readOnly bool) (*sql.DB, error) {
	dsn := "file:" + filepath.Join(dir, dbName) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	if readOnly {
		dsn += "&mode=ro"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	db.SetMaxOpenConns(4)
	return db, nil
}

func newStore(db *sql.DB, lock *os.File, owner, readOnly bool) *Store {
	return &Store{
		db:       db,
		lock:     lock,
		owner:    owner,
		readOnly: readOnly,
		now:      time.Now,
		watchers: make(map[chan Event]struct{}),
	}
}

func newerSchemaError(version int) error {
	return fmt.Errorf("agent: agent.db is at schema v%d, newer than this build's v%d", version, schemaVersion)
}

// Owner reports whether this process holds the lock and runs housekeeping.
func (s *Store) Owner() bool { return s.owner }

// Dir is the directory the store was opened in.
func (s *Store) Dir() string { return s.dir }

// SchemaVersions reports the layout the file is at and the one this build
// understands. Open refuses a newer file, so the two agree on a store that
// opened; doctor reads them so the number is on the diagnostics page rather
// than only in an error nobody sees until the refusal.
func (s *Store) SchemaVersions(ctx context.Context) (file, build int, err error) {
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&file); err != nil {
		return 0, schemaVersion, fmt.Errorf("agent: %w", err)
	}
	return file, schemaVersion, nil
}

// Close releases the database and the ownership lock. Watchers are closed
// so a console loop ranging over them terminates.
func (s *Store) Close() error {
	s.watchMu.Lock()
	for ch := range s.watchers {
		delete(s.watchers, ch)
		close(ch)
	}
	s.watchMu.Unlock()
	err := s.db.Close()
	releaseOwnership(s.lock)
	s.lock = nil
	return err
}

// migrate brings the database to schemaVersion, applying the steps the file
// has not seen yet. The version check happens inside the immediate
// transaction so that two processes opening an empty file at once cannot
// both decide to create it: the second one waits on the write lock and then
// sees the first one's version.
func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version > schemaVersion {
		return newerSchemaError(version)
	}
	for _, step := range migrations[version:] {
		for _, q := range step {
			if err := applyStep(tx, q); err != nil {
				return fmt.Errorf("agent: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, fmt.Sprint(schemaVersion)); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return tx.Commit()
}

// Watch subscribes to events. The channel holds watchBuffer events; when a
// subscriber falls further behind, the oldest event is dropped so that the
// publisher never waits. The returned function unsubscribes and closes the
// channel.
func (s *Store) Watch() (<-chan Event, func()) {
	ch := make(chan Event, watchBuffer)
	s.watchMu.Lock()
	s.watchers[ch] = struct{}{}
	s.watchMu.Unlock()
	stop := func() {
		s.watchMu.Lock()
		defer s.watchMu.Unlock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
	}
	return ch, stop
}

// publish delivers e to every watcher without blocking. It holds watchMu for
// the whole delivery so that stop cannot close a channel mid-send.
func (s *Store) publish(e Event) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for ch := range s.watchers {
		for {
			select {
			case ch <- e:
			default:
				// Full: drop the oldest and retry. The channel cannot be
				// closed here because stop needs watchMu.
				select {
				case <-ch:
				default:
				}
				continue
			}
			break
		}
	}
}
