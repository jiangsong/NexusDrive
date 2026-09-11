package pool

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"cloudfs/internal/provider"
)

// Ids are opaque and stable. The VFS keys its inode cache on the id a
// provider reports, and the meta store never rewrites an id on rename — so a
// path-shaped id would leave every descendant of a renamed directory pointing
// at a stale path. A pool id therefore never changes for the lifetime of a
// path in this index: rename and move keep it, and a file recreated at the
// same path gets the same id with a new version, which is how a real drive
// behaves too.
//
// A caller may also hand a slash path where an id is expected; it resolves
// locally, so `layout.root: /photos` works without a lookup call.
const (
	rootID   = "p-root"
	idPrefix = "p-"
)

// ErrBadPath is returned for an id or path the pool refuses to interpret.
var ErrBadPath = errors.New("pool: invalid path")

// cleanPath canonicalises a virtual path inside the pool ("/", "/a/b").
func cleanPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/")), nil
}

func parentOf(p string) string {
	if p == "/" {
		return "/"
	}
	return path.Dir(p)
}

// dirParent is the parent column of a member_dirs row: the root is nobody's
// child, so it never shows up as a subdirectory of itself.
func dirParent(p string) string {
	if p == "/" {
		return ""
	}
	return parentOf(p)
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// pathOf resolves an id (or a path handed in as one) to the pool path.
func (p *Pool) pathOf(ctx context.Context, id string) (string, error) {
	if id == rootID || id == "" || id == "/" {
		return "/", nil
	}
	if strings.HasPrefix(id, "/") {
		return cleanPath(id)
	}
	if !strings.HasPrefix(id, idPrefix) {
		return "", fmt.Errorf("%w: %q", provider.ErrNotFound, id)
	}
	var pth string
	err := p.db.QueryRowContext(ctx, `SELECT path FROM ids WHERE id = ?`, id).Scan(&pth)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: unknown id %q", provider.ErrNotFound, id)
	}
	if err != nil {
		return "", fmt.Errorf("pool: %w", err)
	}
	return pth, nil
}

// idFor returns the id for a path, minting one on first sight.
func (p *Pool) idFor(ctx context.Context, pth string) (string, error) {
	if pth == "/" {
		return rootID, nil
	}
	var id string
	err := p.db.QueryRowContext(ctx, `SELECT id FROM ids WHERE path = ?`, pth).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("pool: %w", err)
	}
	id = newID()
	// Two callers may race to mint the same path; the UNIQUE constraint
	// keeps one id per path, and the loser reads the winner's.
	if _, err := p.execIndex(ctx, `INSERT OR IGNORE INTO ids(id, path, created_at) VALUES(?, ?, ?)`, id, pth, time.Now().UnixNano()); err != nil {
		return "", fmt.Errorf("pool: %w", err)
	}
	if err := p.db.QueryRowContext(ctx, `SELECT id FROM ids WHERE path = ?`, pth).Scan(&id); err != nil {
		return "", fmt.Errorf("pool: %w", err)
	}
	return id, nil
}

// idForTx is idFor inside a transaction.
func idForTx(tx *sql.Tx, pth string) (string, error) {
	if pth == "/" {
		return rootID, nil
	}
	var id string
	err := tx.QueryRow(`SELECT id FROM ids WHERE path = ?`, pth).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("pool: %w", err)
	}
	id = newID()
	if _, err := tx.Exec(`INSERT INTO ids(id, path, created_at) VALUES(?, ?, ?)`, id, pth, time.Now().UnixNano()); err != nil {
		return "", fmt.Errorf("pool: %w", err)
	}
	return id, nil
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("pool: crypto/rand unavailable: " + err.Error())
	}
	return idPrefix + hex.EncodeToString(b[:])
}
