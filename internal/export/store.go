package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloudfs/internal/provider"

	_ "modernc.org/sqlite"
)

// schemaVersion is the exports.db layout. It is deliberately separate from
// the journal's: an export owns no staged blob and no upload row, so binding
// the two would force every daemon that can export to migrate a queue it does
// not otherwise touch.
const schemaVersion = 1

// Store is the durable export plan.
type Store struct {
	db   *sql.DB
	lock *os.File
	// owner reports whether this process holds the flock. Only the owner may
	// run jobs; everyone else may read.
	owner bool
	now   func() time.Time
	// writeMu serialises writers. SQLite would serialise them anyway, with
	// SQLITE_BUSY instead of a queue.
	writeMu sync.Mutex
}

// OpenStore creates or opens exports.db under dir.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("export: a store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	dsn := "file:" + filepath.Join(dir, "exports.db") +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
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
			err = fmt.Errorf("export: schema v%d requires its owner for migration; connect to the running daemon", current)
		}
	}
	if err != nil {
		releaseOwnership(lock)
		db.Close()
		return nil, err
	}
	return s, nil
}

// Owner reports whether this process may run jobs.
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
		return fmt.Errorf("export: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version > schemaVersion {
		return fmt.Errorf("export: exports.db is at schema v%d, newer than this build's v%d", version, schemaVersion)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS export_jobs (
  id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('planning','running','paused','done','failed','cancelled','purging')),
  pause_reason TEXT NOT NULL DEFAULT '',
  sources TEXT NOT NULL,
  dest TEXT NOT NULL,
  dest_dev INTEGER NOT NULL DEFAULT 0,
  options TEXT NOT NULL,
  meta_identity TEXT NOT NULL,
  bindings TEXT NOT NULL,
  files_total INTEGER NOT NULL DEFAULT 0, files_done INTEGER NOT NULL DEFAULT 0,
  files_skipped INTEGER NOT NULL DEFAULT 0, files_failed INTEGER NOT NULL DEFAULT 0,
  bytes_total INTEGER NOT NULL DEFAULT 0, bytes_done INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS export_items (
  job_id TEXT NOT NULL, rel TEXT NOT NULL,
  vpath TEXT NOT NULL, kind INTEGER NOT NULL,
  size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL,
  remote TEXT NOT NULL, remote_id TEXT NOT NULL, version TEXT NOT NULL,
  hash_type TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('pending','active','done','skipped','failed')),
  ranges TEXT NOT NULL DEFAULT '',
  done_bytes INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, rel)
)`,
		`CREATE INDEX IF NOT EXISTS export_items_state ON export_items(job_id, state, next_at)`,
		`CREATE TABLE IF NOT EXISTS export_extras (job_id TEXT NOT NULL, rel TEXT NOT NULL, kind INTEGER NOT NULL, PRIMARY KEY (job_id, rel))`,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer tx.Rollback()
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("export: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, fmt.Sprint(schemaVersion)); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return tx.Commit()
}

// CreateJob inserts a planned-but-not-walked job.
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	sources, err := json.Marshal(j.Sources)
	if err != nil {
		return err
	}
	options, err := json.Marshal(j.Options)
	if err != nil {
		return err
	}
	bindings, err := json.Marshal(j.Bindings)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO export_jobs
		(id, state, pause_reason, sources, dest, dest_dev, options, meta_identity, bindings, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, string(j.State), string(j.PauseReason), string(sources), j.Dest, int64(j.DestDev),
		string(options), j.MetaIdentity, string(bindings), j.CreatedAt.UnixNano(), j.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

const jobColumns = `id, state, pause_reason, sources, dest, dest_dev, options, meta_identity, bindings,
	files_total, files_done, files_skipped, files_failed, bytes_total, bytes_done,
	last_error, revision, created_at, updated_at, finished_at`

func scanJob(rows interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var state, reason, sources, options, bindings string
	var destDev, created, updated, finished int64
	if err := rows.Scan(&j.ID, &state, &reason, &sources, &j.Dest, &destDev, &options, &j.MetaIdentity, &bindings,
		&j.FilesTotal, &j.FilesDone, &j.FilesSkipped, &j.FilesFailed, &j.BytesTotal, &j.BytesDone,
		&j.LastError, &j.Revision, &created, &updated, &finished); err != nil {
		return Job{}, err
	}
	j.State, j.PauseReason, j.DestDev = State(state), PauseReason(reason), uint64(destDev)
	if err := json.Unmarshal([]byte(sources), &j.Sources); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal([]byte(options), &j.Options); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal([]byte(bindings), &j.Bindings); err != nil {
		return Job{}, err
	}
	j.CreatedAt, j.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	if finished != 0 {
		j.FinishedAt = time.Unix(0, finished)
	}
	return j, nil
}

// Job reads one job.
func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM export_jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("export: %w", err)
	}
	return j, nil
}

// Jobs lists jobs newest first. after is the id the previous page ended on;
// limit 0 means "no limit".
func (s *Store) Jobs(ctx context.Context, limit int, after string) ([]Job, string, error) {
	q := `SELECT ` + jobColumns + ` FROM export_jobs`
	args := []any{}
	if after != "" {
		// created_at descending, id as the tiebreak, so the cursor is the
		// last row of the previous page.
		var created int64
		if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM export_jobs WHERE id = ?`, after).Scan(&created); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, "", ErrNotFound
			}
			return nil, "", fmt.Errorf("export: %w", err)
		}
		q += ` WHERE (created_at, id) < (?, ?)`
		args = append(args, created, after)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, "", fmt.Errorf("export: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("export: %w", err)
	}
	cursor := ""
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		cursor = out[len(out)-1].ID
	}
	return out, cursor, nil
}

// UnfinishedJobs returns the jobs the manager still owns, oldest first.
func (s *Store) UnfinishedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM export_jobs
		WHERE state IN ('planning','running','paused','purging') ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetState moves a job, refusing the write when someone else moved it first.
// Every pause/resume/cancel goes through this compare-and-set, so a resume
// that raced a cancel loses instead of reviving the job.
func (s *Store) SetState(ctx context.Context, id string, revision int64, st State, reason PauseReason) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	finished := int64(0)
	if st.Terminal() {
		finished = s.now().UnixNano()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE export_jobs
		SET state = ?, pause_reason = ?, revision = revision + 1, updated_at = ?,
		    finished_at = CASE WHEN ? = 0 THEN finished_at ELSE ? END
		WHERE id = ? AND revision = ?`,
		string(st), string(reason), s.now().UnixNano(), finished, finished, id, revision)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if n == 0 {
		if _, err := s.Job(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("export: job %s changed underneath this request", id)
	}
	return nil
}

// SetError records the last thing that went wrong without moving the job.
func (s *Store) SetError(ctx context.Context, id, msg string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_jobs SET last_error = ?, updated_at = ? WHERE id = ?`,
		msg, s.now().UnixNano(), id)
	return err
}

// SetDestDev records the destination device observed at planning time.
func (s *Store) SetDestDev(ctx context.Context, id string, dev uint64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_jobs SET dest_dev = ?, updated_at = ? WHERE id = ?`,
		int64(dev), s.now().UnixNano(), id)
	return err
}

// AddItems writes one batch of planned entries.
func (s *Store) AddItems(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO export_items
		(job_id, rel, vpath, kind, size, mtime_ns, remote, remote_id, version, hash_type, hash, state, ranges, done_bytes, attempts, next_at, last_error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(job_id, rel) DO UPDATE SET
			vpath = excluded.vpath, kind = excluded.kind, size = excluded.size, mtime_ns = excluded.mtime_ns,
			remote = excluded.remote, remote_id = excluded.remote_id, version = excluded.version,
			hash_type = excluded.hash_type, hash = excluded.hash, state = excluded.state`)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer st.Close()
	for _, it := range items {
		next := int64(0)
		if !it.NextAt.IsZero() {
			next = it.NextAt.UnixNano()
		}
		if _, err := st.ExecContext(ctx, it.JobID, it.Rel, it.VPath, int(it.Kind), it.Size, it.MTime.UnixNano(),
			it.Remote, it.RemoteID, it.Version, it.HashType, it.Hash, string(it.State), it.Ranges, it.DoneBytes,
			it.Attempts, next, it.LastError); err != nil {
			return fmt.Errorf("export: %w", err)
		}
	}
	return tx.Commit()
}

// ClearPlan drops a job's items and extras, for a job that has to be planned
// again (one that was interrupted mid-plan, which has fetched nothing yet).
func (s *Store) ClearPlan(ctx context.Context, jobID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM export_items WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM export_extras WHERE job_id = ?`, jobID)
	return err
}

// AddExtras records the destination entries a mirror plans to delete.
func (s *Store) AddExtras(ctx context.Context, jobID string, extras []Extra) error {
	if len(extras) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer tx.Rollback()
	for _, e := range extras {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO export_extras (job_id, rel, kind) VALUES (?,?,?)`,
			jobID, e.Rel, int(e.Kind)); err != nil {
			return fmt.Errorf("export: %w", err)
		}
	}
	return tx.Commit()
}

// Extras lists the mirror deletions, deepest path first so a directory is
// removed after everything inside it.
func (s *Store) Extras(ctx context.Context, jobID string) ([]Extra, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel, kind FROM export_extras WHERE job_id = ? ORDER BY rel DESC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	var out []Extra
	for rows.Next() {
		var e Extra
		var kind int
		if err := rows.Scan(&e.Rel, &kind); err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		e.Kind = provider.Kind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}

const itemColumns = `job_id, rel, vpath, kind, size, mtime_ns, remote, remote_id, version,
	hash_type, hash, state, ranges, done_bytes, attempts, next_at, last_error`

func scanItem(rows interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var kind int
	var mtime, nextAt int64
	var state string
	if err := rows.Scan(&it.JobID, &it.Rel, &it.VPath, &kind, &it.Size, &mtime, &it.Remote, &it.RemoteID, &it.Version,
		&it.HashType, &it.Hash, &state, &it.Ranges, &it.DoneBytes, &it.Attempts, &nextAt, &it.LastError); err != nil {
		return Item{}, err
	}
	it.Kind, it.State = provider.Kind(kind), ItemState(state)
	it.MTime = time.Unix(0, mtime)
	if nextAt != 0 {
		it.NextAt = time.Unix(0, nextAt)
	}
	return it, nil
}

// Items lists a job's plan in path order.
func (s *Store) Items(ctx context.Context, jobID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM export_items WHERE job_id = ? ORDER BY rel`, jobID)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemsPage returns a bounded, stable page of a job's plan. The relative
// path is both the sort key and cursor, so this does not require loading a
// million-row export into memory merely to inspect its failures.
func (s *Store) ItemsPage(ctx context.Context, jobID, after string, limit int, state ItemState) ([]Item, string, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	args := []any{jobID, after}
	q := `SELECT ` + itemColumns + ` FROM export_items WHERE job_id = ? AND rel > ?`
	if state != "" {
		q += ` AND state = ?`
		args = append(args, string(state))
	}
	q += ` ORDER BY rel LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	out := make([]Item, 0, limit+1)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, "", fmt.Errorf("export: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].Rel
		out = out[:limit]
	}
	return out, next, nil
}

// Item reads one planned entry.
func (s *Store) Item(ctx context.Context, jobID, rel string) (Item, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM export_items WHERE job_id = ? AND rel = ?`, jobID, rel)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("export: %w", err)
	}
	return it, nil
}

// DueItems returns files that are ready to transfer. Big files come first:
// they are the ones worth several streams, and starting them last would end
// the job with one large file trickling in alone.
func (s *Store) DueItems(ctx context.Context, jobID string, now time.Time, multiRangeMin int64, limit int) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM export_items
		WHERE job_id = ? AND state = 'pending' AND kind = ? AND next_at <= ?
		ORDER BY CASE WHEN size >= ? THEN 0 ELSE 1 END, rel LIMIT ?`,
		jobID, int(provider.KindFile), now.UnixNano(), multiRangeMin, limit)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Pending reports how much work is left and when the earliest deferred item
// comes due, which is what tells a stalled job when to look again.
func (s *Store) Pending(ctx context.Context, jobID string) (count int, earliest time.Time, err error) {
	var n int
	var min sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(next_at) FROM export_items
		WHERE job_id = ? AND state IN ('pending','active') AND kind = ?`, jobID, int(provider.KindFile)).Scan(&n, &min)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("export: %w", err)
	}
	if min.Valid && min.Int64 > 0 {
		earliest = time.Unix(0, min.Int64)
	}
	return n, earliest, nil
}

// Claim moves one pending item to active. It returns false when another
// worker took it first.
func (s *Store) Claim(ctx context.Context, jobID, rel string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE export_items SET state = 'active' WHERE job_id = ? AND rel = ? AND state = 'pending'`, jobID, rel)
	if err != nil {
		return false, fmt.Errorf("export: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Checkpoint records the ranges that are durably on the destination. It runs
// a few times a second on a big file, which is what makes a resume exact.
func (s *Store) Checkpoint(ctx context.Context, jobID, rel, ranges string, doneBytes int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE export_items SET ranges = ?, done_bytes = ? WHERE job_id = ? AND rel = ?`,
		ranges, doneBytes, jobID, rel); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE export_jobs SET
		bytes_done = (SELECT COALESCE(SUM(done_bytes),0) FROM export_items WHERE job_id = ? AND kind = 0),
		updated_at = ? WHERE id = ?`, jobID, s.now().UnixNano(), jobID); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// FinishItem moves an item to a terminal state and refreshes the job's
// counters from the plan.
func (s *Store) FinishItem(ctx context.Context, jobID, rel string, st ItemState, doneBytes int64, msg string) error {
	s.writeMu.Lock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_items SET state = ?, done_bytes = ?, last_error = ? WHERE job_id = ? AND rel = ?`,
		string(st), doneBytes, msg, jobID, rel)
	s.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return s.RefreshCounters(ctx, jobID)
}

// ReleaseItem puts an item back on the queue, optionally after a delay.
// spendAttempt distinguishes a real failure from a deferral: a backend that
// cannot be reached must not consume the retry budget, or a pool that is
// offline for an afternoon would fail every file planned that day.
func (s *Store) ReleaseItem(ctx context.Context, jobID, rel string, at time.Time, spendAttempt bool, msg string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	next := int64(0)
	if !at.IsZero() {
		next = at.UnixNano()
	}
	q := `UPDATE export_items SET state = 'pending', next_at = ?, last_error = ? WHERE job_id = ? AND rel = ?`
	if spendAttempt {
		q = `UPDATE export_items SET state = 'pending', next_at = ?, last_error = ?, attempts = attempts + 1 WHERE job_id = ? AND rel = ?`
	}
	if _, err := s.db.ExecContext(ctx, q, next, msg, jobID, rel); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// ClearBackoff makes every waiting item due now. A user resume is a
// statement that whatever the job was waiting for is over, so the job should
// not then sit through the rest of an hour-long back-off.
func (s *Store) ClearBackoff(ctx context.Context, jobID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_items SET next_at = 0 WHERE job_id = ? AND state = 'pending'`, jobID)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// ResetActive puts everything a stopped daemon had in flight back on the
// queue. Its bytes and its bitmap stay: they are on the destination disk.
func (s *Store) ResetActive(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_items SET state = 'pending' WHERE state = 'active'`)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// RefreshCounters recomputes a job's totals from its plan. Deriving them
// rather than incrementing is what keeps them right across a restart, where
// an item that was in flight comes back with bytes already on disk.
func (s *Store) RefreshCounters(ctx context.Context, jobID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE export_jobs SET
		files_total   = (SELECT COUNT(*) FROM export_items WHERE job_id = ? AND kind = 0),
		files_done    = (SELECT COUNT(*) FROM export_items WHERE job_id = ? AND kind = 0 AND state = 'done'),
		files_skipped = (SELECT COUNT(*) FROM export_items WHERE job_id = ? AND kind = 0 AND state = 'skipped'),
		files_failed  = (SELECT COUNT(*) FROM export_items WHERE job_id = ? AND kind = 0 AND state = 'failed'),
		bytes_total   = (SELECT COALESCE(SUM(size),0) FROM export_items WHERE job_id = ? AND kind = 0),
		bytes_done    = (SELECT COALESCE(SUM(done_bytes),0) FROM export_items WHERE job_id = ? AND kind = 0),
		updated_at = ?
		WHERE id = ?`, jobID, jobID, jobID, jobID, jobID, jobID, s.now().UnixNano(), jobID)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// DeleteJob removes a job and its plan.
func (s *Store) DeleteJob(ctx context.Context, jobID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM export_items WHERE job_id = ?`,
		`DELETE FROM export_extras WHERE job_id = ?`,
		`DELETE FROM export_jobs WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, jobID); err != nil {
			return fmt.Errorf("export: %w", err)
		}
	}
	return tx.Commit()
}
