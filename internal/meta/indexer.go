package meta

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// indexInterval is how often pending names are folded into the FTS index.
const indexInterval = 500 * time.Millisecond

// indexBatch bounds one flush transaction so it never holds the writer lock
// for long against the FUSE path.
const indexBatch = 1000

func (s *Store) startIndexer(interval time.Duration) {
	if interval <= 0 {
		interval = indexInterval
	}
	s.indexStop = make(chan struct{})
	s.indexWG.Add(1)
	go func() {
		defer s.indexWG.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.indexStop:
				return
			case <-t.C:
				// Errors are not fatal: the rows stay pending and the next
				// tick retries. Search still sees them meanwhile.
				_ = s.FlushIndex(context.Background())
			}
		}
	}()
}

// FlushIndex moves every pending name into the FTS index. It returns once
// the pending table is empty, so tests and a closing store can rely on the
// index being complete afterwards.
func (s *Store) FlushIndex(ctx context.Context) error {
	for {
		n, err := s.flushIndexOnce(ctx)
		if err != nil || n < indexBatch {
			return err
		}
	}
}

// pendingWaiting reports whether the flush below would find anything to move.
// It is a plain read: it takes neither writeMu nor SQLite's writer lock, where
// the transaction takes both — the DSN carries _txlock=immediate, so every
// transaction here is a write transaction even when it finds nothing to do,
// and an idle mount was paying two of them a second for an empty table.
//
// It asks the same join the flush does rather than just whether the pending
// table is non-empty, so "the probe said yes" and "the transaction has work"
// cannot drift apart: a pending row whose node is gone would otherwise put the
// tick back to opening a transaction that moves nothing, forever.
//
// The alternative, a Go-side dirty flag, would have to be set wherever the
// pending table gains a row; but the rows are queued by SQL triggers on nodes
// (schema.go v7 -> v8), so there is no single place in Go to set it and a
// path that forgot would strand a name outside the index indefinitely. This
// probe cannot lose a row: it never caches its answer, so a row that commits
// just after an empty probe is simply seen by the next tick.
func (s *Store) pendingWaiting(ctx context.Context) (bool, error) {
	st, err := s.prep(ctx, `SELECT EXISTS(SELECT 1 FROM name_index_pending p JOIN nodes n ON n.ino=p.ino)`)
	if err != nil {
		return false, fmt.Errorf("meta: index: %w", err)
	}
	var exists int
	if err := st.QueryRowContext(ctx).Scan(&exists); err != nil {
		return false, fmt.Errorf("meta: index: %w", err)
	}
	return exists != 0, nil
}

func (s *Store) flushIndexOnce(ctx context.Context) (int, error) {
	waiting, err := s.pendingWaiting(ctx)
	if err != nil || !waiting {
		return 0, err
	}
	var moved int
	err = s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT p.ino,n.name FROM name_index_pending p JOIN nodes n ON n.ino=p.ino LIMIT ?`, indexBatch)
		if err != nil {
			return fmt.Errorf("meta: index: %w", err)
		}
		type entry struct {
			ino  uint64
			name string
		}
		var batch []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.ino, &e.name); err != nil {
				rows.Close()
				return fmt.Errorf("meta: index: %w", err)
			}
			batch = append(batch, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("meta: index: %w", err)
		}
		moved = len(batch)
		if moved == 0 {
			return nil
		}
		inos := make([]string, 0, moved)
		args := make([]any, 0, moved)
		for _, e := range batch {
			if err := indexNameTx(tx, e.ino, e.name); err != nil {
				return err
			}
			inos = append(inos, "?")
			args = append(args, e.ino)
		}
		if _, err := tx.Exec(`DELETE FROM name_index_pending WHERE ino IN (`+strings.Join(inos, ",")+`)`, args...); err != nil {
			return fmt.Errorf("meta: index: %w", err)
		}
		return nil
	})
	return moved, err
}
