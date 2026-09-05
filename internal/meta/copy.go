package meta

import (
	"context"
	"database/sql"
	"errors"

	"cloudfs/internal/provider"
)

var ErrCopyTargetChanged = errors.New("meta: copy target was removed, renamed or replaced")
var ErrCopyReferenced = errors.New("meta: copy content is still referenced by a local file")

// Identity distinguishes a reopened database from a newly recreated tree.
// Persisted inode numbers are not meaningful after metadata loss/recreation.
func (s *Store) Identity(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&id)
	return id, err
}

// FenceCopyCleanup proves absence in this database and flushes preceding WAL
// writes before the caller may discard payload. A read-only FULL transaction
// is insufficient: toggling this fence forces an actual synchronous commit.
// The caller serializes namespace publication until cleanup finishes.
func (s *Store) FenceCopyCleanup(ctx context.Context, identity, remote, remoteID string) error {
	return s.durableTx(ctx, func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM store_identity`).Scan(&current); err != nil {
			return err
		}
		if identity == "" || current != identity {
			return ErrCopyTargetChanged
		}
		var refs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE remote=? AND remote_id=?`, remote, remoteID).Scan(&refs); err != nil {
			return err
		}
		if refs != 0 {
			return ErrCopyReferenced
		}
		_, err := tx.ExecContext(ctx, `UPDATE copy_cleanup_fence SET value=1-value WHERE id=1`)
		return err
	})
}

// CheckCopyTarget is a read-only preflight for an explicit retry. BindCopy is
// still the authoritative atomic check at publication: the tree can change
// after this method returns, and this method never creates or revives a node.
func (s *Store) CheckCopyTarget(ctx context.Context, id string, expected Node) error {
	var ino uint64
	err := s.db.QueryRowContext(ctx, `SELECT ino FROM copy_bindings WHERE copy_id=?`, id).Scan(&ino)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := s.Lookup(ctx, expected.ParentIno, expected.Name)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return ErrExists
	}
	if err != nil {
		return err
	}
	old, err := s.Get(ctx, ino)
	if errors.Is(err, ErrNotFound) {
		return ErrCopyTargetChanged
	}
	if err != nil {
		return err
	}
	if !sameCopyTarget(old, expected) {
		return ErrCopyTargetChanged
	}
	return nil
}

func sameCopyTarget(old, expected Node) bool {
	return old.ParentIno == expected.ParentIno && old.Name == expected.Name && old.Remote == expected.Remote && old.RemoteID == expected.RemoteID && old.Version == expected.Version && old.Size == expected.Size && old.Dirty && old.Kind == provider.KindFile
}

// BindCopy atomically creates a complete local node and records its copy ID.
// A repeated call may reuse only the original, unchanged inode. The binding
// survives unlink; absence must never turn recovery into a second creation.
func (s *Store) BindCopy(ctx context.Context, id string, n Node) (Node, error) {
	if id == "" || n.Kind != provider.KindFile || n.RemoteID == "" || n.Version == "" || n.Size < 0 {
		return Node{}, errors.New("meta: invalid copy publication")
	}
	if n.Mode == 0 {
		n.Mode = 0644
	}
	if n.FetchedAt.IsZero() {
		n.FetchedAt = s.now()
	}
	n.Dirty = true
	err := s.durableTx(ctx, func(tx *sql.Tx) error {
		var ino uint64
		err := tx.QueryRowContext(ctx, `SELECT ino FROM copy_bindings WHERE copy_id=?`, id).Scan(&ino)
		if err == nil {
			old, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE ino=?`, ino))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCopyTargetChanged
			}
			if err != nil {
				return err
			}
			if !sameCopyTarget(old, n) {
				return ErrCopyTargetChanged
			}
			n = old
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var kind provider.Kind
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM nodes WHERE ino=?`, n.ParentIno).Scan(&kind); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if kind != provider.KindDir {
			return ErrCopyTargetChanged
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE parent_ino=? AND name=?`, n.ParentIno, n.Name).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrExists
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO nodes (parent_ino,name,kind,size,mtime_ns,mode,remote,remote_id,version,remote_version,hash_type,hash,fetched_at,ttl_s,dirty) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
			n.ParentIno, n.Name, int(n.Kind), n.Size, n.MTime.UnixNano(), n.Mode, n.Remote, n.RemoteID, n.Version, n.RemoteVersion, n.HashType, n.Hash, n.FetchedAt.Unix(), int64(n.TTL.Seconds()))
		if err != nil {
			return err
		}
		allocated, err := res.LastInsertId()
		if err != nil {
			return err
		}
		n.Ino = uint64(allocated)
		// Node insert triggers maintain both search indexes in this transaction.
		_, err = tx.ExecContext(ctx, `INSERT INTO copy_bindings(copy_id,ino) VALUES (?,?)`, id, n.Ino)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	s.clearAbsent(n.ParentIno, n.Name)
	return n, nil
}
