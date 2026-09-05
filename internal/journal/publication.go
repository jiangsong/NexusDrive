package journal

import (
	"context"
	"database/sql"
	"errors"
)

// Unpublished lists durable writes whose local publication was interrupted.
// Recover must verify their payloads before the VFS uses them; lost payloads
// are dead-lettered there and deliberately excluded here.
func (j *Journal) Unpublished(ctx context.Context) ([]Upload, error) {
	return j.list(ctx, `WHERE needs_publish = 1 AND state = ? ORDER BY rowid`, string(StatePending))
}

// MarkPublished opens the worker gate after cache installation and metadata
// publication. The VFS uses a durable metadata transaction in power mode.
// Repeating it after a crash is harmless. It never requeues a completed or
// dead task, or resets retry/session state.
func (j *Journal) MarkPublished(ctx context.Context, id string) error {
	if !j.Owner() {
		return errors.New("journal: publication requires storage ownership")
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := guardNotPurging(tx, id); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE uploads SET needs_publish = 0 WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
}
