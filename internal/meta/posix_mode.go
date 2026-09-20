package meta

import (
	"context"
	"database/sql"
)

// SetMode records the POSIX permission bits chmod(2) set on a node.
//
// No provider reports a mode, so this database is the only source of
// permissions on the filesystem. mode_set separates a mode a caller chose
// from the default fillMode hands out, and the statements a refresh updates
// nodes through leave a node that has one alone.
//
// Only the permission bits are kept. The mount carries nosuid, so recording a
// setuid or setgid bit would have ls(1) report a permission the kernel will
// not honour.
func (s *Store) SetMode(ctx context.Context, ino uint64, mode uint32) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE nodes SET mode=?, mode_set=1 WHERE ino=?`, mode&0o777, ino)
		return err
	})
}
