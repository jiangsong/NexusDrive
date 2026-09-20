package meta

// schemaVersion is bumped whenever migrations are appended. The store applies
// every migration above the recorded version inside one transaction.
const schemaVersion = 15

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
	// v10 -> v11: applied_gen records the parent's listing generation at the
	// moment a change feed wrote this node's attributes. A listing skips any
	// child whose applied_gen is at least its own generation — that write
	// happened after the listing began, so the snapshot is older than it and
	// must not put the previous values back. It is what lets a pure attribute
	// update use the soft fence instead of refusing the listing outright; see
	// listing_fence.go.
	`ALTER TABLE nodes ADD COLUMN applied_gen INTEGER NOT NULL DEFAULT 0;`,
	// v11 -> v12: directories are a few percent of nodes, and three things
	// count or page over them alone: search coverage (every search answer),
	// Stats.Dirs (every status tick) and the crawler's IncompleteDirs. Each
	// was a scan of the whole table, 116 ms at a million nodes. A partial
	// index over directories only costs a row per directory insert and
	// makes all three index-only; kind = 1 is provider.KindDir.
	`CREATE INDEX nodes_dirs ON nodes(ino) WHERE kind = 1;`,
	// v12 -> v13: extended attributes, held locally beside the node.
	//
	// macOS attaches them to everything it copies, and copyfile(3) — the
	// Finder's copy engine — abandons the whole copy when it cannot write
	// one, so a filesystem that refuses them is one the Finder cannot copy
	// into. They are deliberately not sent to the backend: a cloud drive has
	// nowhere to put them, and the alternative the kernel offers (AppleDouble
	// "._" sidecar files) would upload a junk file per copied file and show
	// it on every other device. The trigger keeps them from outliving the
	// node and being inherited by whatever inode the autoincrement reissues.
	`CREATE TABLE xattrs (
  ino   INTEGER NOT NULL,
  name  TEXT    NOT NULL,
  value BLOB    NOT NULL,
  PRIMARY KEY (ino, name)
) WITHOUT ROWID;
CREATE TRIGGER nodes_xattr_delete AFTER DELETE ON nodes BEGIN
  DELETE FROM xattrs WHERE ino=old.ino;
END;`,
	// v13 -> v14: since when the change feed has covered a remote without a
	// gap (unix seconds; 0 = not covered). A listing taken after this
	// instant is kept current by the feed and needs no TTL; the value
	// outlives the process because the cursor does — the first poll after a
	// restart delivers what happened while the daemon was down.
	`ALTER TABLE remote_cursor ADD COLUMN covered_since INTEGER NOT NULL DEFAULT 0;`,
	// v14 -> v15: mode_set marks a node whose permission bits a caller chose
	// rather than the default fillMode hands out.
	//
	// No provider reports a POSIX mode, so every listing carries the default
	// and a refresh would otherwise drag it back over whatever chmod(2) set —
	// an executable script on the mount would lose its exec bit the next time
	// its directory went stale, which is what makes git see a mode-only diff
	// on a checkout that nothing touched. The local database is the only
	// source of permissions here, so a node that has one keeps it until it is
	// deleted; the inode goes with the node, and the replacement starts over
	// at the default.
	`ALTER TABLE nodes ADD COLUMN mode_set INTEGER NOT NULL DEFAULT 0;`,
}
