package pool

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cloudfs/internal/provider"
)

// A hold is the pool's own hard link to the bytes of a file it has just
// uploaded to one member. The queue releases its copy on success and the
// read cache may evict its own, but the pool still owes the file N-1
// replicas; without the hold it would have to download from the first
// member what it just uploaded there. Holds are bounded by hold_max_bytes
// and hold_max_age — past that, repair reads from a live replica instead.
// The link shares the inode with the cache, so a hold costs no space until
// the cache lets go.

func (p *Pool) holdsDir() string {
	if p.stateDir == "" {
		return ""
	}
	return filepath.Join(p.stateDir, "holds-"+p.name)
}

// takeHold links the upload's blob into the holds directory. It reports
// false, never an error, when there is nothing to hold: holds are an
// optimisation the write path must not depend on.
func (p *Pool) takeHold(ctx context.Context, link provider.UploadBlobLinker, pth string, size int64) (string, bool) {
	dir := p.holdsDir()
	if dir == "" || link == nil {
		return "", false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	dst := filepath.Join(dir, newID()[len(idPrefix):])
	if err := link(dst); err != nil {
		return "", false
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO holds(hold_path, path, ctoken, size, created_at) VALUES(?, ?, '', ?, ?)`, dst, pth, size, p.now().UnixNano()); err != nil {
		os.Remove(dst)
		return "", false
	}
	p.trimHolds(ctx)
	return dst, true
}

// setHoldToken records which content a hold carries once the upload has
// produced its token.
func (p *Pool) setHoldToken(tx *sql.Tx, holdPath, ctoken string) error {
	_, err := tx.Exec(`UPDATE holds SET ctoken = ? WHERE hold_path = ?`, ctoken, holdPath)
	return err
}

// releaseHolds drops every hold for a path.
func (p *Pool) releaseHolds(ctx context.Context, pth string) {
	rows, err := p.db.QueryContext(ctx, `SELECT hold_path FROM holds WHERE path = ?`, pth)
	if err != nil {
		return
	}
	var paths []string
	for rows.Next() {
		var hp string
		if rows.Scan(&hp) == nil {
			paths = append(paths, hp)
		}
	}
	rows.Close()
	for _, hp := range paths {
		os.Remove(hp)
		_, _ = p.db.ExecContext(ctx, `DELETE FROM holds WHERE hold_path = ?`, hp)
	}
}

// holdFor returns a local copy of the content at pth with the given token,
// for repair to upload from.
func (p *Pool) holdFor(ctx context.Context, pth, ctoken string) (string, bool) {
	var hp string
	err := p.db.QueryRowContext(ctx, `SELECT hold_path FROM holds WHERE path = ? AND ctoken = ? ORDER BY created_at DESC LIMIT 1`, pth, ctoken).Scan(&hp)
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(hp); err != nil {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM holds WHERE hold_path = ?`, hp)
		return "", false
	}
	return hp, true
}

// trimHolds enforces the byte budget and the age limit, oldest first.
func (p *Pool) trimHolds(ctx context.Context) {
	maxBytes := int64(p.settings.HoldMaxBytes)
	maxAge := p.settings.HoldMaxAge
	now := p.now()
	rows, err := p.db.QueryContext(ctx, `SELECT hold_path, size, created_at FROM holds ORDER BY created_at ASC`)
	if err != nil {
		return
	}
	type hold struct {
		path    string
		size    int64
		created int64
	}
	var holds []hold
	var total int64
	for rows.Next() {
		var h hold
		if rows.Scan(&h.path, &h.size, &h.created) == nil {
			holds = append(holds, h)
			total += h.size
		}
	}
	rows.Close()
	for _, h := range holds {
		tooOld := maxAge > 0 && now.UnixNano()-h.created > int64(maxAge)
		overBudget := maxBytes > 0 && total > maxBytes
		if !tooOld && !overBudget {
			continue
		}
		os.Remove(h.path)
		_, _ = p.db.ExecContext(ctx, `DELETE FROM holds WHERE hold_path = ?`, h.path)
		total -= h.size
	}
}

// HoldsBytes reports how much the holds occupy, for status.
func (p *Pool) HoldsBytes(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	if err := p.db.QueryRowContext(ctx, `SELECT SUM(size) FROM holds`).Scan(&n); err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	return n.Int64, nil
}

// holdOrphanGrace keeps reconcileHolds off a link that was made a moment
// ago: takeHold links the blob before it inserts the row, so a young file
// no row names may simply be a hold being taken right now.
const holdOrphanGrace = time.Minute

// reconcileHolds squares the holds directory with the holds table at
// start: a file no row names is an orphan of a crash between the link and
// the insert, and a row whose file is gone names nothing. Both go.
func (p *Pool) reconcileHolds(ctx context.Context) (orphans int) {
	dir := p.holdsDir()
	if dir == "" {
		return 0
	}
	rows, err := p.db.QueryContext(ctx, `SELECT hold_path FROM holds`)
	if err != nil {
		return 0
	}
	known := map[string]bool{}
	var missing []string
	for rows.Next() {
		var hp string
		if rows.Scan(&hp) == nil {
			known[hp] = true
			if _, err := os.Stat(hp); err != nil {
				missing = append(missing, hp)
			}
		}
	}
	rows.Close()
	for _, hp := range missing {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM holds WHERE hold_path = ?`, hp)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := p.now().Add(-holdOrphanGrace)
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if known[full] {
			continue
		}
		if fi, err := e.Info(); err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		os.Remove(full)
		orphans++
	}
	return orphans
}
