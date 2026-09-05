package meta

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNodeChanged means that the snapshot used to interpret a remote event is
// no longer current. The caller must reread it before advancing its cursor.
var ErrNodeChanged = errors.New("meta: node changed during remote reconciliation")

// ApplyRemoteNode conditionally folds an event into an existing cached node.
// expected must be a stored snapshot; nil next means deletion. This does not
// create unknown nodes or move names. The VFS supplies its local-write policy
// through protect, evaluated inside the same transaction as publication. The
// callback must not reenter the store. A protected directory descendant keeps
// the original directory object when deletion or replacement is requested.
// The returned bool is true only after successful publication; a zero Node
// then means deletion. Copy bindings remain as recovery tombstones.
func (s *Store) ApplyRemoteNode(ctx context.Context, expected Node, next *Node, protect func(Node) bool) (Node, bool, error) {
	if expected.Ino == RootIno {
		return Node{}, false, errors.New("meta: cannot reconcile the root")
	}
	if next != nil && (next.ParentIno != expected.ParentIno || next.Name != expected.Name) {
		return Node{}, false, errors.New("meta: remote reconciliation cannot move a name")
	}
	var result Node
	var changed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE ino=?`, expected.Ino))
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Already removed; never recreate a stale event's target.
		}
		if err != nil {
			return err
		}
		if protect != nil && protect(current) {
			return nil
		}
		// Compare semantic times rather than their Location pointers.
		observed := expected
		if !current.MTime.Equal(observed.MTime) || !current.FetchedAt.Equal(observed.FetchedAt) {
			return ErrNodeChanged
		}
		observed.MTime, observed.FetchedAt = current.MTime, current.FetchedAt
		if current != observed {
			return ErrNodeChanged
		}
		if next == nil || directoryReplacement(current, *next) {
			retained, err := prepareDirectoryReplacementTx(ctx, tx, current, protect)
			if err != nil || retained {
				return err
			}
		}
		if err := fenceDirListingTx(ctx, tx, current.ParentIno, current.Ino); err != nil {
			return err
		}
		if next != nil {
			result = *next
			if result.FetchedAt.IsZero() {
				result.FetchedAt = s.now()
			}
			fillMode(&result)
			result.Ino, err = s.upsertNodeTx(ctx, tx, result)
			if err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return Node{}, false, err
	}
	if changed {
		if next == nil {
			s.markAbsent(expected.ParentIno, expected.Name, defaultNegativeTTL)
		} else {
			s.clearAbsent(expected.ParentIno, expected.Name)
		}
	}
	return result, changed, nil
}
