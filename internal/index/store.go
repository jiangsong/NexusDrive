// Package index holds the content index (docs/agent-roadmap.md §3): the
// extracted text of selected files, cut into chunks, searchable through an
// FTS5 trigram index and, once an embedder has seen them, by cosine over
// int8 vectors (vectors.go, hybrid.go). It lives in its own SQLite database, index.db, next to
// the metadata store rather than inside it: meta has a single writer that
// FLUSH waits on, and indexing must never sit in that path.
//
// A document is identified by (remote, remote_id); its version says which
// remote revision the text came from. Paths are stored only on documents,
// for display and scope filtering, so renaming a directory is one UPDATE
// and no chunk is touched. Chunks map back to the live tree through their
// document's (remote, remote_id).
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloudfs/internal/textract"

	_ "modernc.org/sqlite"
)

// dbName is the database file under the store directory.
const dbName = "index.db"

// identityKey is the index_meta row holding the meta store identity the
// index was built against. A different identity means different inodes and
// possibly different remote ids, so the index is rebuilt.
const identityKey = "meta_store_identity"

// maxErrorBytes bounds the failure message kept per document.
const maxErrorBytes = 1024

// defaultTextPage is the page size Text uses when the caller passes none.
const defaultTextPage = 64 << 10

// DocState is the lifecycle of an indexed document.
type DocState int

const (
	// DocOK means text and chunks reflect Document.Version.
	DocOK DocState = iota
	// DocDirty means the document must be re-extracted (retry after a
	// failure, or a newer extractor/chunker).
	DocDirty
	// DocFailed means extraction failed; Document.Error says why.
	DocFailed
)

// Document is one indexed file. IndexedAt is when its text was last
// extracted or re-verified; a rename does not move it.
type Document struct {
	ID        int64
	Remote    string
	RemoteID  string
	Version   string
	Path      string
	Ino       uint64
	Kind      string
	Size      int64
	MTimeNS   int64
	TextHash  []byte
	Truncated bool
	State     DocState
	Error     string
	IndexedAt time.Time
}

// Rule selects a subtree for indexing. Source records where it came from:
// "config" rules mirror the configuration file and can only be removed
// there; "builtin" rules follow another part of the configuration (the
// memory root of memory.root, docs/agent-roadmap.md §3.11) and are just as
// fixed; "ui" and "tool" rules were added at run time and persist here.
type Rule struct {
	Path        string   `json:"path"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	MaxFileSize int64    `json:"max_file_size"`
	Source      string   `json:"source"` // config | builtin | ui | tool
}

// The rule sources that follow the configuration rather than a run-time
// request.
const (
	SourceConfig  = "config"
	SourceBuiltin = "builtin"
)

// managed reports whether a rule source is owned by the configuration.
func managed(source string) bool { return source == SourceConfig || source == SourceBuiltin }

// PendingReason says why a file waits in index_pending.
type PendingReason int

const (
	// PendingChange: a change event named the file.
	PendingChange PendingReason = iota
	// PendingReconcile: a reconcile pass found it new or at another version.
	PendingReconcile
	// PendingRetry: a failed document was asked to try again.
	PendingRetry
)

// PendingItem is one queued file.
type PendingItem struct {
	Ino      uint64
	Path     string
	Reason   PendingReason
	QueuedAt time.Time
}

// Stats is the index summary shown by index_status.
type Stats struct {
	DocsOK, DocsDirty, DocsFailed int
	Chunks                        int
	TextBytes                     int64
	Pending                       int
	// Vectors is how many chunks have an embedding; EmbedPending how many
	// wait for one.
	Vectors      int
	EmbedPending int
}

// FailedDoc is one entry of the failure list.
type FailedDoc struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Error     string    `json:"error"`
	IndexedAt time.Time `json:"indexed_at"`
}

// TextPage is one window of a document's extracted text. Offsets are byte
// offsets into the text; NextOffset always sits on a rune boundary.
type TextPage struct {
	Path       string `json:"path"`
	Version    string `json:"version"`
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Kind       string `json:"kind"`
}

var (
	// ErrConfigRule is returned when a rule mirrored from the configuration
	// file is removed or overridden through the store.
	ErrConfigRule = errors.New("index: this rule comes from the configuration file; remove it there")
	// ErrBuiltinRule is returned when a built-in rule (the memory root's)
	// is removed or overridden through the store.
	ErrBuiltinRule = errors.New("index: this rule is built in for memory.root and follows the configuration file")
	// ErrNotIndexed is returned by Text for a path with no document.
	ErrNotIndexed = errors.New("index: this file is not indexed")
	// ErrNoRule is returned when removing a rule that does not exist.
	ErrNoRule = errors.New("index: no such rule")
	// ErrReadOnly is returned by every writer of a store opened with
	// OpenStoreReadOnly.
	ErrReadOnly = errors.New("index: the index was opened read-only")
)

// Store is index.db.
type Store struct {
	db   *sql.DB
	lock *os.File
	// owner reports whether this process holds the flock and so may run
	// the indexer. Everyone else may read, and may write on the indexer's
	// behalf (rules, pending) exactly as the export store allows.
	owner    bool
	readOnly bool
	now      func() time.Time
	// writeMu serialises writers. SQLite would serialise them anyway, with
	// SQLITE_BUSY instead of a queue.
	writeMu sync.Mutex
	vectorStore
}

// OpenStore creates or opens <dir>/index.db, migrating it to this build's
// schema. A database written by a newer build is refused.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("index: a store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	// foreign_keys is on so deleting a document cascades to its chunks
	// (and, from v2 on, to their vectors).
	dsn := "file:" + filepath.Join(dir, dbName) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	db.SetMaxOpenConns(4)
	lock, owner, err := acquireOwnership(dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, lock: lock, owner: owner, now: time.Now}
	if owner {
		err = s.migrate()
	} else {
		var current int
		err = db.QueryRow(`PRAGMA user_version`).Scan(&current)
		if err == nil && current != schemaVersion {
			err = fmt.Errorf("index: schema v%d requires its owner for migration; connect to the running daemon", current)
		}
	}
	if err != nil {
		releaseOwnership(lock)
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenStoreReadOnly opens an existing index.db without taking ownership,
// migrating or writing. It returns os.ErrNotExist (wrapped) when there is
// no index yet, and refuses a schema newer than this build's.
func OpenStoreReadOnly(dir string) (*Store, error) {
	p, err := filepath.Abs(filepath.Join(dir, dbName))
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	db.SetMaxOpenConns(4)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: %w", err)
	}
	if version > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("index: index.db is at schema v%d, newer than this build's v%d", version, schemaVersion)
	}
	return &Store{db: db, readOnly: true, now: time.Now}, nil
}

// Owner reports whether this process may run the indexer.
func (s *Store) Owner() bool { return s.owner }

// Close releases the database and the ownership lock.
func (s *Store) Close() error {
	err := s.db.Close()
	releaseOwnership(s.lock)
	s.lock = nil
	return err
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version > schemaVersion {
		return fmt.Errorf("index: index.db is at schema v%d, newer than this build's v%d", version, schemaVersion)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer tx.Rollback()
	for v := version; v < schemaVersion; v++ {
		for _, q := range migrations[v] {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("index: migrate v%d: %w", v+1, err)
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO index_meta(key, value) VALUES('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(schemaVersion)); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return tx.Commit()
}

// write runs fn inside one immediate transaction, serialised with every
// other writer of this Store.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// IntegrityCheck runs PRAGMA integrity_check and returns its first line,
// "ok" for a healthy database.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var out string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&out); err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	return out, nil
}

// Identity returns the meta store identity the index was built against, ""
// before EnsureIdentity ever ran.
func (s *Store) Identity(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, identityKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	return id, nil
}

// EnsureIdentity binds the index to the meta store it was built from. A
// fresh index records metaIdentity and reports reset == false; the same
// identity is a no-op; a different one (meta was rebuilt, so inodes and
// possibly remote ids changed) wipes documents, chunks and the queue, keeps
// the rules, records the new identity and reports reset == true so the
// indexer knows to rebuild.
func (s *Store) EnsureIdentity(ctx context.Context, metaIdentity string) (reset bool, err error) {
	err = s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, identityKey).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("index: %w", err)
		}
		if err == nil && current == metaIdentity {
			return nil
		}
		if err == nil {
			if err := resetTx(ctx, tx, ch); err != nil {
				return err
			}
			reset = true
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, identityKey, metaIdentity)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		return nil
	})
	return reset, err
}

// Reset drops every document, chunk, vector and queued item. Rules, the
// recorded identity and the recorded embedding model stay: a rebuild
// re-walks the same rules and embeds with the same model.
func (s *Store) Reset(ctx context.Context) error {
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error { return resetTx(ctx, tx, ch) })
}

func resetTx(ctx context.Context, tx *sql.Tx, ch *vecChange) error {
	if err := dropAllVectorsTx(ctx, tx, ch); err != nil {
		return err
	}
	// chunks first so the delete trigger sees every row; the FK cascade
	// would do the same, explicitly is clearer.
	for _, q := range []string{`DELETE FROM chunks`, `DELETE FROM documents`, `DELETE FROM index_pending`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	return nil
}

// UpsertDocument records d's extracted text and replaces its chunks in one
// transaction. The document is keyed by (Remote, RemoteID); a second call
// for the same key updates the row (state ok, error cleared, IndexedAt now)
// and drops the old chunks, their vectors and their queue rows before
// inserting the new ones, so the FTS index never holds both versions. The
// new chunks are queued for embedding as far as the embed cap allows. It
// returns the document id.
func (s *Store) UpsertDocument(ctx context.Context, d Document, text string, chunks []textract.Chunk) (int64, error) {
	if d.Remote == "" || d.RemoteID == "" {
		return 0, errors.New("index: a document needs a remote and a remote id")
	}
	sum := sha256.Sum256([]byte(text))
	var id int64
	err := s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		err := tx.QueryRowContext(ctx, `INSERT INTO documents
			(remote, remote_id, version, path, ino, kind, size, mtime_ns, text, text_hash, truncated,
			 extractor_ver, chunker_ver, state, error, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?)
			ON CONFLICT(remote, remote_id) DO UPDATE SET
			 version = excluded.version, path = excluded.path, ino = excluded.ino, kind = excluded.kind,
			 size = excluded.size, mtime_ns = excluded.mtime_ns, text = excluded.text,
			 text_hash = excluded.text_hash, truncated = excluded.truncated,
			 extractor_ver = excluded.extractor_ver, chunker_ver = excluded.chunker_ver,
			 state = excluded.state, error = '', indexed_at = excluded.indexed_at
			RETURNING id`,
			d.Remote, d.RemoteID, d.Version, d.Path, int64(d.Ino), d.Kind, d.Size, d.MTimeNS,
			text, sum[:], boolInt(d.Truncated), textract.ExtractorVer, textract.ChunkerVer,
			int(DocOK), s.now().UnixNano()).Scan(&id)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if err := dropDocVectorsTx(ctx, tx, id, ch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE doc_id = ?`, id); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if len(chunks) == 0 {
			return nil
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks(doc_id, seq, start_off, end_off, heading, text)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		defer stmt.Close()
		for _, c := range chunks {
			if _, err := stmt.ExecContext(ctx, id, c.Seq, c.StartOff, c.EndOff, c.Heading, c.Text); err != nil {
				return fmt.Errorf("index: chunk %d: %w", c.Seq, err)
			}
		}
		_, err = s.queueChunksTx(ctx, tx, id)
		return err
	})
	return id, err
}

// TouchVersion records that the document's text at a new remote version
// hashes the same as what is stored: the version moves, the state returns
// to ok, and nothing is re-chunked.
func (s *Store) TouchVersion(ctx context.Context, id int64, version string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE documents SET version = ?, state = ?, error = '', indexed_at = ?
			WHERE id = ?`, version, int(DocOK), s.now().UnixNano(), id)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotIndexed
		}
		return nil
	})
}

// MarkFailed records that extracting d failed. The row is created or
// updated in place with state failed and the cause; any text and chunks of
// an earlier version are dropped so the failure is not masked by stale hits.
func (s *Store) MarkFailed(ctx context.Context, d Document, cause error) error {
	if d.Remote == "" || d.RemoteID == "" {
		return errors.New("index: a document needs a remote and a remote id")
	}
	msg := ""
	if cause != nil {
		msg = truncateRunes(cause.Error(), maxErrorBytes)
	}
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO documents
			(remote, remote_id, version, path, ino, kind, size, mtime_ns, text, text_hash, truncated,
			 extractor_ver, chunker_ver, state, error, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', x'', 0, ?, ?, ?, ?, ?)
			ON CONFLICT(remote, remote_id) DO UPDATE SET
			 version = excluded.version, path = excluded.path, ino = excluded.ino, kind = excluded.kind,
			 size = excluded.size, mtime_ns = excluded.mtime_ns, text = '', text_hash = x'', truncated = 0,
			 extractor_ver = excluded.extractor_ver, chunker_ver = excluded.chunker_ver,
			 state = excluded.state, error = excluded.error, indexed_at = excluded.indexed_at
			RETURNING id`,
			d.Remote, d.RemoteID, d.Version, d.Path, int64(d.Ino), d.Kind, d.Size, d.MTimeNS,
			textract.ExtractorVer, textract.ChunkerVer, int(DocFailed), msg, s.now().UnixNano()).Scan(&id)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if err := dropDocVectorsTx(ctx, tx, id, ch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE doc_id = ?`, id); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		return nil
	})
}

const documentColumns = `id, remote, remote_id, version, path, ino, kind, size, mtime_ns, text_hash, truncated, state, error, indexed_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanDocument(r rowScanner) (Document, error) {
	var (
		d         Document
		ino       int64
		truncated int
		state     int
		indexedAt int64
	)
	err := r.Scan(&d.ID, &d.Remote, &d.RemoteID, &d.Version, &d.Path, &ino, &d.Kind, &d.Size, &d.MTimeNS,
		&d.TextHash, &truncated, &state, &d.Error, &indexedAt)
	if err != nil {
		return Document{}, err
	}
	d.Ino = uint64(ino)
	d.Truncated = truncated != 0
	d.State = DocState(state)
	d.IndexedAt = time.Unix(0, indexedAt)
	return d, nil
}

func (s *Store) queryDocument(ctx context.Context, where string, args ...any) (Document, bool, error) {
	d, err := scanDocument(s.db.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM documents WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("index: %w", err)
	}
	return d, true, nil
}

// DocumentByRemote looks a document up by its identity.
func (s *Store) DocumentByRemote(ctx context.Context, remote, remoteID string) (Document, bool, error) {
	return s.queryDocument(ctx, `remote = ? AND remote_id = ?`, remote, remoteID)
}

// DocumentByPath looks a document up by its current path. Should two
// identities briefly share a path (a replace racing a reconcile), the most
// recently indexed one wins.
func (s *Store) DocumentByPath(ctx context.Context, p string) (Document, bool, error) {
	return s.queryDocument(ctx, `path = ? ORDER BY indexed_at DESC, id DESC LIMIT 1`, p)
}

// DocumentByID looks a document up by its row id.
func (s *Store) DocumentByID(ctx context.Context, id int64) (Document, bool, error) {
	return s.queryDocument(ctx, `id = ?`, id)
}

// SetPath moves one document to a new path. IndexedAt is untouched: the
// text did not change.
func (s *Store) SetPath(ctx context.Context, id int64, p string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE documents SET path = ? WHERE id = ?`, p, id)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotIndexed
		}
		return nil
	})
}

// RenamePrefix moves every document at or under oldPrefix to newPrefix and
// returns how many moved. Only the path column changes: chunks carry no
// path, and IndexedAt stays, so a directory rename costs one UPDATE. The
// match is by exact prefix plus '/', never GLOB, so '*?[' in a directory
// name is literal.
func (s *Store) RenamePrefix(ctx context.Context, oldPrefix, newPrefix string) (int64, error) {
	oldPrefix = strings.TrimRight(oldPrefix, "/")
	newPrefix = strings.TrimRight(newPrefix, "/")
	if oldPrefix == "" || newPrefix == "" {
		return 0, errors.New("index: RenamePrefix needs two non-root prefixes")
	}
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE documents SET path = ? || substr(path, length(?) + 1)
			WHERE path = ? OR substr(path, 1, length(?) + 1) = ? || '/'`,
			newPrefix, oldPrefix, oldPrefix, oldPrefix, oldPrefix)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// DeleteDocument removes a document, its chunks and their vectors.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error { return deleteDocumentTx(ctx, tx, id, ch) })
}

func deleteDocumentTx(ctx context.Context, tx *sql.Tx, id int64, ch *vecChange) error {
	if err := dropDocVectorsTx(ctx, tx, id, ch); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// DeleteMissing removes every document whose id is not in seen: the set a
// full reconcile walk just visited. It returns how many were removed.
func (s *Store) DeleteMissing(ctx context.Context, seen map[int64]bool) (int64, error) {
	var n int64
	err := s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM documents`)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		var gone []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("index: %w", err)
			}
			if !seen[id] {
				gone = append(gone, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		for _, id := range gone {
			if err := deleteDocumentTx(ctx, tx, id, ch); err != nil {
				return err
			}
		}
		n = int64(len(gone))
		return nil
	})
	return n, err
}

// DocumentsUnder counts the documents at or under prefix ("/" counts all).
func (s *Store) DocumentsUnder(ctx context.Context, prefix string) (int, error) {
	prefix = strings.TrimRight(prefix, "/")
	var n int
	var err error
	if prefix == "" {
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM documents`).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM documents
			WHERE path = ? OR substr(path, 1, length(?) + 1) = ? || '/'`, prefix, prefix, prefix).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	return n, nil
}

// Rules returns every rule ordered by path.
func (s *Store) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, include, exclude, max_file_size, source FROM rules ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var include, exclude string
		if err := rows.Scan(&r.Path, &include, &exclude, &r.MaxFileSize, &r.Source); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		if err := json.Unmarshal([]byte(include), &r.Include); err != nil {
			return nil, fmt.Errorf("index: rule %s: %w", r.Path, err)
		}
		if err := json.Unmarshal([]byte(exclude), &r.Exclude); err != nil {
			return nil, fmt.Errorf("index: rule %s: %w", r.Path, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return out, nil
}

func upsertRuleTx(ctx context.Context, tx *sql.Tx, r Rule) error {
	include, err := json.Marshal(nonNil(r.Include))
	if err != nil {
		return err
	}
	exclude, err := json.Marshal(nonNil(r.Exclude))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rules(path, include, exclude, max_file_size, source)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET include = excluded.include, exclude = excluded.exclude,
		 max_file_size = excluded.max_file_size, source = excluded.source`,
		r.Path, string(include), string(exclude), r.MaxFileSize, r.Source)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// SyncConfigRules makes the "config" rules exactly rules: those no longer
// in the configuration are dropped, the rest are refreshed. A run-time rule
// at the same path is taken over by the configuration. Rules added at run
// time elsewhere are untouched.
func (s *Store) SyncConfigRules(ctx context.Context, rules []Rule) error {
	return s.syncManaged(ctx, SourceConfig, rules)
}

// SyncBuiltinRules is SyncConfigRules for the "builtin" source. A config
// rule at the same path keeps its source: the configuration file wins
// over a rule derived from it.
func (s *Store) SyncBuiltinRules(ctx context.Context, rules []Rule) error {
	return s.syncManaged(ctx, SourceBuiltin, rules)
}

func (s *Store) syncManaged(ctx context.Context, source string, rules []Rule) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rules WHERE source = ?`, source); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		for _, r := range rules {
			r.Source = source
			if source == SourceBuiltin {
				var existing string
				err := tx.QueryRowContext(ctx, `SELECT source FROM rules WHERE path = ?`, r.Path).Scan(&existing)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("index: %w", err)
				}
				if err == nil && existing == SourceConfig {
					continue
				}
			}
			if err := upsertRuleTx(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddRule persists a run-time rule (Source defaults to "tool"). A path the
// configuration already covers cannot be overridden: ErrConfigRule, or
// ErrBuiltinRule for the built-in memory rule.
func (s *Store) AddRule(ctx context.Context, r Rule) error {
	if r.Path == "" {
		return errors.New("index: a rule needs a path")
	}
	if r.Source == "" {
		r.Source = "tool"
	}
	if managed(r.Source) {
		return errors.New("index: config and builtin rules are synchronised with SyncConfigRules and SyncBuiltinRules")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var source string
		err := tx.QueryRowContext(ctx, `SELECT source FROM rules WHERE path = ?`, r.Path).Scan(&source)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("index: %w", err)
		}
		if err == nil && managed(source) {
			return managedErr(source)
		}
		return upsertRuleTx(ctx, tx, r)
	})
}

// managedErr is the refusal for touching a managed rule.
func managedErr(source string) error {
	if source == SourceBuiltin {
		return ErrBuiltinRule
	}
	return ErrConfigRule
}

// RemoveRule deletes a run-time rule. Config rules answer ErrConfigRule,
// the built-in memory rule ErrBuiltinRule, unknown paths ErrNoRule.
func (s *Store) RemoveRule(ctx context.Context, p string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var source string
		err := tx.QueryRowContext(ctx, `SELECT source FROM rules WHERE path = ?`, p).Scan(&source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoRule
		}
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		if managed(source) {
			return managedErr(source)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rules WHERE path = ?`, p); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		return nil
	})
}

// Enqueue queues a file for extraction. The queue is keyed by inode: a
// second Enqueue for a queued inode refreshes its path and reason and keeps
// its place in line.
func (s *Store) Enqueue(ctx context.Context, ino uint64, p string, why PendingReason) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return enqueueTx(ctx, tx, ino, p, why, s.now().UnixNano())
	})
}

func enqueueTx(ctx context.Context, tx *sql.Tx, ino uint64, p string, why PendingReason, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO index_pending(ino, path, reason, queued_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(ino) DO UPDATE SET path = excluded.path, reason = excluded.reason`,
		int64(ino), p, int(why), now)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// Pending returns up to limit queued files, oldest first.
func (s *Store) Pending(ctx context.Context, limit int) ([]PendingItem, error) {
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ino, path, reason, queued_at FROM index_pending
		ORDER BY queued_at, ino LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var out []PendingItem
	for rows.Next() {
		var it PendingItem
		var ino, reason, queued int64
		if err := rows.Scan(&ino, &it.Path, &reason, &queued); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		it.Ino = uint64(ino)
		it.Reason = PendingReason(reason)
		it.QueuedAt = time.Unix(0, queued)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return out, nil
}

// ChunkCount reports how many chunks document id has.
func (s *Store) ChunkCount(ctx context.Context, id int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE doc_id = ?`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	return n, nil
}

// Queued reports whether the file with inode ino waits in the queue.
func (s *Store) Queued(ctx context.Context, ino uint64) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM index_pending WHERE ino = ?`, int64(ino)).Scan(&n); err != nil {
		return false, fmt.Errorf("index: %w", err)
	}
	return n > 0, nil
}

// Dequeue removes an inode from the queue. Removing an absent inode is not
// an error.
func (s *Store) Dequeue(ctx context.Context, ino uint64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM index_pending WHERE ino = ?`, int64(ino)); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		return nil
	})
}

// Stats summarises the index.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	// octet_length reads the byte count from the record header, so the
	// sum does not decode every document's text.
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM documents WHERE state = ?),
		(SELECT count(*) FROM documents WHERE state = ?),
		(SELECT count(*) FROM documents WHERE state = ?),
		(SELECT count(*) FROM chunks),
		(SELECT coalesce(sum(octet_length(text)), 0) FROM documents),
		(SELECT count(*) FROM index_pending),
		(SELECT count(*) FROM vectors),
		(SELECT count(*) FROM embed_pending)`,
		int(DocOK), int(DocDirty), int(DocFailed)).
		Scan(&st.DocsOK, &st.DocsDirty, &st.DocsFailed, &st.Chunks, &st.TextBytes, &st.Pending, &st.Vectors, &st.EmbedPending)
	if err != nil {
		return Stats{}, fmt.Errorf("index: %w", err)
	}
	return st, nil
}

// Failed pages through failed documents, most recently failed first. The
// returned cursor is "" when no page follows; pass it back to continue.
func (s *Store) Failed(ctx context.Context, cursor string, limit int) ([]FailedDoc, string, error) {
	if limit <= 0 {
		limit = 20
	}
	where := `state = ?`
	args := []any{int(DocFailed)}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		where += ` AND (indexed_at < ? OR (indexed_at = ? AND id < ?))`
		args = append(args, at, at, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, kind, error, indexed_at FROM documents
		WHERE `+where+` ORDER BY indexed_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var out []FailedDoc
	var lastAt, lastID int64
	for rows.Next() {
		var f FailedDoc
		var id, at int64
		if err := rows.Scan(&id, &f.Path, &f.Kind, &f.Error, &at); err != nil {
			return nil, "", fmt.Errorf("index: %w", err)
		}
		if len(out) == limit {
			return out, encodeCursor(lastAt, lastID), nil
		}
		f.IndexedAt = time.Unix(0, at)
		out = append(out, f)
		lastAt, lastID = at, id
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("index: %w", err)
	}
	return out, "", nil
}

func encodeCursor(at, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + ":" + strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (at, id int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, 0, fmt.Errorf("index: bad cursor: %w", err)
	}
	a, b, ok := strings.Cut(string(raw), ":")
	if !ok {
		return 0, 0, errors.New("index: bad cursor")
	}
	if at, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("index: bad cursor: %w", err)
	}
	if id, err = strconv.ParseInt(b, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("index: bad cursor: %w", err)
	}
	return at, id, nil
}

// RetryFailed turns failed documents at or under p ("" or "/" for all)
// back to dirty and queues them for extraction. It returns how many.
func (s *Store) RetryFailed(ctx context.Context, p string) (int64, error) {
	p = strings.TrimRight(p, "/")
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		where := `state = ?`
		args := []any{int(DocFailed)}
		if p != "" {
			where += ` AND (path = ? OR substr(path, 1, length(?) + 1) = ? || '/')`
			args = append(args, p, p, p)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, ino, path FROM documents WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		type hit struct {
			id, ino int64
			path    string
		}
		var hits []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.id, &h.ino, &h.path); err != nil {
				rows.Close()
				return fmt.Errorf("index: %w", err)
			}
			hits = append(hits, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		now := s.now().UnixNano()
		for _, h := range hits {
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET state = ? WHERE id = ?`, int(DocDirty), h.id); err != nil {
				return fmt.Errorf("index: %w", err)
			}
			if h.ino != 0 {
				if err := enqueueTx(ctx, tx, uint64(h.ino), h.path, PendingRetry, now); err != nil {
					return err
				}
			}
		}
		n = int64(len(hits))
		return nil
	})
	return n, err
}

// Text returns the window [offset, offset+max) of the extracted text of
// the document at p, its end pulled back to a rune boundary. max <= 0
// takes a 64 KiB page. Offsets are bytes into the extracted text, which
// for text kinds are file offsets too.
func (s *Store) Text(ctx context.Context, p string, offset int64, max int) (TextPage, error) {
	var text, kind, version string
	err := s.db.QueryRowContext(ctx, `SELECT text, kind, version FROM documents WHERE path = ?
		ORDER BY indexed_at DESC, id DESC LIMIT 1`, p).Scan(&text, &kind, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return TextPage{}, ErrNotIndexed
	}
	if err != nil {
		return TextPage{}, fmt.Errorf("index: %w", err)
	}
	if max <= 0 {
		max = defaultTextPage
	}
	if offset < 0 {
		offset = 0
	}
	n := int64(len(text))
	if offset >= n {
		return TextPage{Path: p, Version: version, Offset: offset, NextOffset: n, EOF: true, Kind: kind}, nil
	}
	start := offset
	for start < n && !utf8.RuneStart(text[start]) {
		start++
	}
	end := start + int64(max)
	if end >= n {
		end = n
	} else {
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == start {
			// A single rune wider than max: return it whole rather than
			// nothing, or the caller would never advance.
			_, size := utf8.DecodeRuneInString(text[start:])
			end = start + int64(size)
		}
	}
	return TextPage{
		Path:       p,
		Version:    version,
		Text:       text[start:end],
		Offset:     offset,
		NextOffset: end,
		EOF:        end >= n,
		Kind:       kind,
	}, nil
}

// MatchQuery turns free text into an FTS5 MATCH expression for chunks_fts:
// each whitespace-separated word becomes a quoted phrase (so operators and
// punctuation are literal) and the phrases are implicitly ANDed. It returns
// "" when nothing is left to match.
func MatchQuery(query string) string {
	var parts []string
	for _, w := range strings.Fields(query) {
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " ")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// truncateRunes cuts s to at most n bytes on a rune boundary.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
