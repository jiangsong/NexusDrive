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

const StatePurging State = "purging"

var ErrUploadPurging = errors.New("journal: upload cleanup is pending or has permanently discarded this upload id")
var ErrUploadCleanupState = errors.New("journal: upload cleanup requires a stopped, non-compensation upload")
var ErrUploadCleanupIdentity = errors.New("journal: upload cleanup metadata identity changed")
var ErrUploadCleanupContent = errors.New("journal: upload cleanup path is not a private journal payload")

const uploadCleanupSchema = `
CREATE TABLE IF NOT EXISTS upload_cleanup (
 upload_id TEXT PRIMARY KEY,
 meta_identity TEXT NOT NULL,
 blob_path TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS upload_discarded (
 upload_id TEXT PRIMARY KEY,
 meta_identity TEXT NOT NULL,
 blob_path TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS upload_discarded_blob ON upload_discarded(blob_path);
`

// UploadCleanup is private recovery state, never a control/MCP response. The
// canonical pathname is fixed when cleanup begins. The upload row continues
// to own its payload, sessions and parts until cleanup commits completely.
type UploadCleanup struct {
	Upload       Upload
	MetaIdentity string
	BlobPath     string
}

// BeginUploadCleanup locks a stopped upload against resume and stale workers.
// It does NOT authorize deletion of any cache or metadata object. The VFS
// coordinator must check handles/mount policy, remove only this local version,
// and commit a FULL metadata barrier in MetaIdentity before calling Finish.
// Ordinary callers must not expose this as an uncoordinated administrative API.
func (j *Journal) BeginUploadCleanup(ctx context.Context, id, metaIdentity string) (UploadCleanup, error) {
	var out UploadCleanup
	if !j.Owner() {
		return out, errors.New("journal: upload cleanup requires queue ownership")
	}
	if !validCleanupIdentity(metaIdentity) {
		return out, ErrUploadCleanupIdentity
	}
	err := j.tx(ctx, func(tx *sql.Tx) error {
		u, err := scanUpload(tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.State == StatePurging {
			out, err = j.uploadCleanupTx(tx, id, metaIdentity)
			return err
		}
		if u.State != StateCancelled || u.Tombstone {
			return ErrUploadCleanupState
		}
		p, err := j.cleanupUploadPath(u)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO upload_cleanup(upload_id,meta_identity,blob_path) VALUES(?,?,?)`, id, metaIdentity, p); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE uploads SET state=?,last_error=? WHERE id=?`, StatePurging, "local cleanup pending; remote result is not reconciled", id); err != nil {
			return err
		}
		u.State = StatePurging
		u.LastError = "local cleanup pending; remote result is not reconciled"
		out = UploadCleanup{Upload: u, MetaIdentity: metaIdentity, BlobPath: p}
		return nil
	})
	if err != nil {
		return UploadCleanup{}, err
	}
	return out, nil
}

func validCleanupIdentity(id string) bool {
	return id != "" && len(id) <= 256 && !strings.ContainsRune(id, 0)
}

func (j *Journal) cleanupUploadPath(u Upload) (string, error) {
	if u.BlobPath == "" {
		return "", ErrUploadCleanupContent
	}
	p, err := filepath.Abs(u.BlobPath)
	if err != nil {
		return "", ErrUploadCleanupContent
	}
	objects, err := filepath.Abs(j.ObjectsDir())
	if err != nil {
		return "", err
	}
	if filepath.Dir(p) != objects {
		copyPath, err := j.copyPath(u.ID)
		if err != nil || p != copyPath {
			return "", ErrUploadCleanupContent
		}
	}
	return p, nil
}

func (j *Journal) uploadCleanupTx(tx *sql.Tx, id, metaIdentity string) (UploadCleanup, error) {
	var out UploadCleanup
	err := tx.QueryRow(`SELECT meta_identity,blob_path FROM upload_cleanup WHERE upload_id=?`, id).Scan(&out.MetaIdentity, &out.BlobPath)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if out.MetaIdentity != metaIdentity || !validCleanupIdentity(metaIdentity) {
		return out, ErrUploadCleanupIdentity
	}
	out.Upload, err = scanUpload(tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id=?`, id))
	if err != nil {
		return out, err
	}
	if out.Upload.State != StatePurging || out.Upload.Tombstone {
		return out, ErrUploadCleanupState
	}
	p, err := j.cleanupUploadPath(out.Upload)
	if err != nil || p != out.BlobPath {
		return out, ErrUploadCleanupContent
	}
	return out, nil
}

// UploadCleanupIdentity is read-only and works without acquiring ownership.
// Older journals have no intents and are inspected without migration.
func (j *Journal) UploadCleanupIdentity(ctx context.Context, id string) (string, error) {
	var present int
	if err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='upload_cleanup'`).Scan(&present); err != nil {
		return "", err
	}
	if present == 0 {
		return "", ErrNotFound
	}
	var identity string
	err := j.db.QueryRowContext(ctx, `SELECT meta_identity FROM upload_cleanup WHERE upload_id=?`, id).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return identity, err
}

// FinishUploadCleanup requires the VFS to have durably fenced the absence of
// local references in metaIdentity and removed cache aliases with checked
// unlink/fsync. It never contacts or deletes a remote object. Failure retains
// the complete intent, even when the payload unlink already succeeded.
func (j *Journal) FinishUploadCleanup(ctx context.Context, id, metaIdentity string) error {
	if !j.Owner() {
		return errors.New("journal: upload cleanup requires queue ownership")
	}
	if !validCleanupIdentity(metaIdentity) {
		return ErrUploadCleanupIdentity
	}
	j.objectMu.Lock()
	defer j.objectMu.Unlock()
	return j.tx(ctx, func(tx *sql.Tx) error {
		out, err := j.uploadCleanupTx(tx, id, metaIdentity)
		if errors.Is(err, ErrNotFound) {
			var saved string
			err := tx.QueryRow(`SELECT meta_identity FROM upload_discarded WHERE upload_id=?`, id).Scan(&saved)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if saved != metaIdentity {
				return ErrUploadCleanupIdentity
			}
			return nil // Repeated completion cannot recreate or delete anything.
		}
		if err != nil {
			return err
		}
		p := out.BlobPath
		parent, err := os.Lstat(filepath.Dir(p))
		if err != nil {
			return err
		}
		if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return ErrUploadCleanupContent
		}
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return ErrUploadCleanupContent
		}
		shared, err := j.uploadCleanupShared(tx, out, info)
		if err != nil {
			return err
		}
		if !shared {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if j.uploadCleanupFault != nil {
				if err := j.uploadCleanupFault("unlinked"); err != nil {
					return err
				}
			}
			if j.durability == DurabilityPower {
				d, err := os.Open(filepath.Dir(p))
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
		}
		if j.uploadCleanupFault != nil {
			if err := j.uploadCleanupFault("finalize"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO upload_discarded(upload_id,meta_identity,blob_path) VALUES(?,?,?)`, id, metaIdentity, p); err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM upload_parts WHERE upload_id=?`,
			`DELETE FROM dead_letter WHERE upload_id=?`,
			`DELETE FROM upload_resume_history WHERE upload_id=?`,
			`DELETE FROM upload_cancellation WHERE upload_id=?`,
			`DELETE FROM uploads WHERE id=?`,
			`DELETE FROM upload_cleanup WHERE upload_id=?`,
		} {
			if _, err := tx.Exec(query, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (j *Journal) uploadCleanupShared(tx *sql.Tx, out UploadCleanup, info os.FileInfo) (bool, error) {
	if j.stagedRefs[out.BlobPath] > 0 {
		return true, nil
	}
	shared, err := uploadBlobReferenced(tx, out.Upload.ID, out.BlobPath, info)
	if err != nil || shared {
		return shared, err
	}
	copyPath, err := j.copyPath(out.Upload.ID)
	if err == nil && copyPath == out.BlobPath {
		var refs int
		if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM copy_jobs WHERE id=? AND state<>'submitted') + (SELECT COUNT(*) FROM copy_cleanup WHERE id=?)`, out.Upload.ID, out.Upload.ID).Scan(&refs); err != nil {
			return false, err
		}
		if refs > 0 {
			return true, nil
		}
	}
	return false, nil
}

// uploadBlobReferenced is shared by administrative cleanup and ordinary GC.
// A raw SQL string comparison misses legacy relative paths and inode aliases.
// Stream rows rather than retaining a queue-sized slice. Callers must hold
// objectMu (and check staging reservations) before the SQL writer transaction,
// keeping both locks through unlink so no owner can acquire the pathname in
// between. An empty excludeID checks every upload, including terminal states.
func uploadBlobReferenced(tx *sql.Tx, excludeID, absolutePath string, info os.FileInfo) (bool, error) {
	rows, err := tx.Query(`SELECT blob_path FROM uploads WHERE (?='' OR id<>?)`, excludeID, excludeID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return false, err
		}
		if other == "" {
			continue
		}
		absolute, err := filepath.Abs(other)
		if err != nil {
			return false, err
		}
		if absolute == absolutePath {
			return true, nil
		}
		if info != nil {
			otherInfo, err := os.Stat(other)
			if err == nil && os.SameFile(info, otherInfo) {
				return true, nil
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, rows.Close()
}

func uploadMutationState(tx *sql.Tx, id string) (State, error) {
	var state State
	err := tx.QueryRow(`SELECT state FROM uploads WHERE id=? UNION ALL SELECT 'purging' FROM upload_discarded WHERE upload_id=? LIMIT 1`, id, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

// Session/part acknowledgments are allowed after cancellation (the remote may
// have replied while stopping), but never after cleanup or permanent discard.
func guardNotPurging(tx *sql.Tx, id string) error {
	state, err := uploadMutationState(tx, id)
	if err != nil {
		return err
	}
	if state == StatePurging {
		return ErrUploadPurging
	}
	return nil
}

func (j *Journal) checkDiscardedPayload(tx *sql.Tx, blob string) error {
	p, err := filepath.Abs(blob)
	if err != nil {
		return err
	}
	var discarded int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM upload_discarded WHERE blob_path=?)`, p).Scan(&discarded); err != nil {
		return err
	}
	if discarded == 0 {
		return nil
	}
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: discarded payload has not been staged again", ErrUploadPurging)
	}
	return nil
}
