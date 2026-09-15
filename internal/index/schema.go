package index

// schemaVersion is the index.db layout this build writes. It is recorded
// both in PRAGMA user_version and in index_meta('schema_version'); a
// database at a newer version is refused rather than reinterpreted.
const schemaVersion = 2

// migrations[i] carries the database from schema version i to i+1. Every
// step runs inside one transaction together with the version bump, so a
// crash mid-migration leaves the old version intact. Later versions must
// only append statements (docs/agent-roadmap.md §3.6: v2 adds tables, it
// does not alter them).
var migrations = [][]string{
	// v0 -> v1: the phase-1 content index (docs/agent-roadmap.md §3.6).
	//
	// documents is keyed by (remote, remote_id): the path is display and
	// scope data only, so a directory rename is one UPDATE and never a
	// re-extraction. chunks carries no path at all; a hit joins back to its
	// document and the caller maps (remote, remote_id) onto the live meta
	// node. chunks_fts is an external-content FTS5 table over chunks so the
	// text is stored once; the three triggers keep it in step.
	{
		`CREATE TABLE IF NOT EXISTS index_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS rules (
  path TEXT PRIMARY KEY,
  include TEXT NOT NULL,
  exclude TEXT NOT NULL,
  max_file_size INTEGER NOT NULL,
  source TEXT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS documents (
  id INTEGER PRIMARY KEY,
  remote TEXT NOT NULL,
  remote_id TEXT NOT NULL,
  version TEXT NOT NULL,
  path TEXT NOT NULL,
  ino INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  mtime_ns INTEGER NOT NULL DEFAULT 0,
  text TEXT NOT NULL DEFAULT '',
  text_hash BLOB NOT NULL DEFAULT x'',
  truncated INTEGER NOT NULL DEFAULT 0,
  extractor_ver INTEGER NOT NULL DEFAULT 0,
  chunker_ver INTEGER NOT NULL DEFAULT 0,
  state INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  indexed_at INTEGER NOT NULL,
  UNIQUE(remote, remote_id)
)`,
		`CREATE INDEX IF NOT EXISTS documents_path ON documents(path)`,
		`CREATE INDEX IF NOT EXISTS documents_state ON documents(state)`,
		`CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY,
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  start_off INTEGER NOT NULL,
  end_off INTEGER NOT NULL,
  heading TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL,
  UNIQUE(doc_id, seq)
)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  text, heading, content='chunks', content_rowid='id', tokenize='trigram'
)`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO chunks_fts(rowid, text, heading) VALUES (new.id, new.text, new.heading);
END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text, heading) VALUES ('delete', old.id, old.text, old.heading);
END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text, heading) VALUES ('delete', old.id, old.text, old.heading);
  INSERT INTO chunks_fts(rowid, text, heading) VALUES (new.id, new.text, new.heading);
END`,
		`CREATE TABLE IF NOT EXISTS index_pending (
  ino INTEGER PRIMARY KEY,
  path TEXT NOT NULL,
  reason INTEGER NOT NULL,
  queued_at INTEGER NOT NULL
)`,
	},
	// v1 -> v2: vectors and the embedding queue (docs/agent-roadmap.md
	// §3.6, T-39). Only tables are added.
	//
	// vectors holds one L2-normalised vector per embedded chunk, int8 with
	// a per-vector scale by default or float32 LE with quantize none; it
	// cascades with the chunk. embed_pending is the queue the embed worker
	// drains; every writer that deletes chunks deletes their queue rows
	// in the same transaction, and the worker's prepare pass drops any
	// orphan a crash left. The model index makes count(*) a small-index
	// walk, which the max_chunks check runs on every upsert.
	{
		`CREATE TABLE IF NOT EXISTS vectors (
  chunk_id INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
  model TEXT NOT NULL,
  scale REAL NOT NULL,
  vec BLOB NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS vectors_model ON vectors(model)`,
		`CREATE TABLE IF NOT EXISTS embed_pending (
  chunk_id INTEGER PRIMARY KEY,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL DEFAULT 0
)`,
	},
}
