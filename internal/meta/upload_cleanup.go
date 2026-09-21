package meta

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"cloudfs/internal/provider"
)

var ErrLocalVersionChanged = errors.New("meta: local upload version or its references changed before cleanup")

// LocalVersionCleanup identifies immutable upload content and the complete set
// of node snapshots authorized for removal. Nodes must be read from this store,
// not reconstructed from an upload's original inode (which may be reused).
type LocalVersionCleanup struct {
	StoreIdentity string
	Remote        string
	RemoteID      string
	Version       string
	Size          int64
	Nodes         []Node
}

// RemoveLocalVersion removes exactly the authorized local file aliases and
// proves their durable absence in one FULL transaction. It is not a public
// deletion API: the VFS must first freeze the journal upload against publication
// and resume, authorize every path, and exclude open handles and new publishers
// until cache/payload cleanup finishes. This method neither checks those locks
// nor authorizes deletion of a remote file. Retry after a completed transaction
// must obtain a fresh snapshot (normally no nodes); even that retry forces a
// real synchronous write. Copy bindings survive as anti-revival tombstones.
func (s *Store) RemoveLocalVersion(ctx context.Context, request LocalVersionCleanup) error {
	if request.StoreIdentity == "" || request.Remote == "" || request.Version == "" || request.Size < 0 ||
		!strings.HasPrefix(request.RemoteID, "cloudfs-local:") || request.RemoteID == "cloudfs-local:" {
		return ErrLocalVersionChanged
	}
	expected := make(map[uint64]Node, len(request.Nodes))
	for _, n := range request.Nodes {
		if n.Ino == 0 || n.Ino == RootIno || (n.Kind != provider.KindFile && n.Kind != provider.KindSymlink) || !n.Dirty ||
			n.Remote != request.Remote || n.RemoteID != request.RemoteID || n.Version != request.Version || n.Size != request.Size {
			return ErrLocalVersionChanged
		}
		if _, duplicate := expected[n.Ino]; duplicate {
			return ErrLocalVersionChanged
		}
		expected[n.Ino] = n
	}
	err := s.durableTx(ctx, func(tx *sql.Tx) error {
		var identity string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&identity); err != nil {
			return err
		}
		if identity != request.StoreIdentity {
			return ErrLocalVersionChanged
		}
		// A local upload ID is global, not just remote-scoped. A malformed alias
		// under another remote also blocks cleanup instead of being overlooked.
		rows, err := tx.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE remote_id=?`, request.RemoteID)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			current, err := scanNode(rows)
			if err != nil {
				return err
			}
			observed, ok := expected[current.Ino]
			if !ok || !current.MTime.Equal(observed.MTime) || !current.FetchedAt.Equal(observed.FetchedAt) {
				return ErrLocalVersionChanged
			}
			observed.MTime, observed.FetchedAt = current.MTime, current.FetchedAt
			if current != observed {
				return ErrLocalVersionChanged
			}
			seen++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if seen != len(expected) {
			return ErrLocalVersionChanged
		}
		for _, n := range request.Nodes {
			// Do not let a corrupt file-with-children turn exact file cleanup
			// into recursive deletion of unrelated content.
			var children bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE parent_ino=?)`, n.Ino).Scan(&children); err != nil {
				return err
			}
			if children {
				return ErrLocalVersionChanged
			}
			if err := fenceDirListingTx(ctx, tx, n.ParentIno); err != nil {
				return err
			}
			if err := removeSubtreeTx(tx, n.Ino); err != nil {
				return err
			}
		}
		// Reuse the persistent absence fence, including on zero-node retries.
		// A read-only transaction at FULL does not flush preceding WAL writes.
		result, err := tx.ExecContext(ctx, `UPDATE copy_cleanup_fence SET value=1-value WHERE id=1`)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrLocalVersionChanged
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, n := range request.Nodes {
		s.markAbsent(n.ParentIno, n.Name, defaultNegativeTTL)
	}
	return nil
}
