package meta

import (
	"context"
	"database/sql"
)

// fenceDirListingTx prevents an in-flight provider snapshot from undoing a
// committed name removal/move or explicit invalidation. It must share the
// mutation's transaction: a failed mutation must not leave a phantom fence.
// Only directories with a prior BeginDirListing have a generation row.
func fenceDirListingTx(ctx context.Context, tx *sql.Tx, dirs ...uint64) error {
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
		if _, err := tx.ExecContext(ctx, `UPDATE directory_refresh_generation SET generation=generation+1 WHERE ino=?`, dir); err != nil {
			return err
		}
	}
	return nil
}
