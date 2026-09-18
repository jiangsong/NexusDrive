package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Extended attributes live here, beside the node, and go no further.
//
// They exist because macOS makes them unavoidable: com.apple.quarantine on
// anything downloaded, FinderInfo, Finder tags, and copyfile(3) — what the
// Finder, cp -p and ditto all copy with — treats a failed setxattr as a
// failed copy. A mount that refused them read and wrote fine from a shell and
// could not be copied into from the Finder.
//
// Keeping them local is the deliberate half. A cloud drive has nowhere to put
// a com.apple.* attribute; the kernel's own fallback is an AppleDouble "._"
// sidecar file, which on this filesystem would mean uploading a junk file for
// every copied file and showing it on every other device that opens the drive.
// The values are small, Mac-specific, and mean nothing to the backend.

var (
	// ErrNoXattr is "no such attribute", which the kernel spells ENOATTR.
	ErrNoXattr = errors.New("meta: no such extended attribute")
	// ErrXattrTooBig refuses a value this store will not hold. A resource
	// fork can arrive as an extended attribute, and a metadata database is
	// not a blob store.
	ErrXattrTooBig = errors.New("meta: extended attribute is too large")
)

const (
	// MaxXattrValue is the largest value stored. Everything macOS attaches in
	// practice is a few hundred bytes; the headroom is for the occasional
	// tagged or signed file, not for a resource fork.
	MaxXattrValue = 64 << 10
	// MaxXattrName matches the kernel's own limit on an attribute name.
	MaxXattrName = 255
)

// SetXattr stores one attribute, replacing any previous value.
func (s *Store) SetXattr(ctx context.Context, ino uint64, name string, value []byte) error {
	if name == "" || len(name) > MaxXattrName || len(value) > MaxXattrValue {
		return ErrXattrTooBig
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO xattrs (ino, name, value) VALUES (?, ?, ?)
ON CONFLICT(ino, name) DO UPDATE SET value = excluded.value`, int64(ino), name, value); err != nil {
			return fmt.Errorf("meta: set xattr: %w", err)
		}
		return nil
	})
}

// Xattr reads one attribute.
func (s *Store) Xattr(ctx context.Context, ino uint64, name string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM xattrs WHERE ino = ? AND name = ?`, int64(ino), name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoXattr
	}
	if err != nil {
		return nil, fmt.Errorf("meta: xattr: %w", err)
	}
	return value, nil
}

// XattrNames lists the attributes held for a node, sorted so a listing is
// stable between calls.
func (s *Store) XattrNames(ctx context.Context, ino uint64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM xattrs WHERE ino = ? ORDER BY name`, int64(ino))
	if err != nil {
		return nil, fmt.Errorf("meta: xattr names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("meta: xattr names: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// RemoveXattr drops one attribute, reporting ErrNoXattr when there was none:
// removexattr(2) distinguishes the two, and so must this.
func (s *Store) RemoveXattr(ctx context.Context, ino uint64, name string) error {
	var missing bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM xattrs WHERE ino = ? AND name = ?`, int64(ino), name)
		if err != nil {
			return fmt.Errorf("meta: remove xattr: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("meta: remove xattr: %w", err)
		}
		missing = n == 0
		return nil
	})
	if err != nil {
		return err
	}
	if missing {
		return ErrNoXattr
	}
	return nil
}
