package meta

import (
	"context"
	"database/sql"
)

// A directory carries two independent refresh counters, and telling them apart
// is what keeps a concurrent change feed from making ordinary reads fail.
//
//   - generation is the hard fence. It moves when a name under the directory
//     is removed or moved, or when the directory object itself is replaced.
//     An older in-flight provider snapshot still lists the removed name, so
//     publishing it would resurrect what was just deleted. Such a listing has
//     to be refused.
//   - stale_generation is the soft fence. It moves when something says only
//     "this directory is out of date" — a change event for an entry we have
//     never listed, or an upload that landed without describing its result.
//     Nothing was removed, so an older snapshot is still a truthful listing;
//     it just may not be the newest one. Publishing it loses nothing as long
//     as the directory stays marked incomplete, so the next reader lists it
//     again.
//
// Both used to be the same counter, and the cost was visible: scanning a large
// library while the change feed applied its first backlog refused listings by
// the hundred, and the reader saw a directory it could not read at all.

// fenceDirListingTx invalidates in-flight listings of dirs, because a name
// under them has been removed or moved and an older snapshot would bring it
// back. It must share the mutation's transaction: a failed mutation must not
// leave a phantom fence. Only directories with a prior BeginDirListing have a
// generation row.
func fenceDirListingTx(ctx context.Context, tx *sql.Tx, dirs ...uint64) error {
	return bumpListingCounters(ctx, tx, "generation", dirs)
}

// markDirStaleTx records that dirs are out of date without invalidating an
// in-flight listing of them. Callers that also remove a name must use
// fenceDirListingTx instead.
func markDirStaleTx(ctx context.Context, tx *sql.Tx, dirs ...uint64) error {
	return bumpListingCounters(ctx, tx, "stale_generation", dirs)
}

func bumpListingCounters(ctx context.Context, tx *sql.Tx, column string, dirs []uint64) error {
	for i, dir := range dirs {
		duplicate := false
		for _, previous := range dirs[:i] {
			if previous == dir {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		// column is a constant chosen by the two callers above, never input.
		if _, err := tx.ExecContext(ctx,
			`UPDATE directory_refresh_generation SET `+column+`=`+column+`+1 WHERE ino=?`, dir); err != nil {
			return err
		}
	}
	return nil
}
