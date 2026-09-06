package pool

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"cloudfs/internal/provider"
)

// Repair brings every file up to its replica target. Work arrives from the
// write path (a fresh upload has one replica), from the scan (a file with
// fewer live replicas than the target, because a member is out or the file
// came with an adopted drive) and from the operator. A copy is read from
// the hold the pool kept of the bytes when there is one, and from a live
// replica otherwise; it is written where the file already has a stale copy
// first — refreshing in place keeps the mirrored tree tidy — and then to
// the members that do not hold it, one member at a time, so repair never
// competes with itself for a drive.

// RepairStats summarises the queue.
type RepairStats struct {
	Queued  int
	Blocked int // waiting for a member or a retry
}

// repairInterval is how often the worker looks at the queue; scanInterval
// how often it looks for under-replicated files the queue does not know.
const (
	repairInterval = 5 * time.Second
	scanInterval   = 10 * time.Minute
	repairBatch    = 16
)

// replicaTarget is how many replicas a file can have right now: the
// configured count, capped by the members that can take one.
func (p *Pool) replicaTarget() (target int, capped bool) {
	eligible := 0
	for _, m := range p.members {
		if st := m.health.Snapshot(); st.State != provider.HealthOut && st.State != provider.HealthDisabled && st.State != provider.HealthDraining {
			eligible++
		}
	}
	target = p.settings.Replicas
	if target < 1 {
		target = 1
	}
	if eligible < target {
		return eligible, true
	}
	return target, false
}

// liveReplicas returns the replicas that carry the entry's content on
// members that are in service.
func (p *Pool) liveReplicas(ctx context.Context, pth, ctoken string) ([]replicaRow, error) {
	all, err := p.replicasOf(ctx, pth, ctoken)
	if err != nil {
		return nil, err
	}
	var out []replicaRow
	for _, r := range all {
		if r.state != "live" {
			continue
		}
		m := p.byName[r.member]
		if m == nil {
			continue
		}
		switch m.state() {
		case provider.HealthOut, provider.HealthDisabled, provider.HealthDraining:
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// enqueueRepair records that pth needs attention.
func (p *Pool) enqueueRepair(ctx context.Context, pth, reason string, priority int) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO repair_queue(path, reason, priority, next_at, attempts, source_hint, created_at) VALUES(?, ?, ?, 0, 0, '', ?)
		ON CONFLICT(path) DO UPDATE SET reason = excluded.reason, priority = MAX(priority, excluded.priority), next_at = 0`, pth, reason, priority, p.now().UnixNano())
	return err
}

// ScanOnce queues every file whose live replica count is below what the
// pool can hold right now. It is how a file that came with an adopted
// drive, or lost a replica to a member that went out, reaches the queue.
func (p *Pool) ScanOnce(ctx context.Context) (int, error) {
	target, _ := p.replicaTarget()
	rows, err := p.db.QueryContext(ctx, `SELECT path, ctoken FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile))
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	type file struct{ path, ctoken string }
	var files []file
	for rows.Next() {
		var f file
		if err := rows.Scan(&f.path, &f.ctoken); err != nil {
			rows.Close()
			return 0, err
		}
		files = append(files, f)
	}
	rows.Close()
	queued := 0
	for _, f := range files {
		live, err := p.liveReplicas(ctx, f.path, f.ctoken)
		if err != nil {
			return queued, err
		}
		if len(live) >= target {
			continue
		}
		if err := p.enqueueRepair(ctx, f.path, "under-replicated", 0); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

// RepairOnce works through the due part of the queue and reports how many
// replicas it made.
func (p *Pool) RepairOnce(ctx context.Context) (int, error) {
	now := p.now().UnixNano()
	rows, err := p.db.QueryContext(ctx, `SELECT path FROM repair_queue WHERE next_at <= ? ORDER BY priority DESC, created_at ASC LIMIT ?`, now, repairBatch)
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	var paths []string
	for rows.Next() {
		var pth string
		if err := rows.Scan(&pth); err != nil {
			rows.Close()
			return 0, err
		}
		paths = append(paths, pth)
	}
	rows.Close()
	made := 0
	for _, pth := range paths {
		if ctx.Err() != nil {
			return made, ctx.Err()
		}
		n, err := p.repairPath(ctx, pth)
		made += n
		if err != nil && ctx.Err() != nil {
			return made, err
		}
	}
	return made, nil
}

// repairPath makes the copies one file is missing.
func (p *Pool) repairPath(ctx context.Context, pth string) (int, error) {
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return 0, err
	}
	if !ok || row.kind != provider.KindFile {
		_, _ = p.db.ExecContext(ctx, `DELETE FROM repair_queue WHERE path = ?`, pth)
		return 0, nil
	}
	target, capped := p.replicaTarget()
	live, err := p.liveReplicas(ctx, pth, row.ctoken)
	if err != nil {
		return 0, err
	}
	made := 0
	var lastErr error
	for len(live) < target {
		dst := p.repairTarget(ctx, pth, live)
		if dst == nil {
			break
		}
		if err := p.copyReplica(ctx, pth, row, live, dst); err != nil {
			lastErr = err
			if !unreachable(err) {
				_ = p.tx(ctx, func(tx *sql.Tx) error {
					recordDivergence(tx, pth, dst.name, "repair-failed", err.Error(), p.now().UnixNano())
					return nil
				})
			}
			// Try the next candidate; the member that failed is behind
			// its own health now.
			live = append(live, replicaRow{member: dst.name, state: "failed"})
			continue
		}
		made++
		live, err = p.liveReplicas(ctx, pth, row.ctoken)
		if err != nil {
			return made, err
		}
	}
	live, _ = p.liveReplicas(ctx, pth, row.ctoken)
	switch {
	case len(live) >= target:
		// Done, for now: the hold has served its purpose. A capped target
		// leaves the file to the scan, which re-queues it when a member
		// comes back.
		_, _ = p.db.ExecContext(ctx, `DELETE FROM repair_queue WHERE path = ?`, pth)
		if len(live) >= p.settings.Replicas || capped {
			p.releaseHolds(ctx, pth)
		}
	default:
		var attempts int
		_ = p.db.QueryRowContext(ctx, `SELECT attempts FROM repair_queue WHERE path = ?`, pth).Scan(&attempts)
		delay := 30 * time.Second << uint(min(attempts, 7))
		if delay > time.Hour {
			delay = time.Hour
		}
		reason := "retry"
		if lastErr != nil {
			reason = "retry: " + lastErr.Error()
		}
		_, _ = p.db.ExecContext(ctx, `UPDATE repair_queue SET attempts = attempts + 1, next_at = ?, reason = ? WHERE path = ?`, p.now().Add(delay).UnixNano(), reason, pth)
	}
	return made, lastErr
}

// repairTarget picks the member to receive the next copy: one that already
// has a stale or pending copy at the path (refreshed in place), then one
// that has none, both in declaration order and only if usable.
func (p *Pool) repairTarget(ctx context.Context, pth string, live []replicaRow) *member {
	taken := map[string]bool{}
	for _, r := range live {
		taken[r.member] = true
	}
	holding := map[string]bool{}
	rows, err := p.db.QueryContext(ctx, `SELECT member FROM replicas WHERE path = ?`, pth)
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				holding[m] = true
			}
		}
		rows.Close()
	}
	probe := p.probeInterval()
	eligible := func(m *member) bool {
		if taken[m.name] {
			return false
		}
		switch m.state() {
		case provider.HealthOut, provider.HealthDisabled, provider.HealthDraining:
			return false
		}
		return m.usable(probe)
	}
	for _, m := range p.members {
		if holding[m.name] && eligible(m) {
			return m
		}
	}
	for _, m := range p.members {
		if !holding[m.name] && eligible(m) {
			return m
		}
	}
	return nil
}

// source is where a copy's bytes come from.
type source struct {
	io.ReaderAt
	size   int64
	hashes provider.Hashes
	close  func()
}

// openSource prefers the local hold, then a live replica.
func (p *Pool) openSource(ctx context.Context, pth string, row entryRow, live []replicaRow) (*source, error) {
	if hp, ok := p.holdFor(ctx, pth, row.ctoken); ok {
		f, err := os.Open(hp)
		if err == nil {
			hashes, err := hashFile(f, row.size)
			if err == nil {
				return &source{ReaderAt: f, size: row.size, hashes: hashes, close: func() { f.Close() }}, nil
			}
			f.Close()
		}
	}
	hashes := provider.Hashes{}
	if row.hashType != "" {
		hashes[provider.HashType(row.hashType)] = row.hash
	}
	probe := p.probeInterval()
	// Any live copy will do as a source, including one on a member being
	// drained — that is the copy we are moving.
	sources, err := p.replicasOf(ctx, pth, row.ctoken)
	if err != nil {
		return nil, err
	}
	for _, r := range sources {
		m := p.byName[r.member]
		if m == nil || r.state != "live" || !m.usable(probe) {
			continue
		}
		if st := m.state(); st == provider.HealthOut || st == provider.HealthDisabled {
			continue
		}
		return &source{ReaderAt: &replicaReader{ctx: ctx, m: m, r: r}, size: row.size, hashes: hashes, close: func() {}}, nil
	}
	_ = live
	return nil, fmt.Errorf("%w: no source for %s", provider.ErrUnavailable, pth)
}

// replicaReader reads a live replica through the member's range read.
type replicaReader struct {
	ctx context.Context
	m   *member
	r   replicaRow
}

func (rr *replicaReader) ReadAt(buf []byte, off int64) (int, error) {
	if ra, ok := rr.m.p.(provider.RangeReaderAt); ok {
		n, err := ra.ReadRangeAt(rr.ctx, rr.r.remoteID, rr.r.version, off, buf)
		if !errors.Is(err, provider.ErrUnsupported) {
			rr.m.note(err)
			return n, err
		}
	}
	rc, err := rr.m.p.ReadRange(rr.ctx, rr.r.remoteID, rr.r.version, off, int64(len(buf)))
	rr.m.note(err)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return io.ReadFull(rc, buf)
}

// hashFile computes the content hashes a member may want for a hash-only
// upload of a local file.
func hashFile(f *os.File, size int64) (provider.Hashes, error) {
	s1, m5, s256 := sha1.New(), md5.New(), sha256.New()
	if _, err := io.Copy(io.MultiWriter(s1, m5, s256), io.NewSectionReader(f, 0, size)); err != nil {
		return nil, err
	}
	hexOf := func(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
	return provider.Hashes{provider.HashSHA1: hexOf(s1), provider.HashMD5: hexOf(m5), provider.HashSHA256: hexOf(s256)}, nil
}

// copyReplica puts the entry's content on dst at its real path, and
// records the new replica with the entry's own content token: a copy is
// the same content, whatever mtime the member stamps on it.
func (p *Pool) copyReplica(ctx context.Context, pth string, row entryRow, live []replicaRow, dst *member) error {
	src, err := p.openSource(ctx, pth, row, live)
	if err != nil {
		return err
	}
	defer src.close()
	dirID, err := p.ensureDir(ctx, dst, parentOf(pth))
	if err != nil {
		return err
	}
	name := path.Base(pth)
	var e provider.Entry
	caps := dst.p.Capabilities()
	if sp, ok := dst.p.(provider.SinglePutter); ok && caps.SinglePutMax > 0 && src.size <= caps.SinglePutMax {
		e, err = sp.PutFile(ctx, dirID, name, io.NewSectionReader(src, 0, src.size), src.size, src.hashes)
		dst.note(err)
		if err != nil {
			return err
		}
	} else {
		sess, err := dst.p.BeginUpload(ctx, dirID, name, src.size, src.hashes)
		dst.note(err)
		if err != nil {
			return err
		}
		if sess.RapidDone && sess.Entry != nil {
			e = *sess.Entry
		} else {
			partSize := sess.PartSize
			if partSize <= 0 {
				partSize = caps.PartSize
			}
			if partSize <= 0 {
				partSize = 4 << 20
			}
			nParts := int((src.size + partSize - 1) / partSize)
			if nParts == 0 {
				nParts = 1
			}
			parts := make([]provider.PartToken, 0, nParts)
			for i := 0; i < nParts; i++ {
				off := int64(i) * partSize
				n := partSize
				if off+n > src.size {
					n = src.size - off
				}
				pt, err := dst.p.UploadPart(ctx, sess, i, io.NewSectionReader(src, off, n), n)
				dst.note(err)
				if err != nil {
					return err
				}
				parts = append(parts, pt)
			}
			e, err = dst.p.CompleteUpload(ctx, sess, parts)
			dst.note(err)
			if err != nil {
				return err
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tx(ctx, func(tx *sql.Tx) error {
		return upsertReplica(tx, pth, parentOf(pth), dst.name, e, row.ctoken, "live", p.now().UnixNano())
	})
}

// RepairStatus reports the queue.
func (p *Pool) RepairStatus(ctx context.Context) (RepairStats, error) {
	var st RepairStats
	now := p.now().UnixNano()
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(next_at > ?), 0) FROM repair_queue`, now).Scan(&st.Queued, &st.Blocked); err != nil {
		return st, fmt.Errorf("pool: %w", err)
	}
	return st, nil
}

// underReplicated counts files below the current target, for status.
func (p *Pool) underReplicated(ctx context.Context) (int, error) {
	target, _ := p.replicaTarget()
	rows, err := p.db.QueryContext(ctx, `SELECT path, ctoken FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile))
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	n := 0
	var pending []struct{ path, ctoken string }
	for rows.Next() {
		var pth, ct string
		if err := rows.Scan(&pth, &ct); err != nil {
			return 0, err
		}
		pending = append(pending, struct{ path, ctoken string }{pth, ct})
	}
	for _, f := range pending {
		live, err := p.liveReplicas(ctx, f.path, f.ctoken)
		if err != nil {
			return n, err
		}
		if len(live) < target {
			n++
		}
	}
	return n, nil
}

var _ = sort.Strings
