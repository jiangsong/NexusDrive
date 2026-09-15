package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Delivery states. A row is pending until its due_at passes and a worker
// claims it (running); it ends done, or goes back to pending with a later
// due_at after a failure, or is parked dead once the retries are spent.
const (
	DeliveryPending = "pending"
	DeliveryRunning = "running"
	DeliveryDone    = "done"
	DeliveryDead    = "dead"
)

// DeliveryStates lists every state a query may filter on.
var DeliveryStates = []string{DeliveryPending, DeliveryRunning, DeliveryDone, DeliveryDead}

// OutputTruncatedMarker is what the runner appends to a stream it cut at
// its size cap. The JSON view derives Truncated from it, so the table
// needs no extra column.
const OutputTruncatedMarker = "\n[cloudfs: output truncated]"

// ErrDeliveryNotFound is returned for an id no row has.
var ErrDeliveryNotFound = errors.New("agent: delivery not found")

// ErrDeliveryNotDead is returned when Retry is asked to reopen a row that
// is not dead: pending and running rows are already on their way, done
// ones have nothing to redo.
var ErrDeliveryNotDead = errors.New("agent: only a dead delivery can be retried")

// Delivery is one trigger_deliveries row: a rule matched a path and the
// action must run at least once (docs/agent-roadmap.md §5.3).
type Delivery struct {
	ID        int64     `json:"id"`
	Rule      string    `json:"rule"`
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Origin    string    `json:"origin"`
	FirstSeen time.Time `json:"first_seen"`
	DueAt     time.Time `json:"due_at"`
	Attempts  int       `json:"attempts"`
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	// Output is what the action produced: the exec's stdout, then its stderr
	// after a separator, each already cut to the runner's cap; or the
	// webhook's status line and response. Truncated says a cap was hit.
	Output    string    `json:"output,omitempty"`
	Truncated bool      `json:"truncated"`
	DoneAt    time.Time `json:"done_at,omitzero"`
}

// DeliveryQuery filters and pages List. Rows come newest first; Cursor is
// the id of the last row of the previous page.
type DeliveryQuery struct {
	Rule   string
	State  string
	Cursor string
	Limit  int // default 100, max 1000
}

// Deliveries is the DAO over trigger_deliveries. The trigger engine is its
// only writer; the control plane reads it and calls Retry.
type Deliveries struct {
	s *Store
}

// Deliveries returns the DAO over this store's trigger_deliveries table.
func (s *Store) Deliveries() *Deliveries { return &Deliveries{s: s} }

const deliveryColumns = `SELECT id, rule, path, kind, origin, first_seen, due_at, attempts, state, last_error, output, done_at FROM trigger_deliveries`

// Enqueue records that rule matched path and must run at due. The partial
// unique index on (rule, path) WHERE state='pending' is the debounce: a
// second event inside the window hits it, INSERT OR IGNORE inserts nothing,
// and merged reports that the earlier row (whose id is returned) will
// carry both. A running row does not block a new pending one, so a change
// made during an action is delivered again afterwards.
func (q *Deliveries) Enqueue(ctx context.Context, rule, path, kind, origin string, due time.Time) (int64, bool, error) {
	if q.s.readOnly {
		return 0, false, errors.New("agent: the store is read-only")
	}
	now := q.s.now()
	res, err := q.s.db.ExecContext(ctx, `INSERT OR IGNORE INTO trigger_deliveries(rule, path, kind, origin, first_seen, due_at, state)
		VALUES (?, ?, ?, ?, ?, ?, 'pending')`, rule, path, kind, origin, now.UnixNano(), due.UnixNano())
	if err != nil {
		return 0, false, fmt.Errorf("agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("agent: %w", err)
	}
	if n == 0 {
		var id int64
		err := q.s.db.QueryRowContext(ctx, `SELECT id FROM trigger_deliveries WHERE rule = ? AND path = ? AND state = 'pending'`, rule, path).Scan(&id)
		if err != nil {
			return 0, false, fmt.Errorf("agent: %w", err)
		}
		return id, true, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("agent: %w", err)
	}
	q.publish(ctx, id)
	return id, false, nil
}

// Claim takes the oldest pending row of rule whose due_at has passed,
// marks it running and counts the attempt. ok is false when nothing is
// due. Workers poll this every tick, so the common "nothing due" answer
// is a plain read that takes no write lock and cannot stall a burst of
// enqueues; only a hit pays for the update. Workers are one per rule, so
// a candidate vanishing between the two statements (a fold) just means
// looking again.
func (q *Deliveries) Claim(ctx context.Context, rule string, now time.Time) (Delivery, bool, error) {
	if q.s.readOnly {
		return Delivery{}, false, errors.New("agent: the store is read-only")
	}
	for {
		var id int64
		err := q.s.db.QueryRowContext(ctx, `SELECT id FROM trigger_deliveries WHERE rule = ? AND state = 'pending' AND due_at <= ?
			ORDER BY due_at, id LIMIT 1`, rule, now.UnixNano()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, false, nil
		}
		if err != nil {
			return Delivery{}, false, fmt.Errorf("agent: %w", err)
		}
		row := q.s.db.QueryRowContext(ctx, `UPDATE trigger_deliveries SET state = 'running', attempts = attempts + 1
			WHERE id = ? AND state = 'pending'
			RETURNING id, rule, path, kind, origin, first_seen, due_at, attempts, state, last_error, output, done_at`, id)
		d, err := scanDelivery(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Delivery{}, false, err
		}
		q.s.publish(Event{Kind: "trigger", Delivery: &d})
		return d, true, nil
	}
}

// Done ends a running row successfully.
func (q *Deliveries) Done(ctx context.Context, id int64, output string) error {
	return q.transition(ctx, id, `UPDATE trigger_deliveries SET state = 'done', output = ?, last_error = '', done_at = ? WHERE id = ?`,
		output, q.s.now().UnixNano(), id)
}

// Fail puts a running row back to pending, to be claimed again at nextDue.
// The attempt count stays, which is what the backoff and the dead limit
// are computed from. A pending duplicate for the same (rule, path) that
// was queued during the run folds into this row: the path will be
// delivered again either way, and keeping the older row keeps its history.
func (q *Deliveries) Fail(ctx context.Context, id int64, errText, output string, nextDue time.Time) error {
	return q.reopen(ctx, id, `UPDATE trigger_deliveries SET state = 'pending', due_at = ?, last_error = ?, output = ?, done_at = 0 WHERE id = ? AND state = 'running'`,
		nextDue.UnixNano(), errText, output, id)
}

// Dead parks a running row for a human: the retries are spent (or the
// action is one that must not be retried blindly).
func (q *Deliveries) Dead(ctx context.Context, id int64, errText, output string) error {
	return q.transition(ctx, id, `UPDATE trigger_deliveries SET state = 'dead', last_error = ?, output = ?, done_at = ? WHERE id = ?`,
		errText, output, q.s.now().UnixNano(), id)
}

// Retry reopens a dead row: pending, due now, attempts kept so the history
// stays honest. Anything but a dead row is refused with ErrDeliveryNotDead.
func (q *Deliveries) Retry(ctx context.Context, id int64) error {
	if q.s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	var state string
	err := q.s.db.QueryRowContext(ctx, `SELECT state FROM trigger_deliveries WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDeliveryNotFound
	}
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if state != DeliveryDead {
		return ErrDeliveryNotDead
	}
	return q.reopen(ctx, id, `UPDATE trigger_deliveries SET state = 'pending', due_at = ?, done_at = 0 WHERE id = ? AND state = 'dead'`,
		q.s.now().UnixNano(), id)
}

// ResetRunning turns every running row back to pending, due now, and
// reports how many. The owner calls it once at start: a row still running
// is one the previous process died in the middle of, and at-least-once
// means it runs again. A pending duplicate queued after the crash folds
// into the older row, as in Fail.
func (q *Deliveries) ResetRunning(ctx context.Context) (int64, error) {
	if q.s.readOnly {
		return 0, errors.New("agent: the store is read-only")
	}
	tx, err := q.s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, rule, path FROM trigger_deliveries WHERE state = 'running' ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	type key struct {
		id         int64
		rule, path string
	}
	var running []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.id, &k.rule, &k.path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("agent: %w", err)
		}
		running = append(running, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	now := q.s.now().UnixNano()
	for _, k := range running {
		if _, err := tx.ExecContext(ctx, `DELETE FROM trigger_deliveries WHERE rule = ? AND path = ? AND state = 'pending' AND id <> ?`, k.rule, k.path, k.id); err != nil {
			return 0, fmt.Errorf("agent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE trigger_deliveries SET state = 'pending', due_at = ?, done_at = 0 WHERE id = ?`, now, k.id); err != nil {
			return 0, fmt.Errorf("agent: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	for _, k := range running {
		q.publish(ctx, k.id)
	}
	return int64(len(running)), nil
}

// Get returns one row.
func (q *Deliveries) Get(ctx context.Context, id int64) (Delivery, error) {
	d, err := scanDelivery(q.s.db.QueryRowContext(ctx, deliveryColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrDeliveryNotFound
	}
	return d, err
}

// List returns rows newest first and, when more remain, the cursor for the
// next page.
func (q *Deliveries) List(ctx context.Context, query DeliveryQuery) ([]Delivery, string, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"1 = 1"}
	args := []any{}
	if query.Cursor != "" {
		last, err := strconv.ParseInt(query.Cursor, 10, 64)
		if err != nil || last <= 0 {
			return nil, "", ErrInvalidCursor
		}
		where = append(where, "id < ?")
		args = append(args, last)
	}
	if query.Rule != "" {
		where = append(where, "rule = ?")
		args = append(args, query.Rule)
	}
	if query.State != "" {
		known := false
		for _, s := range DeliveryStates {
			known = known || s == query.State
		}
		if !known {
			return nil, "", fmt.Errorf("agent: unknown delivery state %q; choose from %s", query.State, strings.Join(DeliveryStates, ", "))
		}
		where = append(where, "state = ?")
		args = append(args, query.State)
	}
	args = append(args, limit+1)
	rows, err := q.s.db.QueryContext(ctx, deliveryColumns+` WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = strconv.FormatInt(out[len(out)-1].ID, 10)
	}
	return out, next, nil
}

// Counts reports how many rows wait and how many are dead, for the status
// endpoint and the console badge.
func (q *Deliveries) Counts(ctx context.Context) (pending, dead int, err error) {
	err = q.s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN state = 'pending' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN state = 'dead' THEN 1 ELSE 0 END), 0) FROM trigger_deliveries`).Scan(&pending, &dead)
	if err != nil {
		return 0, 0, fmt.Errorf("agent: %w", err)
	}
	return pending, dead, nil
}

// transition runs one UPDATE by id and publishes the row it left behind.
func (q *Deliveries) transition(ctx context.Context, id int64, query string, args ...any) error {
	if q.s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	res, err := q.s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeliveryNotFound
	}
	q.publish(ctx, id)
	return nil
}

// reopen moves row id back to pending with query, first folding away any
// other pending row for the same (rule, path) so the unique index holds.
// Both statements run in one immediate transaction.
func (q *Deliveries) reopen(ctx context.Context, id int64, query string, args ...any) error {
	if q.s.readOnly {
		return errors.New("agent: the store is read-only")
	}
	tx, err := q.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	var rule, path string
	err = tx.QueryRowContext(ctx, `SELECT rule, path FROM trigger_deliveries WHERE id = ?`, id).Scan(&rule, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDeliveryNotFound
	}
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trigger_deliveries WHERE rule = ? AND path = ? AND state = 'pending' AND id <> ?`, rule, path, id); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeliveryNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	q.publish(ctx, id)
	return nil
}

// publish reads row id back and delivers it to watchers. A read failure
// only costs the event: the state change itself has already landed.
func (q *Deliveries) publish(ctx context.Context, id int64) {
	d, err := q.Get(ctx, id)
	if err != nil {
		return
	}
	q.s.publish(Event{Kind: "trigger", Delivery: &d})
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	var firstSeen, dueAt, doneAt int64
	if err := row.Scan(&d.ID, &d.Rule, &d.Path, &d.Kind, &d.Origin, &firstSeen, &dueAt, &d.Attempts, &d.State, &d.LastError, &d.Output, &doneAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, err
		}
		return Delivery{}, fmt.Errorf("agent: %w", err)
	}
	d.FirstSeen = time.Unix(0, firstSeen)
	d.DueAt = time.Unix(0, dueAt)
	if doneAt != 0 {
		d.DoneAt = time.Unix(0, doneAt)
	}
	d.Truncated = strings.Contains(d.Output, OutputTruncatedMarker)
	return d, nil
}
