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

func (s *Store) startIndexer() {
	s.indexStop = make(chan struct{})
	s.indexWG.Add(1)
	go func() {
		defer s.indexWG.Done()
		t := time.NewTicker(indexInterval)
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

func (s *Store) flushIndexOnce(ctx context.Context) (int, error) {
	var moved int
	err := s.tx(ctx, func(tx *sql.Tx) error {
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
