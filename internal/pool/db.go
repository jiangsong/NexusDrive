package pool

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// The pool database is an index, never the truth: the members are. Every
// table here can be dropped and rebuilt by listing the members again; what
// is lost by a rebuild is only cheap knowledge (which member holds which
// path, member directory ids) and the opaque ids the local VFS has seen —
// which is why ids live in their own table and survive a rebuild.
const schema = `
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ids (
  id         TEXT PRIMARY KEY,
  path       TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS entries (
  path        TEXT PRIMARY KEY,
  parent      TEXT NOT NULL,
  kind        INTEGER NOT NULL,
  size        INTEGER NOT NULL DEFAULT 0,
  mtime_ns    INTEGER NOT NULL DEFAULT 0,
  ctoken      TEXT NOT NULL DEFAULT '',
  hash_type   TEXT NOT NULL DEFAULT '',
  hash        TEXT NOT NULL DEFAULT '',
  conflict_of TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_parent ON entries(parent);
CREATE TABLE IF NOT EXISTS replicas (
  path      TEXT NOT NULL,
  parent    TEXT NOT NULL,
  member    TEXT NOT NULL,
  remote_id TEXT NOT NULL,
  version   TEXT NOT NULL DEFAULT '',
  size      INTEGER NOT NULL DEFAULT 0,
  mtime_ns  INTEGER NOT NULL DEFAULT 0,
  hash_type TEXT NOT NULL DEFAULT '',
  hash      TEXT NOT NULL DEFAULT '',
  ctoken    TEXT NOT NULL DEFAULT '',
  state     TEXT NOT NULL DEFAULT 'live',
  seen_at   INTEGER NOT NULL,
  -- member_name is the file's real name on the member; it differs from
  -- the last segment of path only for a conflict copy the pool surfaces
  -- under a synthetic name.
  member_name TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path, member)
);
CREATE INDEX IF NOT EXISTS replicas_member ON replicas(member, path);
CREATE INDEX IF NOT EXISTS replicas_parent ON replicas(parent, member);
CREATE INDEX IF NOT EXISTS replicas_remote ON replicas(member, remote_id);
CREATE TABLE IF NOT EXISTS member_dirs (
  member      TEXT NOT NULL,
  path        TEXT NOT NULL,
  parent      TEXT NOT NULL,
  remote_id   TEXT NOT NULL,
  verified_at INTEGER NOT NULL,
  -- listed_at is when this directory's children were last enumerated on
  -- the member. A subdirectory absent from the index while its parent was
  -- enumerated is known not to exist there, and is not asked for.
  listed_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (member, path)
);
CREATE INDEX IF NOT EXISTS member_dirs_parent ON member_dirs(parent, member);
CREATE INDEX IF NOT EXISTS member_dirs_remote ON member_dirs(member, remote_id);
CREATE TABLE IF NOT EXISTS pending_ops (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  member     TEXT NOT NULL,
  op         TEXT NOT NULL,
  path       TEXT NOT NULL,
  parent     TEXT NOT NULL,
  args       TEXT NOT NULL DEFAULT '',
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_at    INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pending_ops_member ON pending_ops(member, state, seq);
CREATE INDEX IF NOT EXISTS pending_ops_parent ON pending_ops(parent, member);
CREATE TABLE IF NOT EXISTS repair_queue (
  path        TEXT PRIMARY KEY,
  reason      TEXT NOT NULL,
  priority    INTEGER NOT NULL DEFAULT 0,
  next_at     INTEGER NOT NULL DEFAULT 0,
  attempts    INTEGER NOT NULL DEFAULT 0,
  source_hint TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS holds (
  hold_path  TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  ctoken     TEXT NOT NULL DEFAULT '',
  size       INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS holds_path ON holds(path);
CREATE TABLE IF NOT EXISTS member_naming (
  member     TEXT NOT NULL,
  pattern    TEXT NOT NULL,
  learned_at INTEGER NOT NULL,
  PRIMARY KEY (member, pattern)
);
CREATE TABLE IF NOT EXISTS divergences (
  path    TEXT NOT NULL,
  member  TEXT NOT NULL,
  kind    TEXT NOT NULL,
  detail  TEXT NOT NULL DEFAULT '',
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (path, member, kind)
);
-- member_usage is what the pool has placed on each member, so free space
-- and rebalance skew cost one row read instead of SUM(size) over every
-- replica. Triggers keep it, not the callers: replicas are deleted from
-- eight places and one forgotten decrement is a number nobody can explain
-- later.
CREATE TABLE IF NOT EXISTS member_usage (
  member TEXT PRIMARY KEY,
  bytes  INTEGER NOT NULL DEFAULT 0,
  files  INTEGER NOT NULL DEFAULT 0
);
CREATE TRIGGER IF NOT EXISTS member_usage_ins AFTER INSERT ON replicas BEGIN
  INSERT INTO member_usage(member, bytes, files) VALUES(new.member, new.size, 1)
    ON CONFLICT(member) DO UPDATE SET bytes = bytes + new.size, files = files + 1;
END;
CREATE TRIGGER IF NOT EXISTS member_usage_del AFTER DELETE ON replicas BEGIN
  UPDATE member_usage SET bytes = MAX(0, bytes - old.size), files = MAX(0, files - 1)
    WHERE member = old.member;
END;
CREATE TRIGGER IF NOT EXISTS member_usage_upd AFTER UPDATE ON replicas BEGIN
  UPDATE member_usage SET bytes = MAX(0, bytes - old.size), files = MAX(0, files - 1)
    WHERE member = old.member;
  INSERT INTO member_usage(member, bytes, files) VALUES(new.member, new.size, 1)
    ON CONFLICT(member) DO UPDATE SET bytes = bytes + new.size, files = files + 1;
END;
`

// schemaVersion is the index layout this build maintains. A database
// written by an older build is migrated on open; the index is a cache, so
// a migration may simply recompute what it needs.
const schemaVersion = 2

func openDB(path string) (*sql.DB, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("pool: %w", err)
		}
		dsn = "file:" + path +
			"?_pragma=journal_mode(WAL)&_txlock=immediate" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("pool: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate brings an index written by an older build up to schemaVersion.
func migrate(db *sql.DB) error {
	var have int
	row := db.QueryRow(`SELECT CAST(v AS INTEGER) FROM meta WHERE k = 'schema_version'`)
	if err := row.Scan(&have); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("pool: schema version: %w", err)
	}
	if have >= schemaVersion {
		return nil
	}
	// v2 added member_usage. An index from v1 has replicas the triggers
	// never saw, so the totals are recomputed once rather than migrated.
	if _, err := db.Exec(`DELETE FROM member_usage`); err != nil {
		return fmt.Errorf("pool: rebuild member_usage: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO member_usage(member, bytes, files)
		SELECT member, COALESCE(SUM(size), 0), COUNT(*) FROM replicas GROUP BY member`); err != nil {
		return fmt.Errorf("pool: rebuild member_usage: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, schemaVersion); err != nil {
		return fmt.Errorf("pool: schema version: %w", err)
	}
	return nil
}

// tx runs fn in one immediate transaction. Whatever it changed, answers
// resolved from the index before it are stale: the replica cache is
// invalidated once the transaction is over (replicaCache).
func (p *Pool) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	defer p.resolveCache.invalidate()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// execIndex runs one statement that changes replicas, entries or ids outside
// a transaction, and invalidates the replica cache as tx does.
func (p *Pool) execIndex(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := p.db.ExecContext(ctx, query, args...)
	p.resolveCache.invalidate()
	return res, err
}

func (p *Pool) metaGet(ctx context.Context, k string) (string, error) {
	var v string
	err := p.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (p *Pool) metaSet(ctx context.Context, k, v string) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
