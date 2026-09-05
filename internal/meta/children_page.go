package meta

import (
	"context"
	"errors"
	"fmt"
)

// ChildrenPage reads a bounded name-ordered range using nodes_parent_name.
// after is exclusive. offset is for compatibility with old numeric cursors;
// new callers should use after to avoid scanning skipped index entries.
// Pages are independent snapshots: changes before after require a new scan.
func (s *Store) ChildrenPage(ctx context.Context, dir uint64, after string, offset, limit int) ([]Node, error) {
	if limit < 1 || limit > 4096 || offset < 0 || (after != "" && offset != 0) {
		return nil, errors.New("meta: invalid directory page")
	}
	s.queries.Add(1)
	rows, err := s.db.QueryContext(ctx, childrenPageSQL, dir, RootIno, after, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("meta: children page: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("meta: children page: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const childrenPageSQL = `SELECT ` + nodeCols + ` FROM nodes
WHERE parent_ino = ? AND ino != ? AND name > ? ORDER BY name LIMIT ? OFFSET ?`

// ChildrenCount preserves listing totals without materializing children.
// It is an index count, not a constant-time operation or a page snapshot.
func (s *Store) ChildrenCount(ctx context.Context, dir uint64) (int, error) {
	s.queries.Add(1)
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE parent_ino = ? AND ino != ?`, dir, RootIno).Scan(&count)
	return count, err
}
