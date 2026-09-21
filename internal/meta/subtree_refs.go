package meta

import (
	"context"

	"cloudfs/internal/provider"
)

// RemoteRef is the provider identity a cache key is built from. It is what a
// retention rule needs from the tree: which stored objects a path covers.
type RemoteRef struct {
	Remote   string
	RemoteID string
	Version  string
}

// SubtreeRefs visits the remote identity of every file at or below ino, or of
// ino alone when recursive is false. Directories and nodes without a remote
// object are skipped: neither has cached content of its own.
//
// This exists so that pin reconciliation can ask "what does this rule cover?"
// in one query per rule. Asking the opposite question — walking every cached
// object and looking up where it lives — costs a query per cached object, and
// that runs on every rename.
func (s *Store) SubtreeRefs(ctx context.Context, ino uint64, recursive bool, visit func(RemoteRef) error) error {
	s.queries.Add(1)
	query := `SELECT remote, remote_id, version FROM nodes WHERE ino=? AND kind!=? AND remote_id!=''`
	args := []any{ino, int(provider.KindDir)}
	if recursive {
		// The root is its own parent, so the walk has to refuse to step back
		// into it or it never terminates.
		query = `WITH RECURSIVE subtree(ino) AS (
  VALUES(?) UNION SELECT n.ino FROM nodes n JOIN subtree p ON n.parent_ino=p.ino WHERE n.ino!=?
)
SELECT remote, remote_id, version FROM nodes WHERE ino IN (SELECT ino FROM subtree) AND kind!=? AND remote_id!=''`
		args = []any{ino, RootIno, int(provider.KindDir)}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ref RemoteRef
		if err := rows.Scan(&ref.Remote, &ref.RemoteID, &ref.Version); err != nil {
			return err
		}
		if err := visit(ref); err != nil {
			return err
		}
	}
	return rows.Err()
}
