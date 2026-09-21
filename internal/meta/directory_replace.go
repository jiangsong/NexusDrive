package meta

import (
	"context"
	"database/sql"
)

// A directory's children and listing freshness belong to its remote object,
// not just its virtual name. Type transitions also require a fresh inode:
// kernel adapters cannot change an existing inode's stable file type.
func directoryReplacement(old, next Node) bool {
	if old.Kind != next.Kind {
		return true
	}
	return old.IsDir() && (old.Remote != next.Remote || old.RemoteID != next.RemoteID)
}

// prepareDirectoryReplacementTx retains the entire old object when one of
// its descendants is protected. Otherwise remove the old tree atomically;
// the caller will insert the replacement with a fresh inode in the same tx.
func prepareDirectoryReplacementTx(ctx context.Context, tx *sql.Tx, old Node, protect func(Node) bool) (retained bool, err error) {
	if protect != nil {
		retained, err = protectedSubtreeTx(ctx, tx, old.Ino, protect)
		if err != nil || retained {
			return retained, err
		}
	}
	// Keep nodes until the auxiliary indices are cleared so each CTE sees
	// the same descendants. SQLite holds the set, not an unbounded Go slice.
	cte := `WITH RECURSIVE subtree(ino) AS (VALUES(?) UNION
SELECT n.ino FROM nodes n JOIN subtree p ON n.parent_ino=p.ino WHERE n.ino!=?) `
	for _, q := range []string{
		`DELETE FROM dir_state WHERE ino IN (SELECT ino FROM subtree)`,
		`DELETE FROM name_index WHERE rowid IN (SELECT ino FROM subtree)`,
		`DELETE FROM name_indexed WHERE ino IN (SELECT ino FROM subtree)`,
		`DELETE FROM name_index_pending WHERE ino IN (SELECT ino FROM subtree)`,
		`DELETE FROM nodes WHERE ino IN (SELECT ino FROM subtree)`,
	} {
		if _, err := tx.ExecContext(ctx, cte+q, old.Ino, RootIno); err != nil {
			return false, err
		}
	}
	return false, nil
}
