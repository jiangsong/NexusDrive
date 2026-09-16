package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/vfs"
)

// FileInfo is what the preimage and rollback code needs to know about a
// path: whether it is there, what it is, and the identity the block cache
// keys its content by.
type FileInfo struct {
	IsDir    bool
	Size     int64
	Remote   string
	RemoteID string
	Version  string
}

// FSOps is the slice of the VFS that preimage capture and rollback use,
// by path. vfs.FS is adapted to it by VFSOps; tests use an in-memory
// fake, so this package's tests need no mount, cache or journal. Errors
// follow the VFS's own sentinels: vfs.ErrNotFound for an absent path,
// vfs.ErrNotEmpty for a directory Remove refuses.
type FSOps interface {
	StatPath(ctx context.Context, p string) (FileInfo, error)
	// ReadFileRange with length 0 reads the whole file.
	ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error)
	WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (FileInfo, error)
	Mkdir(ctx context.Context, p string) error
	Remove(ctx context.Context, p string, recursive bool) error
	Rename(ctx context.Context, from, to string) error
	// HydratedPath is the local file holding all of p's content, when the
	// block cache has one, so a preimage can be a hard link rather than a
	// copy.
	HydratedPath(ctx context.Context, p string) (string, bool)
}

// Reserver is the cache's disk admission: a preimage that has to be
// copied rather than linked reserves its size around the write. nil skips
// the accounting.
type Reserver interface {
	ReserveDisk(dir string, n int64) (func(), error)
}

// Pre is what Capture found at a path before a tool changed it.
type Pre struct {
	State    string // absent | file | dir
	Remote   string
	RemoteID string
	Version  string
	Size     int64
	// Hash is the sha256 of the content that was read; Blob names the
	// file under the preimage directory that holds it, the same string.
	Hash string
	Blob string
	// Reason is "" when Blob holds the content, too_large when the file is
	// over the limit, not_cached when it could not be read in full.
	Reason string
}

// Preimages keeps the content files that overwrite, edit and delete are
// rolled back from, under <cache.dir>/agent/preimages/<sha256>. A blob is
// a hard link into the block cache's hydrated file when there is one and a
// copy otherwise, so a rollback never downloads what it is restoring. The
// rows in session_ops name the blobs; a blob no row names is an orphan
// that Recover removes, and GC drops the rows and blobs of sessions past
// their retention (docs/agent-roadmap.md §4.8).
type Preimages struct {
	dir   string
	store *Store
	cache Reserver
	max   int64

	// mu guards inflight: blobs captured but not yet named by a row.
	// Recover and GC leave those alone, so a sweep that runs between
	// Capture and Record cannot pull the content out from under the row
	// about to be written.
	mu       sync.Mutex
	inflight map[string]int

	// afterLink is a test hook that runs after a blob is linked or copied
	// and before it is reported, where a crash leaves an orphan.
	afterLink func()
}

// NewPreimages opens the preimage directory, creating it if needed. max is
// the largest file a preimage is kept for.
func NewPreimages(store *Store, dir string, cache Reserver, max int64) (*Preimages, error) {
	if store == nil || dir == "" {
		return nil, errors.New("agent: preimages need a store and a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: preimages: %w", err)
	}
	return &Preimages{dir: dir, store: store, cache: cache, max: max, inflight: map[string]int{}}, nil
}

// Dir is the blob directory.
func (p *Preimages) Dir() string { return p.dir }

// HookAfterLink installs a function that runs after a blob is linked or
// copied and before the caller learns of it. It exists for the chaos test
// that kills the process there, to prove the crash leaves an orphan blob
// and never a row without its preimage.
func (p *Preimages) HookAfterLink(f func()) { p.afterLink = f }

// Store is the agent.db the rows live in.
func (p *Preimages) Store() *Store { return p.store }

// MaxBytes is the largest file a preimage is kept for.
func (p *Preimages) MaxBytes() int64 { return p.max }

// Capture records what is at path before a tool changes it. An absent
// path and a directory carry no content. A file at most MaxBytes long is
// read in full through the ordinary read path (a cache hit costs nothing,
// an uncached file costs the one download the write would have needed
// anyway for edit_file), hashed, and kept under its hash: linked from the
// cache's hydrated file when there is one, otherwise written from the
// bytes just read. A file that is too large or could not be read gets a
// Reason instead of a Blob. Capture never refuses the write it precedes:
// the only error it returns is a cancelled context.
func (p *Preimages) Capture(ctx context.Context, fs FSOps, path string) (Pre, error) {
	if err := ctx.Err(); err != nil {
		return Pre{}, err
	}
	info, err := fs.StatPath(ctx, path)
	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return Pre{State: "absent"}, nil
	case err != nil:
		// Unknown state: keep the row conservative. A file that cannot be
		// stat'ed cannot be restored, and rollback never removes a path
		// whose pre-state was "file".
		return Pre{State: "file", Reason: "not_cached"}, nil
	}
	pre := Pre{Remote: info.Remote, RemoteID: info.RemoteID, Version: info.Version, Size: info.Size}
	if info.IsDir {
		pre.State = "dir"
		return pre, nil
	}
	pre.State = "file"
	if info.Size > p.max {
		pre.Reason = "too_large"
		return pre, nil
	}
	data, err := fs.ReadFileRange(ctx, path, 0, 0)
	if err != nil || int64(len(data)) != info.Size {
		if ctx.Err() != nil {
			return Pre{}, ctx.Err()
		}
		pre.Reason = "not_cached"
		return pre, nil
	}
	sum := sha256.Sum256(data)
	pre.Hash = hex.EncodeToString(sum[:])
	if err := p.keep(ctx, fs, path, pre.Hash, data); err != nil {
		if ctx.Err() != nil {
			return Pre{}, ctx.Err()
		}
		slog.Warn("agent: preimage not kept", "path", path, "err", err)
		pre.Reason = "not_cached"
		return pre, nil
	}
	pre.Blob = pre.Hash
	return pre, nil
}

// keep makes sure dir/<hash> holds data and marks it in flight: an existing
// blob is reused, a hydrated cache file is hard-linked, and failing both
// the bytes are written to a temporary file and renamed into place. The
// lock covers the checks and the link, not the copy: a copy of a file at
// the size limit takes a while, and every other tool's capture would wait
// on it. The in-flight mark is taken before the lock is dropped, which is
// what keeps a sweep off the blob and its temporary file meanwhile.
func (p *Preimages) keep(ctx context.Context, fs FSOps, path, hash string, data []byte) error {
	p.mu.Lock()
	blob := filepath.Join(p.dir, hash)
	if info, err := os.Stat(blob); err == nil && info.Size() == int64(len(data)) {
		p.inflight[hash]++
		p.mu.Unlock()
		return nil
	} else if err == nil {
		// Same name, wrong size: cannot be our content. Replace it.
		_ = os.Remove(blob)
	}
	linked := false
	if src, ok := fs.HydratedPath(ctx, path); ok {
		if err := os.Link(src, blob); err == nil {
			// The cache file is the file that was read only if nothing
			// replaced it in between; the size is the cheap check.
			if info, err := os.Stat(blob); err == nil && info.Size() == int64(len(data)) {
				linked = true
			} else {
				_ = os.Remove(blob)
			}
		} else if errors.Is(err, os.ErrExist) {
			// Another capture of the same content won the race.
			linked = true
		}
	}
	p.inflight[hash]++
	p.mu.Unlock()
	if !linked {
		if err := p.copy(blob, data); err != nil {
			p.release(hash)
			return err
		}
	}
	if p.afterLink != nil {
		p.afterLink()
	}
	return nil
}

// copy writes data to blob through a temporary name, reserving its size
// with the cache first so the preimage directory shares the cache's disk
// budget rather than filling the disk behind its back.
func (p *Preimages) copy(blob string, data []byte) error {
	release := func() {}
	if p.cache != nil {
		r, err := p.cache.ReserveDisk(p.dir, int64(len(data)))
		if err != nil {
			return err
		}
		release = r
	}
	defer release()
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	tmp := blob + ".tmp-" + hex.EncodeToString(nonce[:])
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, blob); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

// Record inserts the row that names a captured preimage and releases the
// in-flight hold on its blob. It is the second half of Capture.
func (p *Preimages) Record(ctx context.Context, sessionID string, op Op) (int64, error) {
	defer p.release(op.PreBlob)
	return p.store.RecordOp(ctx, sessionID, op)
}

// Discard releases a capture that will not be recorded, because the call
// failed between Capture and Record. The blob stays until Recover or GC
// finds nothing naming it.
func (p *Preimages) Discard(pre Pre) { p.release(pre.Blob) }

func (p *Preimages) release(blob string) {
	if blob == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[blob] <= 1 {
		delete(p.inflight, blob)
	} else {
		p.inflight[blob]--
	}
}

// Open returns the content of a preimage, checked against its hash, so a
// blob that was tampered with or truncated is refused rather than written
// back over a file.
func (p *Preimages) Open(blob string) ([]byte, error) {
	if blob == "" || strings.ContainsAny(blob, "/\\") {
		return nil, errors.New("agent: no preimage")
	}
	data, err := os.ReadFile(filepath.Join(p.dir, blob))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != blob {
		return nil, fmt.Errorf("agent: preimage %s does not match its hash", blob)
	}
	return data, nil
}

// Recover removes blobs no session_ops row names and the temporary files
// of interrupted copies, and reports how many. A crash between linking a
// blob and inserting its row leaves exactly such an orphan; a crash the
// other way round is impossible by construction. The owner runs it at
// start.
func (p *Preimages) Recover(ctx context.Context) (int, error) {
	return p.sweep(ctx)
}

// GCResult says what one GC pass removed.
type GCResult struct {
	Sessions int
	Ops      int64
	Blobs    int
	// Expired counts rows whose blob was released while the row stayed.
	Expired int64
}

// PreimageExpired is the pre_reason of a row whose blob the GC released
// before the row itself: history still names the write, rollback skips it.
const PreimageExpired = "expired"

// GC drops the ops of sessions that finished, expired or were rolled back
// before now-retain, releases the blobs of sessions older than retainBlobs
// (their rows stay, marked expired, so history outlives undo), then
// removes the blobs nothing names any more. It unlinks only its own
// name: a blob that is also the cache's hydrated file stays in the cache.
// An active session is never collected. retainBlobs <= 0 means blobs
// live as long as rows.
func (p *Preimages) GC(ctx context.Context, retain, retainBlobs time.Duration) (GCResult, error) {
	var res GCResult
	if retain < 0 {
		return res, nil
	}
	now := p.store.now()
	ids, err := p.sessionsFinishedBefore(ctx, now.Add(-retain).UnixNano())
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		r, err := p.store.db.ExecContext(ctx, `DELETE FROM session_ops WHERE session_id = ?`, id)
		if err != nil {
			return res, fmt.Errorf("agent: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Sessions++
		res.Ops += n
	}
	expired := int64(0)
	if retainBlobs > 0 && retainBlobs < retain {
		ids, err := p.sessionsFinishedBefore(ctx, now.Add(-retainBlobs).UnixNano())
		if err != nil {
			return res, err
		}
		for _, id := range ids {
			r, err := p.store.db.ExecContext(ctx, `UPDATE session_ops SET pre_blob = '', pre_reason = ? WHERE session_id = ? AND pre_blob != ''`, PreimageExpired, id)
			if err != nil {
				return res, fmt.Errorf("agent: %w", err)
			}
			n, _ := r.RowsAffected()
			expired += n
		}
	}
	res.Expired = expired
	if res.Sessions == 0 && expired == 0 {
		return res, nil
	}
	blobs, err := p.sweep(ctx)
	res.Blobs = blobs
	return res, err
}

// sessionsFinishedBefore lists the sessions with ops that ended before
// cutoff.
func (p *Preimages) sessionsFinishedBefore(ctx context.Context, cutoff int64) ([]string, error) {
	rows, err := p.store.db.QueryContext(ctx, `SELECT DISTINCT o.session_id FROM session_ops o JOIN sessions s ON s.id = o.session_id
		WHERE s.state IN ('finished', 'expired', 'rolled_back') AND s.finished_at > 0 AND s.finished_at < ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return ids, nil
}

// sweep removes every blob no row names and is not in flight, plus the
// temporary files of copies that are not in flight either.
func (p *Preimages) sweep(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return 0, fmt.Errorf("agent: preimages: %w", err)
	}
	referenced := map[string]bool{}
	rows, err := p.store.db.QueryContext(ctx, `SELECT DISTINCT pre_blob FROM session_ops WHERE pre_blob != ''`)
	if err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			rows.Close()
			return 0, fmt.Errorf("agent: %w", err)
		}
		referenced[blob] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("agent: %w", err)
	}
	removed := 0
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if base, _, isTmp := strings.Cut(name, ".tmp"); isTmp {
			if p.inflight[base] == 0 && os.Remove(filepath.Join(p.dir, name)) == nil {
				removed++
			}
			continue
		}
		if referenced[name] || p.inflight[name] > 0 {
			continue
		}
		if os.Remove(filepath.Join(p.dir, name)) == nil {
			removed++
		}
	}
	return removed, nil
}

// RunGC runs GC now and then every interval until ctx ends, in the owner
// only, the way RunAuditRetention does for audit rows.
func (p *Preimages) RunGC(ctx context.Context, retain, retainBlobs, every time.Duration) {
	if !p.store.owner || retain <= 0 {
		return
	}
	if every <= 0 {
		every = time.Hour
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if res, err := p.GC(ctx, retain, retainBlobs); err != nil {
			if ctx.Err() == nil {
				slog.Warn("agent: preimage gc failed", "err", err)
			}
		} else if res.Sessions > 0 {
			slog.Debug("agent: preimage gc", "sessions", res.Sessions, "ops", res.Ops, "blobs", res.Blobs, "retain", retain)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
