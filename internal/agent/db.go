// Package agent keeps what CloudFS knows about the agents that talk to it
// over MCP: the principals allowed in, the sessions they open, and an audit
// row per tool call. It is a consumer of the VFS, never a dependency of it,
// and lives in its own SQLite file, <cache.dir>/agent/agent.db, so that the
// journal and the metadata cache never migrate on its account.
package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is the agent.db layout this build understands.
const schemaVersion = 1

// dbName is the database file inside the store directory.
const dbName = "agent.db"

// watchBuffer is how many events a watcher may fall behind before the oldest
// are dropped. Publishing never blocks: a slow console must not stall a tool
// call.
const watchBuffer = 64

// schemaV1 is applied in one transaction by the first opener. Every time
// column is Unix nanoseconds; zero means "not yet".
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

// Store is the open agent.db.
type Store struct {
	db   *sql.DB
	lock *os.File
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

// migrate brings the database to schemaVersion. The version check happens
// inside the immediate transaction so that two processes opening an empty
// file at once cannot both decide to create it: the second one waits on the
// write lock and then sees the first one's version.
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
	for _, q := range schemaV1 {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("agent: %w", err)
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
