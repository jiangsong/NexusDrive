// Package meta is the persistent metadata cache: the directory tree, per
// directory freshness state, the negative cache, delta cursors, pins and the
// filename index. Every lookup, getattr and readdir is served from here when
// the entry is fresh, so a warm tree costs zero provider calls
// (docs/DESIGN.md §4.3).
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/provider"

	_ "modernc.org/sqlite"
)

// RootIno is the inode of the mount root. It always exists.
const RootIno uint64 = 1

// ErrNotFound is returned when an inode or name is not in the cache.
var ErrNotFound = errors.New("meta: not found")

var ErrExists = errors.New("meta: already exists")

// Node is one cached tree entry.
type Node struct {
	Ino       uint64
	ParentIno uint64
	Name      string
	Kind      provider.Kind
	Size      int64
	MTime     time.Time
	Mode      uint32
	Remote    string
	RemoteID  string
	Version   string
	// RemoteVersion is the last version observed on the provider. It differs
	// from Version while a local write is still queued, and it is what
	// conflict detection compares against.
	RemoteVersion string
	HashType      string
	Hash          string
	FetchedAt     time.Time
	TTL           time.Duration
	Dirty         bool
}

// IsDir reports whether n is a directory.
func (n Node) IsDir() bool { return n.Kind == provider.KindDir }

// Fresh reports whether the node's attributes are still within their TTL.
func (n Node) Fresh(now time.Time) bool {
	if n.TTL <= 0 {
		return false
	}
	return now.Sub(n.FetchedAt) < n.TTL
}

// DirState is the listing freshness of a directory.
type DirState struct {
	Complete bool
	ListedAt time.Time
	Cursor   string
	Dirty    bool
}

// Fresh reports whether a directory listing can be served locally.
func (d DirState) Fresh(now time.Time, ttl time.Duration) bool {
	return d.Complete && ttl > 0 && now.Sub(d.ListedAt) < ttl
}

// Store is the SQLite-backed metadata cache. It is safe for concurrent use.
type Store struct {
	db           *sql.DB
	path         string
	dsn          string
	now          func() time.Time
	listingMu    sync.Mutex
	listingStop  chan struct{}
	listingSlots chan struct{}
	listings     map[*DirListing]struct{}

	// writeMu serialises write transactions. SQLite allows one writer; taking
	// the lock in-process avoids SQLITE_BUSY churn under the FUSE load.
	writeMu sync.Mutex

	// The indexer folds name_index_pending into the FTS table off the
	// readdir path.
	indexStop chan struct{}
	indexWG   sync.WaitGroup
	closeOnce sync.Once

	// childrenScans counts full directory reads, so tests can assert that a
	// path which only needs one row does not read them all.
	childrenScans atomic.Int64

	// stmts caches prepared statements for the point queries the FUSE path
	// issues thousands of times a second; parsing SQL per call was a fifth
	// of the CPU behind a small-file create.
	stmts sync.Map // query text -> *sql.Stmt

	// absent is the negative cache: names known not to exist under a parent,
	// until a deadline. It lives in memory because it is only a cache — a
	// miss costs one listing — and keeping it in a table made every create
	// pay a write transaction to record the miss that preceded it.
	absentMu sync.Mutex
	absent   map[absentKey]time.Time

	// queries and writeTx count statements and write transactions, so a
	// test can put a budget on what one FUSE operation may cost.
	queries atomic.Int64
	writeTx atomic.Int64
}

type absentKey struct {
	parent uint64
	name   string
}

// QueryStats reports point queries and write transactions so far.
func (s *Store) QueryStats() (queries, writeTx int64) {
	return s.queries.Load(), s.writeTx.Load()
}

// prep returns a prepared statement for q, preparing it on first use.
func (s *Store) prep(ctx context.Context, q string) (*sql.Stmt, error) {
	if st, ok := s.stmts.Load(q); ok {
		return st.(*sql.Stmt), nil
	}
	st, err := s.db.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	if prev, loaded := s.stmts.LoadOrStore(q, st); loaded {
		st.Close()
		return prev.(*sql.Stmt), nil
	}
	return st, nil
}

// txStmt returns the cached prepared statement for q bound to tx.
func (s *Store) txStmt(ctx context.Context, tx *sql.Tx, q string) (*sql.Stmt, error) {
	st, err := s.prep(ctx, q)
	if err != nil {
		return nil, err
	}
	return tx.StmtContext(ctx, st), nil
}

// ChildrenScans reports how many times a whole directory was read.
func (s *Store) ChildrenScans() int64 { return s.childrenScans.Load() }

// Options configures Open.
type Options struct {
	// Now is injectable for tests; nil uses time.Now.
	Now func() time.Time
	// NoIndexMaintenance disables automatic index flushing, including Close.
	// Targeted offline administration must not process unrelated pending names.
	NoIndexMaintenance bool
}

// Open opens (creating if needed) the metadata database at dbPath and applies
// migrations. Use ":memory:" for tests.
func Open(dbPath string, opt Options) (*Store, error) {
	dsn := dbPath
	if dbPath == ":memory:" {
		// A shared in-memory database keeps every pooled connection on the
		// same data; a plain ":memory:" would give each connection its own.
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	} else {
		if dir := path.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("meta: %w", err)
			}
		}
		dsn = "file:" + dbPath +
			"?_pragma=journal_mode(WAL)&_txlock=immediate" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=busy_timeout(5000)" +
			"&_pragma=foreign_keys(0)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	if dbPath == ":memory:" {
		// The shared cache lives as long as one connection is open.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	s := &Store{db: db, path: dbPath, dsn: dsn, now: opt.Now, absent: map[absentKey]time.Time{},
		listingStop: make(chan struct{}), listingSlots: make(chan struct{}, 8), listings: make(map[*DirListing]struct{})}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if !opt.NoIndexMaintenance {
		s.startIndexer()
	}
	return s, nil
}

// Close releases the database, flushing pending names unless automatic index
// maintenance was disabled for targeted offline administration.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closeListings()
		if s.indexStop != nil {
			close(s.indexStop)
			s.indexWG.Wait()
			// Best effort: pending rows survive failures for the next open.
			_ = s.FlushIndex(context.Background())
		}
		err = s.db.Close()
	})
	return err
}

// DB exposes the handle for packages that share the file (journal, cache
// index). Callers must respect WriteLock for write transactions.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var current int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("meta: read user_version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("meta: database schema v%d is newer than this build (v%d)", current, schemaVersion)
	}
	for v := current; v < schemaVersion; v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("meta: %w", err)
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("meta: migration %d: %w", v+1, err)
		}
		// PRAGMA does not accept placeholders.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("meta: set user_version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("meta: %w", err)
		}
	}
	return s.ensureRoot()
}

func (s *Store) ensureRoot() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE ino = ?`, RootIno).Scan(&n); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if n > 0 {
		return nil
	}
	now := s.now()
	_, err := s.db.Exec(
		`INSERT INTO nodes (ino, parent_ino, name, kind, mode, mtime_ns, fetched_at, ttl_s)
		 VALUES (?, ?, '', ?, ?, ?, ?, ?)`,
		RootIno, RootIno, int(provider.KindDir), 0o755, now.UnixNano(), now.Unix(), int64((365 * 24 * time.Hour).Seconds()))
	if err != nil {
		return fmt.Errorf("meta: create root: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO dir_state (ino, complete, listed_at) VALUES (?, 0, 0)`, RootIno)
	if err != nil {
		return fmt.Errorf("meta: create root dir_state: %w", err)
	}
	return nil
}

// WriteLock serialises external write transactions on the shared handle.
func (s *Store) WriteLock() *sync.Mutex { return &s.writeMu }

const nodeCols = `ino, parent_ino, name, kind, size, mtime_ns, mode, remote, remote_id,
                  version, remote_version, hash_type, hash, fetched_at, ttl_s, dirty`

type scanner interface {
	Scan(dest ...any) error
}

func scanNode(sc scanner) (Node, error) {
	var n Node
	var mtimeNS, fetchedAt, ttlS int64
	var kind, dirty int
	err := sc.Scan(&n.Ino, &n.ParentIno, &n.Name, &kind, &n.Size, &mtimeNS, &n.Mode,
		&n.Remote, &n.RemoteID, &n.Version, &n.RemoteVersion, &n.HashType, &n.Hash,
		&fetchedAt, &ttlS, &dirty)
	if err != nil {
		return Node{}, err
	}
	n.Kind = provider.Kind(kind)
	n.MTime = time.Unix(0, mtimeNS)
	n.FetchedAt = time.Unix(fetchedAt, 0)
	n.TTL = time.Duration(ttlS) * time.Second
	n.Dirty = dirty != 0
	return n, nil
}

// Get returns the node with the given inode.
func (s *Store) Get(ctx context.Context, ino uint64) (Node, error) {
	s.queries.Add(1)
	st, err := s.prep(ctx, `SELECT `+nodeCols+` FROM nodes WHERE ino = ?`)
	if err != nil {
		return Node{}, fmt.Errorf("meta: get %d: %w", ino, err)
	}
	n, err := scanNode(st.QueryRowContext(ctx, ino))
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("meta: get %d: %w", ino, err)
	}
	return n, nil
}

// Lookup returns the child of parent with the given name.
func (s *Store) Lookup(ctx context.Context, parent uint64, name string) (Node, error) {
	s.queries.Add(1)
	st, err := s.prep(ctx, `SELECT `+nodeCols+` FROM nodes WHERE parent_ino = ? AND name = ? AND ino != ?`)
	if err != nil {
		return Node{}, fmt.Errorf("meta: lookup %d/%s: %w", parent, name, err)
	}
	n, err := scanNode(st.QueryRowContext(ctx, parent, name, RootIno))
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("meta: lookup %d/%s: %w", parent, name, err)
	}
	return n, nil
}

// ByRemoteID finds a node by the provider's own id. The delta refresher needs
// it because a change feed identifies files by remote id, not by path.
func (s *Store) ByRemoteID(ctx context.Context, remote, remoteID string) (Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE remote = ? AND remote_id = ? LIMIT 1`, remote, remoteID)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("meta: by_remote_id %s/%s: %w", remote, remoteID, err)
	}
	return n, nil
}

// Aliases returns all cached views of a remote identity. One remote can be
// mounted at several prefixes, so retention cannot use ByRemoteID's first hit.
func (s *Store) Aliases(ctx context.Context, remote, remoteID string) ([]Node, error) {
	s.queries.Add(1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE remote = ? AND remote_id = ?`, remote, remoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Children returns the cached children of dir, sorted by name.
func (s *Store) Children(ctx context.Context, dir uint64) ([]Node, error) {
	s.childrenScans.Add(1)
	s.queries.Add(1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE parent_ino = ? AND ino != ? ORDER BY name`, dir, RootIno)
	if err != nil {
		return nil, fmt.Errorf("meta: children %d: %w", dir, err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("meta: children %d: %w", dir, err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UncommittedDirs lists directories that exist only in the tree: no remote
// id and dirty. A queued directory holds a local id, a backend directory a
// real one; a directory in neither state was inserted and never committed,
// which only an interrupted mkdir leaves behind. Mount directories, which
// also have no remote id, are not dirty and are not listed.
func (s *Store) UncommittedDirs(ctx context.Context) ([]Node, error) {
	s.queries.Add(1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE kind = ? AND remote_id = '' AND dirty = 1 AND ino != ?`,
		int(provider.KindDir), RootIno)
	if err != nil {
		return nil, fmt.Errorf("meta: uncommitted dirs: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("meta: uncommitted dirs: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DirState returns the listing state of dir.
func (s *Store) DirState(ctx context.Context, dir uint64) (DirState, error) {
	var d DirState
	var complete, dirty int
	var listedAt int64
	s.queries.Add(1)
	st, err := s.prep(ctx, `SELECT complete, listed_at, cursor, dirty FROM dir_state WHERE ino = ?`)
	if err != nil {
		return DirState{}, fmt.Errorf("meta: dir_state %d: %w", dir, err)
	}
	err = st.QueryRowContext(ctx, dir).Scan(&complete, &listedAt, &d.Cursor, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return DirState{}, nil
	}
	if err != nil {
		return DirState{}, fmt.Errorf("meta: dir_state %d: %w", dir, err)
	}
	d.Complete = complete != 0
	d.Dirty = dirty != 0
	d.ListedAt = time.Unix(listedAt, 0)
	return d, nil
}

// Path returns the absolute path of ino within the mount ("/" for the root).
func (s *Store) Path(ctx context.Context, ino uint64) (string, error) {
	if ino == RootIno {
		return "/", nil
	}
	// One recursive query walks to the root; a Get per level cost a
	// statement per path component on every create and open.
	s.queries.Add(1)
	st, err := s.prep(ctx, `
		WITH RECURSIVE up(ino, parent_ino, name, depth) AS (
			SELECT ino, parent_ino, name, 0 FROM nodes WHERE ino = ?
			UNION ALL
			SELECT n.ino, n.parent_ino, n.name, up.depth + 1
			  FROM nodes n JOIN up ON n.ino = up.parent_ino
			 WHERE up.ino != ? AND up.parent_ino != up.ino AND up.depth < 512
		)
		SELECT ino, parent_ino, name FROM up ORDER BY depth`)
	if err != nil {
		return "", fmt.Errorf("meta: path %d: %w", ino, err)
	}
	rows, err := st.QueryContext(ctx, ino, RootIno)
	if err != nil {
		return "", fmt.Errorf("meta: path %d: %w", ino, err)
	}
	defer rows.Close()
	var parts []string
	reachedRoot := false
	for rows.Next() {
		var cur, parent uint64
		var name string
		if err := rows.Scan(&cur, &parent, &name); err != nil {
			return "", fmt.Errorf("meta: path %d: %w", ino, err)
		}
		if cur == RootIno {
			reachedRoot = true
			break
		}
		parts = append(parts, name)
		if parent == cur {
			reachedRoot = true // a self-parented node ends the walk, as before
			break
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("meta: path %d: %w", ino, err)
	}
	if !reachedRoot {
		return "", ErrNotFound
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/"), nil
}

// Resolve walks a slash-separated path from the root and returns the node.
// Missing components yield ErrNotFound.
func (s *Store) Resolve(ctx context.Context, p string) (Node, error) {
	cur, err := s.Get(ctx, RootIno)
	if err != nil {
		return Node{}, err
	}
	for _, seg := range strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if seg == "" {
			continue
		}
		cur, err = s.Lookup(ctx, cur.Ino, seg)
		if err != nil {
			return Node{}, err
		}
	}
	return cur, nil
}

// tx runs fn inside a write transaction holding the in-process writer lock.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeTx.Add(1)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("meta: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("meta: commit: %w", err)
	}
	return nil
}

// Upsert inserts or updates one node under parent and returns it with its
// inode filled in. Name and ParentIno identify the row; the inode is stable
// across updates so open handles keep working.
func (s *Store) Upsert(ctx context.Context, n Node) (Node, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ino, err := s.upsertNodeTx(ctx, tx, n)
		if err != nil {
			return err
		}
		n.Ino = ino
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	// A present name invalidates any negative-cache entry.
	s.clearAbsent(n.ParentIno, n.Name)
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	return n, nil
}

// Insert creates a name without replacing an existing inode. The absence
// check and insert share a transaction; callers must not emulate O_EXCL with
// a Lookup followed by Upsert, which can overwrite a concurrent creator.
func (s *Store) Insert(ctx context.Context, n Node) (Node, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var ino uint64
		err := tx.QueryRowContext(ctx, `SELECT ino FROM nodes WHERE parent_ino=? AND name=?`, n.ParentIno, n.Name).Scan(&ino)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ino, err = s.upsertNodeTx(ctx, tx, n)
		n.Ino = ino
		return err
	})
	if err != nil {
		return Node{}, err
	}
	s.clearAbsent(n.ParentIno, n.Name)
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	return n, nil
}

// InsertCompleteDir creates a directory that is known to be empty — this
// machine just made it — so it is born with a complete listing and the first
// lookup inside it is answered locally instead of listing an empty directory
// on the backend. The node and its listing state are one transaction: a
// listing marked complete afterwards would have to be reconciled against
// children that may have arrived in between.
func (s *Store) InsertCompleteDir(ctx context.Context, n Node) (Node, error) {
	if n.Kind != provider.KindDir {
		return Node{}, errors.New("meta: InsertCompleteDir needs a directory")
	}
	now := s.now()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var ino uint64
		err := tx.QueryRowContext(ctx, `SELECT ino FROM nodes WHERE parent_ino=? AND name=?`, n.ParentIno, n.Name).Scan(&ino)
		if err == nil {
			return ErrExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ino, err = s.upsertNodeTx(ctx, tx, n)
		if err != nil {
			return err
		}
		n.Ino = ino
		_, err = tx.Exec(
			`INSERT INTO dir_state (ino, complete, listed_at, cursor, dirty) VALUES (?, 1, ?, '', 0)
			 ON CONFLICT(ino) DO UPDATE SET complete=1, listed_at=excluded.listed_at, cursor='', dirty=0`,
			ino, now.Unix())
		if err != nil {
			return fmt.Errorf("meta: mark new directory complete: %w", err)
		}
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	s.clearAbsent(n.ParentIno, n.Name)
	if n.FetchedAt.IsZero() {
		n.FetchedAt = now
	}
	return n, nil
}

func (s *Store) upsertNodeTx(ctx context.Context, tx *sql.Tx, n Node) (uint64, error) {
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	if n.Mode == 0 {
		if n.Kind == provider.KindDir {
			n.Mode = 0o755
		} else {
			n.Mode = 0o644
		}
	}
	dirty := 0
	if n.Dirty {
		dirty = 1
	}
	var existing uint64
	sel, err := s.txStmt(ctx, tx, `SELECT ino FROM nodes WHERE parent_ino = ? AND name = ?`)
	if err != nil {
		return 0, err
	}
	err = sel.QueryRowContext(ctx, n.ParentIno, n.Name).Scan(&existing)
	switch {
	case err == nil:
		upd, err := s.txStmt(ctx, tx,
			`UPDATE nodes SET kind=?, size=?, mtime_ns=?, mode=?, remote=?, remote_id=?,
			   version=?, remote_version=?, hash_type=?, hash=?, fetched_at=?, ttl_s=?, dirty=?
			 WHERE ino = ?`)
		if err != nil {
			return 0, err
		}
		_, err = upd.ExecContext(ctx,
			int(n.Kind), n.Size, n.MTime.UnixNano(), n.Mode, n.Remote, n.RemoteID,
			n.Version, n.RemoteVersion, n.HashType, n.Hash, n.FetchedAt.Unix(),
			int64(n.TTL.Seconds()), dirty, existing)
		if err != nil {
			return 0, fmt.Errorf("meta: update node: %w", err)
		}
		// The row was found by name, so the search index already has it.
		return existing, nil
	case errors.Is(err, sql.ErrNoRows):
		ins, err := s.txStmt(ctx, tx,
			`INSERT INTO nodes (parent_ino, name, kind, size, mtime_ns, mode, remote, remote_id,
			                    version, remote_version, hash_type, hash, fetched_at, ttl_s, dirty)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return 0, err
		}
		res, err := ins.ExecContext(ctx,
			n.ParentIno, n.Name, int(n.Kind), n.Size, n.MTime.UnixNano(), n.Mode, n.Remote, n.RemoteID,
			n.Version, n.RemoteVersion, n.HashType, n.Hash, n.FetchedAt.Unix(),
			int64(n.TTL.Seconds()), dirty)
		if err != nil {
			return 0, fmt.Errorf("meta: insert node: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("meta: insert node: %w", err)
		}
		ino := uint64(id)
		if n.Kind == provider.KindDir {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO dir_state (ino) VALUES (?)`, ino); err != nil {
				return 0, fmt.Errorf("meta: insert dir_state: %w", err)
			}
		}
		// Search triggers publish short postings and defer trigram work.
		return ino, nil
	default:
		return 0, fmt.Errorf("meta: upsert lookup: %w", err)
	}
}

func reindexTx(tx *sql.Tx, ino uint64, name string) error {
	return indexNameTx(tx, ino, name)
}

// indexNameTx puts one name into the FTS table, replacing what was there.
// The FTS rowid is the inode, so the delete is a key lookup rather than a
// scan of an UNINDEXED column, and name_indexed records what is in.
func indexNameTx(tx *sql.Tx, ino uint64, name string) error {
	if _, err := tx.Exec(`DELETE FROM name_index WHERE rowid = ?`, ino); err != nil {
		return fmt.Errorf("meta: reindex: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO name_index (rowid, name, path, ino) VALUES (?, ?, '', ?)`, ino, name, ino); err != nil {
		return fmt.Errorf("meta: reindex: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO name_indexed (ino, name) VALUES (?, ?)`, ino, name); err != nil {
		return fmt.Errorf("meta: reindex: %w", err)
	}
	// A synchronous rename must not leave an older deferred name behind.
	if _, err := tx.Exec(`DELETE FROM name_index_pending WHERE ino=?`, ino); err != nil {
		return fmt.Errorf("meta: reindex pending: %w", err)
	}
	return nil
}

// PutDir replaces the cached children of dir with entries and marks the
// listing complete. Children that disappeared are deleted with their subtrees.
// Inodes of surviving children are preserved.
// PutDir replaces the children of a directory with a fresh listing.
//
// protect, when non-nil, is asked about every child already in the tree. A
// child it protects is neither overwritten nor removed, whatever the listing
// says. That is what keeps a file whose upload is still queued alive: the
// backend has not seen it yet, so it is absent from the listing, and without
// this the next directory refresh would delete the entry out from under a
// caller that had just written it — and the read that follows its own write
// would fail with ENOENT.
//
// The work is batched: one scan of the existing children, one prepared
// UPDATE per changed row, multi-row INSERTs for new rows, and a single
// statement that queues every name for the FTS index. Per-row statements
// made a 2,000-entry listing cost two seconds — almost all of it SQLite, not
// the network — which is the wrong place for a cold directory walk to spend
// its time.
func (s *Store) PutDir(ctx context.Context, dir uint64, children []Node, childTTL time.Duration, protect func(Node) bool) error {
	_, err := s.PutDirChanged(ctx, dir, children, childTTL, protect)
	return err
}

// UpdateByIno rewrites a node's content fields — size, times, remote
// identity, hashes, dirtiness — leaving its name and parent as they are now.
// A commit or an upload completion must address the node it started with,
// not the name it had then: a rename in between would otherwise make an
// Upsert by name insert a second node under the old name, while the renamed
// one kept pointing at data the upsert then released.
func (s *Store) UpdateByIno(ctx context.Context, n Node) error {
	return s.updateByIno(ctx, n, false)
}

// PublishByIno durably publishes a journal-backed version. Unlike ordinary
// remote metadata cache updates, its transaction uses synchronous=FULL so a
// journal publication barrier may safely be released after it returns.
func (s *Store) PublishByIno(ctx context.Context, n Node) error {
	// Warm the statement before reserving a connection: :memory: stores have
	// a single connection and preparing via DB inside that transaction blocks.
	if _, err := s.prep(ctx, updateNodeSQL); err != nil {
		return err
	}
	return s.updateByIno(ctx, n, true)
}

const updateNodeSQL = `UPDATE nodes SET size=?, mtime_ns=?, remote=?, remote_id=?, version=?, remote_version=?,
			   hash_type=?, hash=?, fetched_at=?, ttl_s=?, dirty=?
			 WHERE ino = ?`

// updateNodeIfSQL is updateNodeSQL guarded on the identity the caller believes
// the node still has.
const updateNodeIfSQL = updateNodeSQL + ` AND remote_id = ?`

// AdoptByIno writes the node only while its stored remote id is still expect.
// It reports whether the write applied.
//
// An upload completing and a newer write committing are two read-modify-write
// sequences over the same row, and they interleave: the uploader reads the
// node, the writer commits a newer version, and the uploader then writes back
// what it read — putting the previous content's size, id and version back on a
// file that has moved on. The condition is what makes the loser notice.
//
// durable selects the synchronous=FULL transaction PublishByIno uses, so a
// journal publication barrier may be released after it returns.
func (s *Store) AdoptByIno(ctx context.Context, n Node, expect string, durable bool) (bool, error) {
	if durable {
		// Warm the statement before reserving a connection, as PublishByIno does.
		if _, err := s.prep(ctx, updateNodeIfSQL); err != nil {
			return false, err
		}
	}
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	dirty := 0
	if n.Dirty {
		dirty = 1
	}
	run := s.tx
	if durable {
		run = s.durableTx
	}
	var rows int64
	err := run(ctx, func(tx *sql.Tx) error {
		upd, err := s.txStmt(ctx, tx, updateNodeIfSQL)
		if err != nil {
			return err
		}
		res, err := upd.ExecContext(ctx,
			n.Size, n.MTime.UnixNano(), n.Remote, n.RemoteID, n.Version, n.RemoteVersion,
			n.HashType, n.Hash, n.FetchedAt.Unix(), int64(n.TTL.Seconds()), dirty, n.Ino, expect)
		if err != nil {
			return fmt.Errorf("meta: adopt node %d: %w", n.Ino, err)
		}
		rows, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// SetRemoteVersion records what the provider last had for one node without
// touching anything else. An upload that has been superseded still has to
// report the version it moved the remote to — that is what the next upload's
// conflict check compares against — but it must not carry the rest of the
// stale row it read along with it.
func (s *Store) SetRemoteVersion(ctx context.Context, ino uint64, version string) error {
	var rows int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE nodes SET remote_version=? WHERE ino=?`, version, ino)
		if err != nil {
			return fmt.Errorf("meta: set remote version %d: %w", ino, err)
		}
		rows, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) updateByIno(ctx context.Context, n Node, durable bool) error {
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	dirty := 0
	if n.Dirty {
		dirty = 1
	}
	var rows int64
	run := s.tx
	if durable {
		run = s.durableTx
	}
	err := run(ctx, func(tx *sql.Tx) error {
		upd, err := s.txStmt(ctx, tx, updateNodeSQL)
		if err != nil {
			return err
		}
		res, err := upd.ExecContext(ctx,
			n.Size, n.MTime.UnixNano(), n.Remote, n.RemoteID, n.Version, n.RemoteVersion,
			n.HashType, n.Hash, n.FetchedAt.Unix(), int64(n.TTL.Seconds()), dirty, n.Ino)
		if err != nil {
			return fmt.Errorf("meta: update node %d: %w", n.Ino, err)
		}
		rows, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// DirChange is what a listing changed in the tree: names that appeared,
// names that vanished, and inodes whose version or size moved. The VFS uses
// it to tell the kernel exactly what to forget — and, more importantly, to
// tell it nothing when nothing changed, because an invalidation for a
// directory the kernel is busy caching throws that cache away.
type DirChange struct {
	Added   []string
	Removed []string
	Updated []uint64
}

// Any reports whether the listing changed anything.
func (c DirChange) Any() bool {
	return len(c.Added)+len(c.Removed)+len(c.Updated) > 0
}

// PutDirChanged is PutDir reporting what it changed.
func (s *Store) PutDirChanged(ctx context.Context, dir uint64, children []Node, childTTL time.Duration, protect func(Node) bool) (DirChange, error) {
	now := s.now()
	var change DirChange
	err := s.tx(ctx, func(tx *sql.Tx) error {
		change = DirChange{}
		if err := fenceDirListingTx(ctx, tx, dir); err != nil {
			return err
		}
		existing := map[string]uint64{}
		was := map[string]Node{}
		protected := map[string]bool{}
		rows, err := tx.Query(
			`SELECT ino, name, kind, remote, remote_id, version, remote_version, size, fetched_at, dirty
			   FROM nodes WHERE parent_ino = ? AND ino != ?`, dir, RootIno)
		if err != nil {
			return fmt.Errorf("meta: put_dir scan: %w", err)
		}
		for rows.Next() {
			var n Node
			var kind, dirty int
			var fetched int64
			if err := rows.Scan(&n.Ino, &n.Name, &kind, &n.Remote, &n.RemoteID,
				&n.Version, &n.RemoteVersion, &n.Size, &fetched, &dirty); err != nil {
				rows.Close()
				return fmt.Errorf("meta: put_dir scan: %w", err)
			}
			n.Kind = provider.Kind(kind)
			n.FetchedAt = time.Unix(fetched, 0)
			n.Dirty = dirty != 0
			existing[n.Name] = n.Ino
			was[n.Name] = n
			if protect != nil && protect(n) {
				protected[n.Name] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("meta: put_dir scan: %w", err)
		}

		update, err := tx.Prepare(
			`UPDATE nodes SET kind=?, size=?, mtime_ns=?, mode=?, remote=?, remote_id=?,
			   version=?, remote_version=?, hash_type=?, hash=?, fetched_at=?, ttl_s=?, dirty=0
			 WHERE ino = ?`)
		if err != nil {
			return fmt.Errorf("meta: put_dir prepare: %w", err)
		}
		defer update.Close()

		keep := make(map[string]bool, len(children))
		var inserts []Node
		for _, c := range children {
			keep[c.Name] = true
			if protected[c.Name] {
				// The local copy is newer than anything the listing can say.
				continue
			}
			c.ParentIno = dir
			c.FetchedAt = now
			if c.TTL == 0 {
				c.TTL = childTTL
			}
			fillMode(&c)
			if ino, ok := existing[c.Name]; ok {
				old := was[c.Name]
				if directoryReplacement(old, c) {
					retained, err := prepareDirectoryReplacementTx(ctx, tx, old, protect)
					if err != nil {
						return err
					}
					if retained {
						continue
					}
					inserts = append(inserts, c)
					change.Added = append(change.Added, c.Name)
					change.Removed = append(change.Removed, c.Name)
					change.Updated = append(change.Updated, old.Ino)
					continue
				}
				same := old.Version == c.Version && old.Size == c.Size && old.Kind == c.Kind &&
					old.RemoteID == c.RemoteID && old.RemoteVersion == c.RemoteVersion && old.Remote == c.Remote
				if same {
					// Nothing about the entry moved: its fetched_at is bumped
					// by the one statement below, and the row is otherwise
					// left alone. Rewriting all 2,000 rows of an unchanged
					// directory on every refresh was most of a cold walk.
					continue
				}
				if _, err := update.Exec(int(c.Kind), c.Size, c.MTime.UnixNano(), c.Mode, c.Remote,
					c.RemoteID, c.Version, c.RemoteVersion, c.HashType, c.Hash,
					c.FetchedAt.Unix(), int64(c.TTL.Seconds()), ino); err != nil {
					return fmt.Errorf("meta: put_dir update: %w", err)
				}
				if old.Version != c.Version || old.Size != c.Size || old.Kind != c.Kind {
					change.Updated = append(change.Updated, ino)
				}
				continue
			}
			change.Added = append(change.Added, c.Name)
			inserts = append(inserts, c)
		}
		if err := insertNodesTx(tx, inserts); err != nil {
			return err
		}
		// One statement refreshes the fetch time of every clean child that
		// is still listed; the rows rewritten above set theirs already.
		if _, err := tx.Exec(
			`UPDATE nodes SET fetched_at = ?, ttl_s = ? WHERE parent_ino = ? AND ino != ? AND dirty = 0`,
			now.Unix(), int64(childTTL.Seconds()), dir, RootIno); err != nil {
			return fmt.Errorf("meta: put_dir touch: %w", err)
		}
		for name, ino := range existing {
			if keep[name] || protected[name] {
				continue
			}
			if was[name].IsDir() && protect != nil {
				protected, err := protectedSubtreeTx(ctx, tx, ino, protect)
				if err != nil {
					return err
				}
				if protected {
					continue
				}
			}
			if err := removeSubtreeTx(tx, ino); err != nil {
				return err
			}
			change.Removed = append(change.Removed, name)
		}
		// Triggers queue only inserted/renamed nodes. Unchanged refreshes
		// neither scan the index mirror nor generate new index work.
		_, err = tx.Exec(
			`INSERT INTO dir_state (ino, complete, listed_at, cursor, dirty) VALUES (?, 1, ?, '', 0)
			 ON CONFLICT(ino) DO UPDATE SET complete=1, listed_at=excluded.listed_at, cursor='', dirty=0`,
			dir, now.Unix())
		if err != nil {
			return fmt.Errorf("meta: put_dir state: %w", err)
		}
		return nil
	})
	if err != nil {
		return DirChange{}, err
	}
	for _, name := range change.Added {
		s.clearAbsent(dir, name)
	}
	return change, nil
}

// insertBatch bounds the parameters per statement well under SQLite's limit.
const insertBatch = 200

// insertNodesTx adds new rows with multi-row INSERTs.
func insertNodesTx(tx *sql.Tx, nodes []Node) error {
	const cols = 15
	for len(nodes) > 0 {
		n := len(nodes)
		if n > insertBatch {
			n = insertBatch
		}
		batch := nodes[:n]
		nodes = nodes[n:]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO nodes (parent_ino, name, kind, size, mtime_ns, mode, remote, remote_id,
			version, remote_version, hash_type, hash, fetched_at, ttl_s, dirty) VALUES `)
		args := make([]any, 0, n*cols)
		for i, c := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			dirty := 0
			if c.Dirty {
				dirty = 1
			}
			args = append(args, c.ParentIno, c.Name, int(c.Kind), c.Size, c.MTime.UnixNano(), c.Mode,
				c.Remote, c.RemoteID, c.Version, c.RemoteVersion, c.HashType, c.Hash,
				c.FetchedAt.Unix(), int64(c.TTL.Seconds()), dirty)
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return fmt.Errorf("meta: put_dir insert: %w", err)
		}
	}
	return nil
}

func fillMode(n *Node) {
	if n.Mode != 0 {
		return
	}
	if n.Kind == provider.KindDir {
		n.Mode = 0o755
	} else {
		n.Mode = 0o644
	}
}

// Remove deletes a node and, for directories, its whole subtree.
func (s *Store) Remove(ctx context.Context, ino uint64) error {
	if ino == RootIno {
		return errors.New("meta: cannot remove the root")
	}
	var parent uint64
	var name string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT parent_ino, name FROM nodes WHERE ino = ?`, ino).Scan(&parent, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("meta: remove: %w", err)
		}
		if err := fenceDirListingTx(ctx, tx, parent); err != nil {
			return err
		}
		if err := removeSubtreeTx(tx, ino); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The name is now known-absent for a short while.
	s.markAbsent(parent, name, defaultNegativeTTL)
	return nil
}

func removeSubtreeTx(tx *sql.Tx, ino uint64) error {
	rows, err := tx.Query(`SELECT ino FROM nodes WHERE parent_ino = ? AND ino != ?`, ino, RootIno)
	if err != nil {
		return fmt.Errorf("meta: remove subtree: %w", err)
	}
	var kids []uint64
	for rows.Next() {
		var k uint64
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return fmt.Errorf("meta: remove subtree: %w", err)
		}
		kids = append(kids, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("meta: remove subtree: %w", err)
	}
	for _, k := range kids {
		if err := removeSubtreeTx(tx, k); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DELETE FROM nodes WHERE ino = ?`,
		`DELETE FROM dir_state WHERE ino = ?`,
		`DELETE FROM name_index WHERE rowid = ?`,
		`DELETE FROM name_indexed WHERE ino = ?`,
		`DELETE FROM name_index_pending WHERE ino = ?`,
	} {
		if _, err := tx.Exec(q, ino); err != nil {
			return fmt.Errorf("meta: remove subtree: %w", err)
		}
	}
	return nil
}

// Rename moves ino under newParent with newName, replacing any existing target.
func (s *Store) Rename(ctx context.Context, ino, newParent uint64, newName string) error {
	if ino == RootIno {
		return errors.New("meta: cannot rename the root")
	}
	var oldParent uint64
	var oldName string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		return renameTx(ctx, tx, ino, newParent, newName, &oldParent, &oldName)
	})
	if err != nil {
		return err
	}
	s.clearAbsent(newParent, newName)
	s.markAbsent(oldParent, oldName, defaultNegativeTTL)
	return nil
}

// renameTx is the move itself, without a transaction of its own, so it can be
// committed together with a change that must land with it. It reports the
// names it moved away from through oldParent and oldName, which the caller
// needs for the negative-entry bookkeeping that follows the commit.
func renameTx(ctx context.Context, tx *sql.Tx, ino, newParent uint64, newName string, oldParent *uint64, oldName *string) error {
	err := tx.QueryRow(`SELECT parent_ino, name FROM nodes WHERE ino = ?`, ino).Scan(oldParent, oldName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("meta: rename: %w", err)
	}
	if *oldParent != newParent || *oldName != newName {
		if err := fenceDirListingTx(ctx, tx, *oldParent, newParent); err != nil {
			return err
		}
	}
	var victim uint64
	err = tx.QueryRow(`SELECT ino FROM nodes WHERE parent_ino = ? AND name = ?`, newParent, newName).Scan(&victim)
	if err == nil && victim != ino {
		if err := removeSubtreeTx(tx, victim); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("meta: rename: %w", err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET parent_ino = ?, name = ? WHERE ino = ?`, newParent, newName, ino); err != nil {
		return fmt.Errorf("meta: rename: %w", err)
	}
	return reindexTx(tx, ino, newName)
}

// Invalidate marks a directory listing stale so the next readdir refetches.
// It is the soft fence: it says the directory is out of date, not that a name
// under it was removed, so a listing already in flight may still publish what
// it saw — it will simply leave the directory incomplete. Callers that remove
// a name go through Remove or Rename, which fence for real.
func (s *Store) Invalidate(ctx context.Context, dir uint64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := markDirStaleTx(ctx, tx, dir); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE dir_state SET complete = 0, dirty = 1 WHERE ino = ?`, dir)
		if err != nil {
			return fmt.Errorf("meta: invalidate: %w", err)
		}
		return nil
	})
}

// InvalidateAll marks every directory listing stale and forgets every negative
// entry, so the next access of anything goes back to its backend. Nodes and
// pins are kept: a benchmark that wants a cold start needs the remote to be
// consulted again, not the tree to be rebuilt from nothing.
func (s *Store) InvalidateAll(ctx context.Context) error {
	s.absentMu.Lock()
	s.absent = map[absentKey]time.Time{}
	s.absentMu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE dir_state SET complete = 0, dirty = 1`); err != nil {
			return fmt.Errorf("meta: invalidate all: %w", err)
		}
		// Soft: nothing was removed, so a listing in flight may still
		// publish; it stays incomplete and the next reader goes to the
		// backend, which is exactly what dropping the cache asked for.
		if _, err := tx.ExecContext(ctx, `UPDATE directory_refresh_generation SET stale_generation=stale_generation+1`); err != nil {
			return err
		}
		return nil
	})
}

// Touch refreshes a node's fetched_at without changing its attributes.
func (s *Store) Touch(ctx context.Context, ino uint64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE nodes SET fetched_at = ? WHERE ino = ?`, s.now().Unix(), ino)
		if err != nil {
			return fmt.Errorf("meta: touch: %w", err)
		}
		return nil
	})
}

const defaultNegativeTTL = 5 * time.Second

// MarkAbsent records that name does not exist under parent for ttl.
func (s *Store) MarkAbsent(ctx context.Context, parent uint64, name string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = defaultNegativeTTL
	}
	s.markAbsent(parent, name, ttl)
	return nil
}

// IsAbsent reports whether a negative-cache entry for parent/name is live.
func (s *Store) IsAbsent(ctx context.Context, parent uint64, name string) (bool, error) {
	s.absentMu.Lock()
	defer s.absentMu.Unlock()
	until, ok := s.absent[absentKey{parent, name}]
	if !ok {
		return false, nil
	}
	if !s.now().Before(until) {
		delete(s.absent, absentKey{parent, name})
		return false, nil
	}
	return true, nil
}

// ClearAbsent drops every negative-cache entry under parent.
func (s *Store) ClearAbsent(ctx context.Context, parent uint64) error {
	s.absentMu.Lock()
	defer s.absentMu.Unlock()
	for k := range s.absent {
		if k.parent == parent {
			delete(s.absent, k)
		}
	}
	return nil
}

func (s *Store) markAbsent(parent uint64, name string, ttl time.Duration) {
	s.absentMu.Lock()
	defer s.absentMu.Unlock()
	// Bound the map: expired entries are dropped when it grows.
	if len(s.absent) > 65536 {
		now := s.now()
		for k, until := range s.absent {
			if !now.Before(until) {
				delete(s.absent, k)
			}
		}
	}
	s.absent[absentKey{parent, name}] = s.now().Add(ttl)
}

func (s *Store) clearAbsent(parent uint64, name string) {
	s.absentMu.Lock()
	delete(s.absent, absentKey{parent, name})
	s.absentMu.Unlock()
}

// absentCount is the number of live negative-cache entries.
func (s *Store) absentCount() int64 {
	s.absentMu.Lock()
	defer s.absentMu.Unlock()
	now := s.now()
	var n int64
	for _, until := range s.absent {
		if now.Before(until) {
			n++
		}
	}
	return n
}

// Cursor returns the stored delta cursor for a remote.
func (s *Store) Cursor(ctx context.Context, remote string) (string, error) {
	var c string
	err := s.db.QueryRowContext(ctx, `SELECT delta_cursor FROM remote_cursor WHERE remote = ?`, remote).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("meta: cursor: %w", err)
	}
	return c, nil
}

// SetCursor stores the delta cursor for a remote.
func (s *Store) SetCursor(ctx context.Context, remote, cursor string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO remote_cursor (remote, delta_cursor, updated_at) VALUES (?,?,?)
			 ON CONFLICT(remote) DO UPDATE SET delta_cursor=excluded.delta_cursor, updated_at=excluded.updated_at`,
			remote, cursor, s.now().Unix())
		if err != nil {
			return fmt.Errorf("meta: set cursor: %w", err)
		}
		return nil
	})
}

// Pin is a directory or file kept fully cached.
type Pin struct {
	Path      string
	Recursive bool
	Mode      string
}

// AddPin records a pin.
func (s *Store) AddPin(ctx context.Context, p Pin) error {
	if p.Mode == "" {
		p.Mode = "keep"
	}
	rec := 0
	if p.Recursive {
		rec = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO pins (path, recursive, mode) VALUES (?,?,?)`, p.Path, rec, p.Mode)
		if err != nil {
			return fmt.Errorf("meta: add pin: %w", err)
		}
		return nil
	})
}

// RemovePin drops a pin.
func (s *Store) RemovePin(ctx context.Context, path string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM pins WHERE path = ?`, path)
		if err != nil {
			return fmt.Errorf("meta: remove pin: %w", err)
		}
		return nil
	})
}

// Pins lists pins sorted by path.
func (s *Store) Pins(ctx context.Context) ([]Pin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, recursive, mode FROM pins ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("meta: pins: %w", err)
	}
	defer rows.Close()
	var out []Pin
	for rows.Next() {
		var p Pin
		var rec int
		if err := rows.Scan(&p.Path, &rec, &p.Mode); err != nil {
			return nil, fmt.Errorf("meta: pins: %w", err)
		}
		p.Recursive = rec != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// IsPinned reports whether p is covered by a pin (exact, or under a recursive
// pin).
func (s *Store) IsPinned(ctx context.Context, p string) (bool, error) {
	pins, err := s.Pins(ctx)
	if err != nil {
		return false, err
	}
	for _, pin := range pins {
		if pin.Path == p {
			return true, nil
		}
		if pin.Recursive && strings.HasPrefix(p, strings.TrimSuffix(pin.Path, "/")+"/") {
			return true, nil
		}
	}
	return false, nil
}

// SearchResult is one metadata name or path match, with the facts a result
// row shows: the nodes columns come through the bounded CTE at no extra
// query.
type SearchResult struct {
	Ino   uint64
	Name  string
	Path  string
	Kind  provider.Kind
	Size  int64
	MTime time.Time
	// Remote, RemoteID and Version identify the content so a caller can ask
	// the block cache; Cached is filled by vfs.FS.Search, never read from
	// meta, which knows nothing about the cache.
	Remote, RemoteID, Version string
	Cached                    bool
}

// Stats summarises the cache for `cloudfs status`.
type Stats struct {
	Nodes      int64
	Dirs       int64
	CompleteDs int64
	Absent     int64
	Pins       int64
	// LastCrawl is the newest listed_at over dir_state, zero when nothing
	// has been listed. The crawler and readdir share the table, so this
	// is the last time anything extended the index.
	LastCrawl time.Time
}

// Stats reads counters.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	q := func(dst *int64, query string, args ...any) error {
		return s.db.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	if err := q(&st.Nodes, `SELECT COUNT(*) FROM nodes WHERE ino != ?`, RootIno); err != nil {
		return st, fmt.Errorf("meta: stats: %w", err)
	}
	if err := q(&st.Dirs, `SELECT COUNT(*) FROM nodes WHERE kind = ? AND ino != ?`, int(provider.KindDir), RootIno); err != nil {
		return st, fmt.Errorf("meta: stats: %w", err)
	}
	if err := q(&st.CompleteDs, `SELECT COUNT(*) FROM dir_state WHERE complete = 1`); err != nil {
		return st, fmt.Errorf("meta: stats: %w", err)
	}
	st.Absent = s.absentCount()
	if err := q(&st.Pins, `SELECT COUNT(*) FROM pins`); err != nil {
		return st, fmt.Errorf("meta: stats: %w", err)
	}
	var lastListed int64
	if err := q(&lastListed, `SELECT COALESCE(MAX(listed_at), 0) FROM dir_state`); err != nil {
		return st, fmt.Errorf("meta: stats: %w", err)
	}
	if lastListed > 0 {
		st.LastCrawl = time.Unix(lastListed, 0)
	}
	return st, nil
}

// Vacuum runs integrity checks and reclaims space; `cloudfs doctor --fix` uses it.
func (s *Store) Vacuum(ctx context.Context) error {
	var res string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return fmt.Errorf("meta: integrity_check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("meta: integrity_check failed: %s", res)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("meta: checkpoint: %w", err)
	}
	return nil
}
