package export

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// planBatch is how many rows one planning transaction writes. A cold tree of
// a million files should not be one transaction, and it should not be a
// million either.
const planBatch = 500

// mtimeSlack is how far apart two modification times may be and still count
// as the same file. exFAT keeps two-second granularity, which is also what
// the pool's change tokens allow for (internal/pool/ctoken.go): anything
// tighter would re-copy an entire drive after a round trip through it.
const mtimeSlack = 2 * time.Second

// plan walks the sources and writes the job's items. It is the only phase
// that touches the namespace, and on a warm tree it costs no provider call at
// all: vfs.ReadDirPath serves a fresh directory out of the metadata store.
func (m *Manager) plan(ctx context.Context, job Job) error {
	// A job interrupted while planning has nothing on the destination yet,
	// so it is replanned from scratch rather than resumed half-walked.
	if err := m.store.ClearPlan(ctx, job.ID); err != nil {
		return err
	}
	b := &planner{m: m, job: job, seen: map[string]bool{}}
	for _, src := range job.Sources {
		if err := b.source(ctx, src); err != nil {
			return err
		}
	}
	if err := b.flush(ctx); err != nil {
		return err
	}
	if job.Options.Mirror {
		extras, err := b.extras()
		if err != nil {
			return err
		}
		if err := m.store.AddExtras(ctx, job.ID, extras); err != nil {
			return err
		}
	}
	if err := writeMarker(job.Dest, marker{
		Sources: job.Sources, JobID: job.ID, MetaIdentity: job.MetaIdentity, UpdatedAt: m.now(),
	}); err != nil {
		return err
	}
	return m.store.RefreshCounters(ctx, job.ID)
}

type planner struct {
	m     *Manager
	job   Job
	batch []Item
	// seen holds the planned relative paths, for the mirror's deletion plan.
	// It is only filled when a mirror asked for one.
	seen map[string]bool
}

func (p *planner) add(ctx context.Context, it Item) error {
	if p.job.Options.Mirror {
		p.seen[it.Rel] = true
	}
	p.batch = append(p.batch, it)
	if len(p.batch) >= planBatch {
		return p.flush(ctx)
	}
	return nil
}

func (p *planner) flush(ctx context.Context) error {
	if len(p.batch) == 0 {
		return nil
	}
	err := p.m.store.AddItems(ctx, p.batch)
	p.batch = p.batch[:0]
	return err
}

// relRoot is where one source lands below the destination. Exporting
// /photos/2024 produces <dest>/2024, so several sources never collide and a
// second export into the same drive is recognisable by name. The whole mount
// is the exception: its content goes straight into the destination.
func relRoot(src string) string {
	src = path.Clean("/" + strings.Trim(src, "/"))
	if src == "/" {
		return ""
	}
	return path.Base(src)
}

func (p *planner) source(ctx context.Context, src string) error {
	a, err := p.m.fs.StatPath(ctx, src)
	if err != nil {
		return fmt.Errorf("export: source %s: %w", src, err)
	}
	rel := relRoot(src)
	if !a.IsDir {
		return p.file(ctx, src, rel, a)
	}
	if rel != "" {
		if err := p.dir(ctx, src, rel, a); err != nil {
			return err
		}
	}
	return p.walk(ctx, src, rel)
}

func (p *planner) walk(ctx context.Context, vdir, rel string) error {
	entries, err := p.m.fs.ReadDirPath(ctx, vdir)
	if err != nil {
		return fmt.Errorf("export: list %s: %w", vdir, err)
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		child := path.Join(vdir, e.Name)
		childRel := e.Name
		if rel != "" {
			childRel = rel + "/" + e.Name
		}
		if e.IsDir {
			if err := p.dir(ctx, child, childRel, e); err != nil {
				return err
			}
			if err := p.walk(ctx, child, childRel); err != nil {
				return err
			}
			continue
		}
		if err := p.file(ctx, child, childRel, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) dir(ctx context.Context, vpath, rel string, a vfs.Attr) error {
	// Directories are not transferred: they are created on demand by the
	// files inside them, and their modification times are stamped in
	// post-order once the job finishes. The row exists so that stamping has
	// something to read and so a mirror knows the directory belongs here.
	return p.add(ctx, Item{
		JobID: p.job.ID, Rel: rel, VPath: vpath, Kind: provider.KindDir,
		MTime: a.MTime, State: ItemDone,
	})
}

func (p *planner) file(ctx context.Context, vpath, rel string, a vfs.Attr) error {
	it := Item{
		JobID: p.job.ID, Rel: rel, VPath: vpath, Kind: provider.KindFile,
		Size: a.Size, MTime: a.MTime, Remote: a.Remote, Version: a.Version, State: ItemPending,
	}
	if rel == markerName || strings.HasSuffix(rel, "/"+markerName) {
		// The destination's own bookkeeping file cannot be overwritten by a
		// source file of the same name; say so instead of silently losing
		// one of the two.
		it.State, it.LastError = ItemFailed, "the destination reserves this name for the export marker"
		return p.add(ctx, it)
	}
	// The node, not the attribute, carries the backend identity and content
	// hash. This is a metadata-store read: no provider call, warm or cold.
	n, err := p.m.fs.Meta().Get(ctx, a.Ino)
	if err != nil {
		if errors.Is(err, meta.ErrNotFound) {
			it.State, it.LastError = ItemFailed, "the source disappeared while the export was being planned"
			return p.add(ctx, it)
		}
		return fmt.Errorf("export: %s: %w", vpath, err)
	}
	it.RemoteID, it.Version, it.Remote = n.RemoteID, n.Version, n.Remote
	it.HashType, it.Hash = n.HashType, n.Hash
	if it.RemoteID == "" {
		it.State, it.LastError = ItemFailed, "the source has no committed content yet"
		return p.add(ctx, it)
	}
	if skip, err := p.alreadyThere(it); err != nil {
		return err
	} else if skip {
		it.State, it.DoneBytes = ItemSkipped, it.Size
		it.Ranges = fullBitmap(it.Size, p.job.Options.RangeSize)
	}
	return p.add(ctx, it)
}

// alreadyThere reports whether the destination already holds this file. Size
// plus modification time is the cheap answer; --verify pays for a local hash
// on top of it, which is pure local IO and asks no backend anything.
func (p *planner) alreadyThere(it Item) (bool, error) {
	dest := filepath.Join(p.job.Dest, filepath.FromSlash(it.Rel))
	st, err := os.Stat(dest)
	if err != nil {
		return false, nil
	}
	if st.IsDir() || st.Size() != it.Size {
		return false, nil
	}
	if d := st.ModTime().Sub(it.MTime); d > mtimeSlack || d < -mtimeSlack {
		return false, nil
	}
	if !p.job.Options.Verify || it.HashType == "" || it.Hash == "" {
		return true, nil
	}
	sum, err := hashFile(dest, it.HashType)
	if errors.Is(err, errUnknownHash) {
		return true, nil
	}
	if err != nil {
		return false, nil
	}
	return sum == it.Hash, nil
}

// extras lists what a mirror would delete: everything under the destination
// roots this job owns that the plan does not name.
func (p *planner) extras() ([]Extra, error) {
	roots := map[string]bool{}
	for _, src := range p.job.Sources {
		roots[relRoot(src)] = true
	}
	var out []Extra
	for root := range roots {
		base := p.job.Dest
		if root != "" {
			base = filepath.Join(p.job.Dest, filepath.FromSlash(root))
		}
		err := filepath.WalkDir(base, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			rel, rerr := filepath.Rel(p.job.Dest, name)
			if rerr != nil {
				return rerr
			}
			if rel == "." {
				return nil
			}
			slash := filepath.ToSlash(rel)
			// The marker proves this directory is ours; a part file belongs
			// to a transfer, finished or not. Neither is a stray.
			if slash == markerName || strings.HasSuffix(slash, partSuffix) {
				return nil
			}
			if p.seen[slash] {
				return nil
			}
			kind := provider.KindFile
			if d.IsDir() {
				kind = provider.KindDir
			}
			out = append(out, Extra{Rel: slash, Kind: kind})
			if d.IsDir() {
				// Its children are listed too, deepest first, so the
				// directory can be removed after they are gone.
				return nil
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("export: mirror scan: %w", err)
		}
	}
	return out, nil
}

// fullBitmap is the bitmap of a file that is already complete.
func fullBitmap(size, rangeSize int64) string {
	chunks := chunkCount(size, rangeSize)
	b := parseBitmap("", chunks)
	for i := 0; i < chunks; i++ {
		b.set(i)
	}
	return b.String()
}

var errUnknownHash = errors.New("export: unsupported hash type")

func newHash(kind string) (hash.Hash, error) {
	switch provider.HashType(kind) {
	case provider.HashSHA1:
		return sha1.New(), nil
	case provider.HashMD5:
		return md5.New(), nil
	case provider.HashSHA256:
		return sha256.New(), nil
	}
	return nil, errUnknownHash
}

// hashFile digests a local file. Every caller is checking bytes that are
// already on the destination disk, so this never touches a backend.
func hashFile(name, kind string) (string, error) {
	h, err := newHash(kind)
	if err != nil {
		return "", err
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
