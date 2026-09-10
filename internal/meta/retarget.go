package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Retarget rewrites the backend id of ino after the backend moved it.
//
// Backends that address entries by path — sftp, webdav, s3, smb, anything
// with Caps.PathIDs — change the id of every descendant when a directory is
// renamed or moved. descendants asks for that second half: every node under
// ino whose id still begins with the old id as a path prefix is rewritten to
// begin with the new one. Nodes addressed some other way (a queued write's
// local placeholder, an entry the backend re-parented on its own) do not
// match the prefix and are left alone, and neither is a sibling whose id
// merely starts with the same characters — the prefix carries the separator.
//
// No fence is pushed: no name left the directory and no listing became wrong,
// only the address a read uses.
func (s *Store) Retarget(ctx context.Context, ino uint64, newID string, descendants bool) error {
	if ino == RootIno {
		return errors.New("meta: cannot retarget the root")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		return retargetTx(ctx, tx, ino, newID, descendants)
	})
}

// retargetTx is the rewrite itself, without a transaction of its own, so a
// caller that must publish it together with another change can put both in
// one. RenameAndRetarget is why that matters: on a path-addressed backend the
// new name and the new ids are a single fact.
func retargetTx(ctx context.Context, tx *sql.Tx, ino uint64, newID string, descendants bool) error {
	{
		var oldID, remote string
		err := tx.QueryRowContext(ctx, `SELECT remote_id, remote FROM nodes WHERE ino = ?`, ino).Scan(&oldID, &remote)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("meta: retarget: %w", err)
		}
		if oldID == newID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET remote_id = ? WHERE ino = ?`, newID, ino); err != nil {
			return fmt.Errorf("meta: retarget: %w", err)
		}
		if !descendants || oldID == "" {
			return nil
		}
		// substr and length count characters, not bytes, so the prefix
		// arithmetic is done by SQLite rather than by Go's len().
		// Some path-addressed backends (notably S3) use a trailing slash for
		// directory IDs. Normalize only the separator used for matching and
		// replacement; the directory itself keeps the exact ID the backend
		// returned above.
		oldPrefix := strings.TrimSuffix(oldID, "/") + "/"
		newPrefix := strings.TrimSuffix(newID, "/") + "/"
		// The walk follows parent_ino, which crosses a nested mount: a
		// subtree of another remote can sit under this directory, and its
		// ids mean something on that other backend. Renaming here says
		// nothing about them, so the rewrite is scoped to this remote.
		_, err = tx.ExecContext(ctx, `
			WITH RECURSIVE subtree(ino) AS (
			  VALUES(?) UNION SELECT n.ino FROM nodes n JOIN subtree p ON n.parent_ino = p.ino
			)
			UPDATE nodes SET remote_id = ? || substr(remote_id, length(?) + 1)
			 WHERE ino IN (SELECT ino FROM subtree)
			   AND ino != ?
			   AND remote = ?
			   AND substr(remote_id, 1, length(?)) = ?`,
			ino, newPrefix, oldPrefix, ino, remote, oldPrefix, oldPrefix)
		if err != nil {
			return fmt.Errorf("meta: retarget subtree: %w", err)
		}
		return nil
	}
}

// RenameAndRetarget moves a node and rewrites the backend ids underneath it in
// one transaction. Committing them separately publishes a tree that was never
// true — the directory under its new name, its children still addressed by the
// old path — and a failure or a crash between the two leaves it that way, with
// every descendant pointing at a path the backend no longer has.
func (s *Store) RenameAndRetarget(ctx context.Context, ino, newParent uint64, newName, newID string, descendants bool) error {
	if ino == RootIno {
		return errors.New("meta: cannot rename the root")
	}
	var oldParent uint64
	var oldName string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := renameTx(ctx, tx, ino, newParent, newName, &oldParent, &oldName); err != nil {
			return err
		}
		return retargetTx(ctx, tx, ino, newID, descendants)
	})
	if err != nil {
		return err
	}
	s.clearAbsent(newParent, newName)
	s.markAbsent(oldParent, oldName, defaultNegativeTTL)
	return nil
}
