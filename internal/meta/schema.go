package meta

// schemaVersion is bumped whenever migrations are appended. The store applies
// every migration above the recorded version inside one transaction.
const schemaVersion = 10

// migrations[i] upgrades the database from version i to i+1.
var migrations = []string{
	// v0 -> v1: initial tree, directory state, negative cache, delta cursors,
	// pins and the filename index (docs/DESIGN.md §4.3).
	`
CREATE TABLE nodes (
  ino        INTEGER PRIMARY KEY AUTOINCREMENT,
  parent_ino INTEGER NOT NULL,
  name       TEXT    NOT NULL,
  kind       INTEGER NOT NULL,
  size       INTEGER NOT NULL DEFAULT 0,
  mtime_ns   INTEGER NOT NULL DEFAULT 0,
  mode       INTEGER NOT NULL DEFAULT 0,
  remote     TEXT    NOT NULL DEFAULT '',
  remote_id  TEXT    NOT NULL DEFAULT '',
  version    TEXT    NOT NULL DEFAULT '',
  hash_type  TEXT    NOT NULL DEFAULT '',
  hash       TEXT    NOT NULL DEFAULT '',
  fetched_at INTEGER NOT NULL DEFAULT 0,
  ttl_s      INTEGER NOT NULL DEFAULT 0,
  dirty      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX nodes_parent_name ON nodes(parent_ino, name);
CREATE INDEX nodes_remote_id ON nodes(remote, remote_id);

CREATE TABLE dir_state (
  ino       INTEGER PRIMARY KEY,
  complete  INTEGER NOT NULL DEFAULT 0,
  listed_at INTEGER NOT NULL DEFAULT 0,
  cursor    TEXT    NOT NULL DEFAULT '',
  dirty     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE absent (
  parent_ino INTEGER NOT NULL,
  name       TEXT    NOT NULL,
  until      INTEGER NOT NULL,
  PRIMARY KEY (parent_ino, name)
);

CREATE TABLE remote_cursor (
  remote       TEXT PRIMARY KEY,
  delta_cursor TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE pins (
  path      TEXT PRIMARY KEY,
  recursive INTEGER NOT NULL DEFAULT 0,
  mode      TEXT    NOT NULL DEFAULT 'keep'
);

CREATE VIRTUAL TABLE name_index USING fts5(
  name,
  path UNINDEXED,
  ino  UNINDEXED,
  tokenize='trigram'
);
`,
	// v1 -> v2: track the last version actually observed on the remote,
	// separately from the node's current version. A local write moves version
	// to a local-only token while remote_version keeps pointing at what the
	// server last had, which is what conflict detection must compare against.
	`ALTER TABLE nodes ADD COLUMN remote_version TEXT NOT NULL DEFAULT '';`,
	// v2 -> v3: names waiting to enter the FTS index. A directory listing
	// writes here in one statement and a background pass folds the rows into
	// name_index, so indexing cost leaves the readdir path. Search consults
	// both tables, so nothing is invisible while it waits.
	`CREATE TABLE name_index_pending (
  ino  INTEGER PRIMARY KEY,
  name TEXT    NOT NULL
);`,
	// v3 -> v4: the negative cache moved into memory; the table only cost a
	// write transaction per create.
	`DROP TABLE IF EXISTS absent;`,
	// v4 -> v5: name_indexed mirrors what the FTS table holds, keyed by ino,
	// so a directory refresh can queue only the names that actually changed
	// and FTS rows can be addressed by rowid. Every refresh used to re-queue
	// every child, and each was a full scan of the FTS table to delete plus
	// a segment merge to insert — thousands of times per refresh.
	`CREATE TABLE name_indexed (
  ino  INTEGER PRIMARY KEY,
  name TEXT    NOT NULL
);
INSERT OR REPLACE INTO name_indexed (ino, name) SELECT ino, name FROM name_index WHERE ino > 0;
DELETE FROM name_index;
INSERT INTO name_index (rowid, name, path, ino) SELECT ino, name, '', ino FROM name_indexed;`,
	// v5 -> v6: a copy-created inode and its intent are bound atomically.
	// Bindings survive unlink so recovery cannot recreate deleted targets.
	`CREATE TABLE copy_bindings (copy_id TEXT PRIMARY KEY, ino INTEGER NOT NULL UNIQUE);
CREATE TABLE store_identity (id TEXT NOT NULL);
INSERT INTO store_identity(id) VALUES (lower(hex(randomblob(16))));`,
	// v6 -> v7: force a real FULL-synchronous WAL write before deleting a
	// payload whose last metadata reference was removed by a NORMAL write.
	`CREATE TABLE copy_cleanup_fence (id INTEGER PRIMARY KEY CHECK(id=1), value INTEGER NOT NULL CHECK(value IN (0,1)));
INSERT INTO copy_cleanup_fence(id,value) VALUES(1,0);`,
	// v7 -> v8: transactional one/two-rune postings. Trigram FTS cannot
	// answer these queries. Triggers also keep deferred names current when
	// renamed, including writes through copy binding/publication transactions.
	`CREATE VIRTUAL TABLE short_name_index USING fts5(terms, tokenize='ascii');
INSERT INTO short_name_index(rowid,terms)
  SELECT ino,cloudfs_short_terms(name) FROM nodes WHERE ino != 1;
INSERT OR REPLACE INTO name_index_pending(ino,name)
  SELECT n.ino,n.name FROM nodes n LEFT JOIN name_indexed i ON i.ino=n.ino
  WHERE n.ino!=1 AND (i.ino IS NULL OR i.name!=n.name);
CREATE TRIGGER nodes_search_insert AFTER INSERT ON nodes WHEN new.ino != 1 BEGIN
  INSERT INTO short_name_index(rowid,terms) VALUES(new.ino,cloudfs_short_terms(new.name));
  INSERT OR REPLACE INTO name_index_pending(ino,name) VALUES(new.ino,new.name);
END;
CREATE TRIGGER nodes_search_rename AFTER UPDATE OF name ON nodes WHEN new.name != old.name AND new.ino != 1 BEGIN
  DELETE FROM short_name_index WHERE rowid=old.ino;
  INSERT INTO short_name_index(rowid,terms) VALUES(new.ino,cloudfs_short_terms(new.name));
  INSERT OR REPLACE INTO name_index_pending(ino,name) VALUES(new.ino,new.name);
END;
CREATE TRIGGER nodes_search_delete AFTER DELETE ON nodes WHEN old.ino != 1 BEGIN
  DELETE FROM short_name_index WHERE rowid=old.ino;
  DELETE FROM name_index_pending WHERE ino=old.ino;
END;`,
	// v8 -> v9: a later refresh fences an older in-flight provider listing.
	`CREATE TABLE directory_refresh_generation (ino INTEGER PRIMARY KEY, generation INTEGER NOT NULL);
CREATE TRIGGER nodes_refresh_delete AFTER DELETE ON nodes BEGIN
  DELETE FROM directory_refresh_generation WHERE ino=old.ino;
END;`,
	// v9 -> v10: separate "a name here was removed or moved" from "this
	// directory is stale". Only the first has to refuse an older listing;
	// see listing_fence.go for why conflating them refused listings that
	// were perfectly safe to publish.
	`ALTER TABLE directory_refresh_generation ADD COLUMN stale_generation INTEGER NOT NULL DEFAULT 0;`,
}
