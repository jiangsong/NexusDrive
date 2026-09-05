package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cloudfs/internal/provider"
)

var ErrResumeChanged = errors.New("journal: cancelled upload changed while preparing resume")
var ErrResumeContent = errors.New("journal: retained upload content cannot be verified")

// UploadBinding identifies the local database, mount and credential
// generation that is authorized to start a provider request. It contains no
// provider secret or provider-issued account identifier.
type UploadBinding struct {
	MetaIdentity   string
	MountPrefix    string
	MountRootID    string
	AccountBinding string
}

// PreparedUploadResume is a validated immutable snapshot. Its commit is still
// conditional on a cancellation revision, so later stops always win.
type PreparedUploadResume struct {
	j        *Journal
	u        Upload
	revision int64
}

func (p *PreparedUploadResume) Upload() Upload {
	u := p.u
	u.Hashes = nil
	u.Session = nil
	return u
}

// PrepareUploadResume verifies the complete local object without holding the
// journal writer lock. Cancelled rows retain their blob while this runs.
func (j *Journal) PrepareUploadResume(ctx context.Context, id string) (*PreparedUploadResume, error) {
	if !j.Owner() {
		return nil, errors.New("journal: resume requires queue ownership")
	}
	p := &PreparedUploadResume{j: j}
	err := j.tx(ctx, func(tx *sql.Tx) error {
		var err error
		p.u, err = scanUpload(tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.u.State != StateCancelled || p.u.Tombstone {
			return ErrResumeChanged
		}
		return tx.QueryRow(`SELECT COALESCE((SELECT revision FROM upload_cancellation WHERE upload_id=?),0)`, id).Scan(&p.revision)
	})
	if err != nil {
		return nil, err
	}
	u := p.u
	abs, err := filepath.Abs(u.BlobPath)
	if err != nil {
		return nil, ErrResumeContent
	}
	objects, err := filepath.Abs(j.ObjectsDir())
	if err != nil {
		return nil, ErrResumeContent
	}
	copyPath, copyErr := j.copyPath(u.ID)
	if filepath.Dir(abs) != objects && (copyErr != nil || abs != copyPath) {
		return nil, ErrResumeContent
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() != u.Size {
		return nil, ErrResumeContent
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, ErrResumeContent
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrResumeContent
	}
	want := u.Hashes[provider.HashCRC32C]
	if want == "" {
		return nil, ErrResumeContent
	}
	h := crc32.New(castagnoli)
	buf := make([]byte, 256<<10)
	var total int64
	reader := io.NewSectionReader(f, 0, u.Size)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrResumeContent
		}
	}
	finalInfo, statErr := f.Stat()
	if statErr != nil || finalInfo.Size() != u.Size || total != u.Size || fmt.Sprintf("%08x", h.Sum32()) != want {
		return nil, ErrResumeContent
	}
	return p, nil
}

// Commit starts a fresh attempt, not an uncertain old multipart session. The
// old session and parts are retained in private history for later reconciliation.
// Callers must validate current VFS target/version immediately before this.
func (p *PreparedUploadResume) Commit(ctx context.Context) error {
	return p.commit(ctx, nil)
}

// CommitBound starts the new attempt under the caller's freshly revalidated
// binding. Explicit resume is the only way a legacy stopped upload can be
// adopted by a current account generation.
func (p *PreparedUploadResume) CommitBound(ctx context.Context, binding UploadBinding) error {
	values := []string{binding.MetaIdentity, binding.MountPrefix, binding.MountRootID, binding.AccountBinding}
	for _, value := range values {
		if value == "" || len(value) > 8192 || strings.ContainsRune(value, 0) {
			return ErrResumeChanged
		}
	}
	return p.commit(ctx, &binding)
}

func (p *PreparedUploadResume) commit(ctx context.Context, binding *UploadBinding) error {
	if p == nil || p.j == nil {
		return ErrResumeChanged
	}
	j := p.j
	return j.tx(ctx, func(tx *sql.Tx) error {
		current, err := scanUpload(tx.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id=?`, p.u.ID))
		if err != nil {
			return ErrResumeChanged
		}
		var revision int64
		if err := tx.QueryRow(`SELECT COALESCE((SELECT revision FROM upload_cancellation WHERE upload_id=?),0)`, p.u.ID).Scan(&revision); err != nil {
			return err
		}
		if current.State != StateCancelled || current.Tombstone || revision != p.revision || current.BlobPath != p.u.BlobPath || current.Size != p.u.Size || current.Ino != p.u.Ino || current.Remote != p.u.Remote || current.Name != p.u.Name || current.RemoteParentID != p.u.RemoteParentID {
			return ErrResumeChanged
		}
		if current.Ino != 0 {
			var newer int
			if err := tx.QueryRow(`SELECT count(*) FROM uploads WHERE remote=? AND ino=? AND rowid>(SELECT rowid FROM uploads WHERE id=?)`, current.Remote, current.Ino, current.ID).Scan(&newer); err != nil {
				return err
			}
			if newer > 0 {
				return ErrResumeChanged
			}
		}
		rows, err := tx.Query(`SELECT idx,etag,state FROM upload_parts WHERE upload_id=? ORDER BY idx`, current.ID)
		if err != nil {
			return err
		}
		var parts []Part
		for rows.Next() {
			var part Part
			if err := rows.Scan(&part.Index, &part.ETag, &part.State); err != nil {
				rows.Close()
				return err
			}
			parts = append(parts, part)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		snapshot, err := json.Marshal(struct {
			Upload Upload
			Parts  []Part
		}{current, parts})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO upload_resume_history(upload_id,revision,snapshot) VALUES(?,?,?)`, current.ID, revision, string(snapshot)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM upload_parts WHERE upload_id=?`, current.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM dead_letter WHERE upload_id=?`, current.ID); err != nil {
			return err
		}
		if binding == nil {
			_, err = tx.Exec(`UPDATE uploads SET state=?,attempt=0,next_retry_at=0,last_error='',session='{}' WHERE id=?`, StatePending, current.ID)
		} else {
			_, err = tx.Exec(`UPDATE uploads SET state=?,attempt=0,next_retry_at=0,last_error='',session='{}',meta_identity=?,mount_prefix=?,mount_root_id=?,account_binding=? WHERE id=?`,
				StatePending, binding.MetaIdentity, binding.MountPrefix, binding.MountRootID, binding.AccountBinding, current.ID)
		}
		return err
	})
}
