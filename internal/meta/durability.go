package meta

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// durableTx is reserved for publication of journal-backed local writes.
// The sync pragma is connection-local and cannot change inside a transaction.
// Reserve one connection until both the transaction and pragma restoration
// finish; ordinary directory-cache updates keep their NORMAL policy.
func (s *Store) durableTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeTx.Add(1)
	s.durableTxs.Add(1)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var previous int
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&previous); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return err
	}
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// A failed reset leaves FULL, which is conservative, never weaker.
		_, _ = conn.ExecContext(resetCtx, fmt.Sprintf(`PRAGMA synchronous=%d`, previous))
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("meta: begin durable publication: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("meta: commit durable publication: %w", err)
	}
	return nil
}
