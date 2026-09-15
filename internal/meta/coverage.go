package meta

import (
	"context"
	"fmt"

	"cloudfs/internal/provider"
)

// Coverage says how much of the known tree the name index actually holds:
// a directory the crawler or a readdir has listed is in it, a directory that
// is merely known to exist (its parent was listed) is not. Progress lives in
// dir_state, the table readdir already maintains; there is no crawler table,
// which is what lets a pass survive a restart for free.
type Coverage struct {
	// Listed is the number of dir_state rows with complete = 1.
	Listed int64 `json:"listed"`
	// Known is the number of directory nodes, the root included, so
	// Listed <= Known.
	Known int64 `json:"known"`
}

// Coverage counts listed and known directories.
func (s *Store) Coverage(ctx context.Context) (Coverage, error) {
	var c Coverage
	s.queries.Add(2)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dir_state WHERE complete = 1`).Scan(&c.Listed); err != nil {
		return c, fmt.Errorf("meta: coverage: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE kind = ?`, int(provider.KindDir)).Scan(&c.Known); err != nil {
		return c, fmt.Errorf("meta: coverage: %w", err)
	}
	return c, nil
}

// IncompleteDirs pages directory nodes whose listing is missing or stale, by
// ascending ino, so a sweep that only moves `after` forward terminates even
// while it creates new directories (AUTOINCREMENT puts them after the
// cursor). It walks nodes in primary-key order and probes dir_state by its
// key, so a million-node tree costs one index range scan per page.
func (s *Store) IncompleteDirs(ctx context.Context, after uint64, limit int) ([]Node, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.queries.Add(1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE kind = ? AND ino > ?
		  AND NOT EXISTS (SELECT 1 FROM dir_state d WHERE d.ino = nodes.ino AND d.complete = 1)
		ORDER BY ino LIMIT ?`, int(provider.KindDir), after, limit)
	if err != nil {
		return nil, fmt.Errorf("meta: incomplete dirs: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("meta: incomplete dirs: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
