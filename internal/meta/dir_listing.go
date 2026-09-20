package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const DirListingBatch = 200

var ErrListingChanged = errors.New("meta: directory listing target changed or a newer refresh started")

// DirListing spools one authoritative provider listing in a connection-private
// SQLite TEMP database with a bounded page cache. Close releases that database;
// an interrupted collection never publishes a partial directory. Callers must
// Close on every path. It is not a durable download/checkpoint job.
type DirListing struct {
	mu                        sync.Mutex
	s                         *Store
	db                        *sql.DB
	parent                    Node
	identity                  string
	generation                int64
	staleGeneration           int64
	publishedStale            bool
	closed, committed, failed bool
}

func (s *Store) BeginDirListing(ctx context.Context, expected Node) (*DirListing, error) {
	dir := expected.Ino
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.listingStop:
		return nil, errors.New("meta: store closed")
	case s.listingSlots <- struct{}{}:
	}
	l := &DirListing{s: s}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		l.parent, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE ino=?`, dir))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !l.parent.IsDir() {
			return errors.New("meta: listing target is not a directory")
		}
		if l.parent.Remote != expected.Remote || l.parent.RemoteID != expected.RemoteID {
			return ErrListingChanged
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&l.identity); err != nil {
			return err
		}
		// The hard counter advances so that a listing started earlier is
		// fenced by this one; the soft counter is only observed.
		return tx.QueryRowContext(ctx, `INSERT INTO directory_refresh_generation(ino,generation,stale_generation) VALUES(?,1,0)
ON CONFLICT(ino) DO UPDATE SET generation=generation+1
RETURNING generation, stale_generation`, dir).Scan(&l.generation, &l.staleGeneration)
	})
	if err != nil {
		<-s.listingSlots
		return nil, err
	}
	// A dedicated connection does not reserve a slot in the ordinary metadata
	// pool while waiting on provider IO. All main writes still use writeMu.
	dsn := s.dsn
	if !strings.Contains(dsn, "_txlock=") {
		dsn += "&_txlock=immediate"
	}
	l.db, err = sql.Open("sqlite", dsn)
	if err == nil {
		l.db.SetMaxOpenConns(1)
		l.db.SetMaxIdleConns(1)
		var identity string
		err = l.db.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&identity)
		if err == nil && (identity == "" || identity != l.identity) {
			err = ErrListingChanged
		}
	}
	if err == nil {
		var forcedMemory bool
		err = l.db.QueryRowContext(ctx, `SELECT sqlite_compileoption_used('TEMP_STORE=3')`).Scan(&forcedMemory)
		if err == nil && forcedMemory {
			err = errors.New("meta: SQLite build forces temporary listings into memory")
		}
	}
	if err == nil {
		_, err = l.db.ExecContext(ctx, `PRAGMA temp_store=FILE;
PRAGMA temp.cache_size=-2048;
PRAGMA temp.cache_spill=ON;
CREATE TEMP TABLE listing_nodes(name TEXT PRIMARY KEY, body BLOB NOT NULL) WITHOUT ROWID;
CREATE TEMP TABLE listing_changes(name TEXT PRIMARY KEY, ino INTEGER NOT NULL, kind TEXT NOT NULL) WITHOUT ROWID;
CREATE TEMP TABLE listing_deleted(ino INTEGER PRIMARY KEY);
CREATE TEMP TABLE listing_cursors(cursor TEXT PRIMARY KEY) WITHOUT ROWID;`)
	}
	if err != nil {
		if l.db != nil {
			l.db.Close()
		}
		<-s.listingSlots
		return nil, err
	}
	s.listingMu.Lock()
	select {
	case <-s.listingStop:
		s.listingMu.Unlock()
		l.db.Close()
		<-s.listingSlots
		return nil, errors.New("meta: store closed")
	default:
	}
	s.listings[l] = struct{}{}
	s.listingMu.Unlock()
	return l, nil
}

func (l *DirListing) collecting() error {
	if l.closed || l.failed || l.committed {
		return errors.New("meta: directory listing is no longer collecting")
	}
	return nil
}

// RecordCursor detects arbitrary provider cursor cycles without a growing Go
// map. Call before each List request, including the first empty cursor.
func (l *DirListing) RecordCursor(ctx context.Context, cursor string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.collecting(); err != nil {
		return err
	}
	if len(cursor) > 1<<20 {
		l.failed = true
		return errors.New("meta: provider cursor exceeds limit")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO temp.listing_cursors(cursor) VALUES(?)`, cursor)
	if err != nil {
		l.failed = true
		return fmt.Errorf("meta: repeated or unavailable listing cursor: %w", err)
	}
	return nil
}

// Append accepts bounded batches; duplicates or storage failures poison this
// collection so a caller cannot accidentally publish an incomplete snapshot.
func (l *DirListing) Append(ctx context.Context, nodes []Node) (err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err = l.collecting(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			l.failed = true
		}
	}()
	if len(nodes) > DirListingBatch {
		return errors.New("meta: listing batch exceeds limit")
	}
	l.s.writeMu.Lock()
	defer l.s.writeMu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO temp.listing_nodes(name,body) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, n := range nodes {
		if n.Name == "" || n.Name == "." || n.Name == ".." || len(n.Name) > 4096 || !utf8.ValidString(n.Name) || strings.ContainsAny(n.Name, "/\x00") {
			return errors.New("meta: invalid name in provider listing")
		}
		b, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err = stmt.ExecContext(ctx, n.Name, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (l *DirListing) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.db.Close()
	l.s.listingMu.Lock()
	delete(l.s.listings, l)
	l.s.listingMu.Unlock()
	<-l.s.listingSlots
	return err
}

func (s *Store) closeListings() {
	s.listingMu.Lock()
	close(s.listingStop)
	active := make([]*DirListing, 0, len(s.listings))
	for l := range s.listings {
		active = append(active, l)
	}
	s.listingMu.Unlock()
	for _, l := range active {
		_ = l.Close()
	}
}

// Commit atomically merges the complete staged listing and publishes its TTL.
// All protection checks run against current nodes under the writer lock.
// Neither the cached directory nor the change spool grows a full Go slice.
func (l *DirListing) Commit(ctx context.Context, childTTL time.Duration, protect func(Node) bool) (err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err = l.collecting(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			l.failed = true
		}
	}()
	l.s.writeMu.Lock()
	defer l.s.writeMu.Unlock()
	l.s.writeTx.Add(1)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var identity string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&identity); err != nil {
		return err
	}
	if identity != l.identity {
		return ErrListingChanged
	}
	current, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE ino=?`, l.parent.Ino))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrListingChanged
	}
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Remote != l.parent.Remote || current.RemoteID != l.parent.RemoteID {
		return ErrListingChanged
	}
	var generation, staleGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT generation, stale_generation FROM directory_refresh_generation WHERE ino=?`,
		current.Ino).Scan(&generation, &staleGeneration); err != nil {
		return err
	}
	if generation != l.generation {
		return ErrListingChanged
	}
	// Something marked this directory stale while the snapshot was being
	// collected, but nothing removed a name from it. The snapshot is still a
	// truthful listing, so it is published — and left incomplete, so the next
	// reader goes back to the backend for whatever the mark was about.
	// Refusing here instead is what made a library scan racing the first
	// change-feed backlog fail on directory after directory.
	stale := staleGeneration != l.staleGeneration
	now := l.s.now()
	if err := l.mergeStaged(ctx, tx, now, childTTL, protect); err != nil {
		return err
	}
	if err := l.removeMissing(ctx, tx, protect); err != nil {
		return err
	}
	// Entries the change feed wrote after this snapshot began are not part of
	// what it confirmed, so it does not get to call them freshly fetched
	// either; the next read goes back to the backend for them.
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET fetched_at=?,ttl_s=? WHERE parent_ino=? AND ino!=? AND dirty=0 AND applied_gen<?`,
		now.Unix(), int64(childTTL.Seconds()), current.Ino, RootIno, l.generation); err != nil {
		return err
	}
	complete, dirty := 1, 0
	if stale {
		complete, dirty = 0, 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dir_state(ino,complete,listed_at,cursor,dirty) VALUES(?,?,?,'',?)
ON CONFLICT(ino) DO UPDATE SET complete=excluded.complete,listed_at=excluded.listed_at,cursor='',dirty=excluded.dirty`,
		current.Ino, complete, now.Unix(), dirty); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.committed, l.publishedStale = true, stale
	// The negative cache itself is bounded. Clearing the parent avoids
	// materializing every added name just to invalidate negative lookups.
	return l.s.ClearAbsent(ctx, current.Ino)
}

// PublishedStale reports whether Commit published this snapshot but left the
// directory incomplete because it was marked stale while being collected. The
// entries are usable; the directory just is not cacheable as a whole.
func (l *DirListing) PublishedStale() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.publishedStale
}

func (l *DirListing) mergeStaged(ctx context.Context, tx *sql.Tx, now time.Time, childTTL time.Duration, protect func(Node) bool) error {
	after := ""
	update, err := tx.PrepareContext(ctx, `UPDATE nodes SET kind=?,size=?,mtime_ns=?,mode=CASE WHEN mode_set=1 THEN mode ELSE ? END,remote=?,remote_id=?,version=?,remote_version=?,hash_type=?,hash=?,fetched_at=?,ttl_s=?,dirty=0 WHERE ino=?`)
	if err != nil {
		return err
	}
	defer update.Close()
	for {
		rows, err := tx.QueryContext(ctx, `SELECT body FROM temp.listing_nodes WHERE name>? ORDER BY name LIMIT ?`, after, DirListingBatch)
		if err != nil {
			return err
		}
		var batch []Node
		for rows.Next() {
			var b []byte
			var n Node
			if err = rows.Scan(&b); err == nil {
				err = json.Unmarshal(b, &n)
			}
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		after = batch[len(batch)-1].Name
		// Only current nodes matching this bounded batch enter the map.
		args := []any{l.parent.Ino, RootIno}
		for _, n := range batch {
			args = append(args, n.Name)
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE parent_ino=? AND ino!=? AND name IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return err
		}
		old := make(map[string]Node, len(batch))
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				rows.Close()
				return err
			}
			old[n.Name] = n
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		applied, err := l.appliedAfterStartTx(ctx, tx, batch)
		if err != nil {
			return err
		}
		var inserts []Node
		for _, n := range batch {
			previous, exists := old[n.Name]
			// The change feed wrote this entry after the snapshot began, so
			// the snapshot is the older of the two and does not get to undo
			// it. This holds without a protect predicate: it is the store's
			// invariant, not an agreement with the caller.
			if exists && applied[n.Name] {
				continue
			}
			if exists && protect != nil && protect(previous) {
				continue
			}
			n.ParentIno, n.FetchedAt = l.parent.Ino, now
			if n.TTL == 0 {
				n.TTL = childTTL
			}
			fillMode(&n)
			if !exists {
				inserts = append(inserts, n)
				if _, err := tx.ExecContext(ctx, `INSERT INTO temp.listing_changes(name,ino,kind) VALUES(?,0,'add')`, n.Name); err != nil {
					return err
				}
				continue
			}
			if directoryReplacement(previous, n) {
				retained, err := prepareDirectoryReplacementTx(ctx, tx, previous, protect)
				if err != nil {
					return err
				}
				if retained {
					continue
				}
				inserts = append(inserts, n)
				if _, err := tx.ExecContext(ctx, `INSERT INTO temp.listing_changes(name,ino,kind) VALUES(?,?,'replace')`, n.Name, previous.Ino); err != nil {
					return err
				}
				continue
			}
			if previous.Version == n.Version && previous.Size == n.Size && previous.Kind == n.Kind && previous.RemoteID == n.RemoteID && previous.RemoteVersion == n.RemoteVersion && previous.Remote == n.Remote {
				continue
			}
			if _, err := update.ExecContext(ctx, int(n.Kind), n.Size, n.MTime.UnixNano(), n.Mode, n.Remote, n.RemoteID, n.Version, n.RemoteVersion, n.HashType, n.Hash, now.Unix(), int64(n.TTL.Seconds()), previous.Ino); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO temp.listing_changes(name,ino,kind) VALUES(?,?,'update')`, n.Name, previous.Ino); err != nil {
				return err
			}
		}
		if err := insertNodesTx(tx, inserts); err != nil {
			return err
		}
	}
}

func (l *DirListing) removeMissing(ctx context.Context, tx *sql.Tx, protect func(Node) bool) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE parent_ino=? AND ino!=? AND name>?
AND NOT EXISTS(SELECT 1 FROM temp.listing_nodes s WHERE s.name=nodes.name) ORDER BY name LIMIT ?`, l.parent.Ino, RootIno, after, DirListingBatch)
		if err != nil {
			return err
		}
		var batch []Node
		for rows.Next() {
			n, err := scanNode(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].Name
		applied, err := l.appliedAfterStartTx(ctx, tx, batch)
		if err != nil {
			return err
		}
		for _, n := range batch {
			// Written by the change feed after this snapshot began: the
			// snapshot simply predates the entry, and removing it would undo
			// a newer write.
			if applied[n.Name] {
				continue
			}
			if protect != nil && protect(n) {
				continue
			}
			if n.IsDir() && protect != nil {
				protected, err := protectedSubtreeTx(ctx, tx, n.Ino, protect)
				if err != nil {
					return err
				}
				if protected {
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO temp.listing_changes(name,ino,kind) VALUES(?,?,'remove')`, n.Name, n.Ino); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO temp.listing_deleted(ino)
WITH RECURSIVE subtree(ino) AS (VALUES(?) UNION SELECT n.ino FROM nodes n JOIN subtree p ON n.parent_ino=p.ino WHERE n.ino!=?) SELECT ino FROM subtree`, n.Ino, RootIno); err != nil {
				return err
			}
		}
	}
	// Keep nodes until the auxiliary indices have been removed. The TEMP
	// inode set also bounds Go memory when a vanished directory is enormous.
	for _, q := range []string{
		`DELETE FROM dir_state WHERE ino IN (SELECT ino FROM temp.listing_deleted)`,
		`DELETE FROM name_index WHERE rowid IN (SELECT ino FROM temp.listing_deleted)`,
		`DELETE FROM name_indexed WHERE ino IN (SELECT ino FROM temp.listing_deleted)`,
		`DELETE FROM name_index_pending WHERE ino IN (SELECT ino FROM temp.listing_deleted)`,
		`DELETE FROM nodes WHERE ino IN (SELECT ino FROM temp.listing_deleted)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// A remote listing may omit a clean directory that contains an unuploaded
// local file. Protecting only its immediate child would delete that file's
// metadata along with the ancestor. Inspect descendants in SQLite, not a Go
// subtree slice, before admitting the removal.
func protectedSubtreeTx(ctx context.Context, tx *sql.Tx, ino uint64, protect func(Node) bool) (bool, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE subtree(ino) AS
(SELECT ino FROM nodes WHERE parent_ino=? AND ino!=? UNION SELECT n.ino FROM nodes n JOIN subtree p ON n.parent_ino=p.ino WHERE n.ino!=?)
SELECT `+nodeCols+` FROM nodes WHERE ino IN (SELECT ino FROM subtree)`, ino, RootIno, RootIno)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return false, err
		}
		if protect(n) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Changes streams committed changes without accumulating a full directory's
// names/IDs. Callbacks run outside the writer transaction, after publication.
func (l *DirListing) Changes(ctx context.Context, visit func(DirChange)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.committed {
		return errors.New("meta: listing changes unavailable before commit or after close")
	}
	rows, err := l.db.QueryContext(ctx, `SELECT name,ino,kind FROM temp.listing_changes ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var change DirChange
	for rows.Next() {
		var name, kind string
		var ino uint64
		if err := rows.Scan(&name, &ino, &kind); err != nil {
			return err
		}
		cost := 1
		if kind == "replace" {
			cost = 3
		}
		if len(change.Added)+len(change.Removed)+len(change.Updated)+cost > 128 {
			visit(change)
			change = DirChange{}
		}
		switch kind {
		case "replace":
			change.Added = append(change.Added, name)
			change.Removed = append(change.Removed, name)
			change.Updated = append(change.Updated, ino)
		case "add":
			change.Added = append(change.Added, name)
		case "remove":
			change.Removed = append(change.Removed, name)
		case "update":
			change.Updated = append(change.Updated, ino)
		default:
			return errors.New("meta: invalid listing change kind")
		}
		if len(change.Added)+len(change.Removed)+len(change.Updated) == 128 {
			visit(change)
			change = DirChange{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if change.Any() {
		visit(change)
	}
	return nil
}

// Summary caps counting at the notification batch limit. Large changes can
// invalidate a subtree without loading all changed names or spawning an
// unbounded number of kernel notification goroutines.
func (l *DirListing) Summary(ctx context.Context) (count int, removed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.committed {
		return 0, false, errors.New("meta: listing summary unavailable")
	}
	err = l.db.QueryRowContext(ctx, `SELECT
(SELECT MIN(129,COALESCE(SUM(CASE kind WHEN 'replace' THEN 3 ELSE 1 END),0)) FROM (SELECT kind FROM temp.listing_changes LIMIT 129)),
EXISTS(SELECT 1 FROM temp.listing_changes WHERE kind IN ('remove','replace'))`).Scan(&count, &removed)
	return
}

// appliedAfterStartTx names the entries in batch that the change feed wrote
// after this listing began. applied_gen carries the parent's generation at the
// time of that write, and this listing's generation was taken when it started,
// so "at least ours" means "not older than us".
func (l *DirListing) appliedAfterStartTx(ctx context.Context, tx *sql.Tx, batch []Node) (map[string]bool, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	args := []any{l.parent.Ino, RootIno, l.generation}
	for _, n := range batch {
		args = append(args, n.Name)
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM nodes WHERE parent_ino=? AND ino!=? AND applied_gen>=? AND name IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}
