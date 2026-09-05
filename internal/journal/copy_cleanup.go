package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const copyCleanupSchema = `CREATE TABLE IF NOT EXISTS copy_cleanup (
 id TEXT PRIMARY KEY,
 spec TEXT NOT NULL,
 want TEXT NOT NULL,
 checkpoint INTEGER NOT NULL,
 crc32c TEXT NOT NULL,
 revision INTEGER NOT NULL
);`

func (j *Journal) checkCopyUploadOwner(tx *sql.Tx, blob string) error {
	p, err := filepath.Abs(blob)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(filepath.Join(j.dir, "copies"))
	if err != nil {
		return err
	}
	if filepath.Dir(p) != dir {
		return nil
	}
	id := strings.TrimSuffix(filepath.Base(p), ".part")
	want, err := j.copyPath(id)
	if err != nil || p != want {
		return ErrCopyCorrupt
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM copy_jobs WHERE id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrCopyState
	}
	return nil
}

// The union is read-compatible with journals predating cleanup. Keeping the
// full intent visible as "purging" makes interrupted cleanup inspectable.
func (j *Journal) copyRowsSQL(ctx context.Context) (string, error) {
	query := `SELECT ` + j.copyColumns() + ` FROM copy_jobs`
	var present int
	if err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='copy_cleanup'`).Scan(&present); err != nil {
		return "", err
	}
	if present != 0 {
		query += ` UNION ALL SELECT id,spec,want,'purging' AS state,checkpoint,crc32c,'' AS last_error,revision FROM copy_cleanup`
	}
	return query, nil
}

func copyUploadsAbsent(tx *sql.Tx, id, payload string) error {
	var refs int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM uploads WHERE id=? OR blob_path=?`, id, payload).Scan(&refs); err != nil {
		return err
	}
	if refs != 0 {
		return ErrCopyReferenced
	}
	return nil
}

// BeginCopyCleanup transfers a terminal record into a durable cleanup intent.
// VFS must first fence absence of local metadata references. Upload references
// and active preparation handles are independently protected here.
func (j *Journal) BeginCopyCleanup(ctx context.Context, id string) error {
	p, err := j.copyPath(id)
	if err != nil {
		return err
	}
	if err := j.claimCopy(id); err != nil {
		return err
	}
	defer j.releaseCopy(id)
	return j.tx(ctx, func(tx *sql.Tx) error {
		if err := copyUploadsAbsent(tx, id, p); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM copy_cleanup WHERE id=?`, id).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return nil
		}
		var state CopyState
		if err := tx.QueryRow(`SELECT state FROM copy_jobs WHERE id=?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state != CopyFailed && state != CopyCancelled && state != CopySubmitted {
			return ErrCopyState
		}
		if _, err := tx.Exec(`INSERT INTO copy_cleanup(id,spec,want,checkpoint,crc32c,revision) SELECT id,spec,want,checkpoint,crc32c,revision FROM copy_jobs WHERE id=?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM copy_jobs WHERE id=?`, id)
		return err
	})
}

// FinishCopyCleanup deletes only this job's exact private name, then commits
// removal of the cleanup intent. Failure (including directory fsync/SQL commit)
// leaves an intent that can safely retry after restart, even if unlink worked.
func (j *Journal) FinishCopyCleanup(ctx context.Context, id string) error {
	p, err := j.copyPath(id)
	if err != nil {
		return err
	}
	if err := j.claimCopy(id); err != nil {
		return err
	}
	defer j.releaseCopy(id)
	return j.tx(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM copy_cleanup WHERE id=?`, id).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			return ErrNotFound
		}
		if err := copyUploadsAbsent(tx, id, p); err != nil {
			return err
		}
		dir := filepath.Dir(p)
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("journal: copy cleanup directory is not a private directory")
		}
		if info, err := os.Lstat(p); err == nil && info.IsDir() {
			return ErrCopyCorrupt
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if j.durability == DurabilityPower {
			d, err := os.Open(dir)
			if err != nil {
				return err
			}
			err = d.Sync()
			closeErr := d.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		_, err = tx.Exec(`DELETE FROM copy_cleanup WHERE id=?`, id)
		return err
	})
}
