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
	"strings"
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
		target, _ := p.targetFor(f.path)
		if len(live) >= target {
			continue
		}
		// Below min_replicas the next member failing loses the file, so
		// those repairs go to the front of the queue.
		reason, priority := "under-replicated", 0
		if len(live) < p.minReplicas(f.path) {
			reason, priority = "below min_replicas", 2
		}
		if err := p.enqueueRepair(ctx, f.path, reason, priority); err != nil {
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
	rows, err := p.db.QueryContext(ctx, `SELECT path, reason FROM repair_queue WHERE next_at <= ? ORDER BY priority DESC, created_at ASC LIMIT ?`, now, repairBatch)
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	type due struct{ path, reason string }
	var paths []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.path, &d.reason); err != nil {
			rows.Close()
			return 0, err
		}
		paths = append(paths, d)
	}
	rows.Close()
	made := 0
	for _, d := range paths {
		if ctx.Err() != nil {
			return made, ctx.Err()
		}
		pth := d.path
		if strings.HasPrefix(d.reason, reasonCopyUnsure) {
			// A server-side copy failed in a way that does not say
			// whether it landed. Re-list before deciding: a copy that did
			// land is a replica like any other, and sending the file
			// again would make a second one.
			if err := p.ScrubPath(ctx, pth); err != nil && ctx.Err() != nil {
				return made, err
			}
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
	target, capped := p.targetFor(pth)
	live, err := p.liveReplicas(ctx, pth, row.ctoken)
	if err != nil {
		return 0, err
	}
	made := 0
	var lastErr error
	noMember := false
	for len(live) < target {
		dst := p.repairTarget(ctx, pth, live)
		if dst == nil {
			noMember = true
			break
		}
		if err := p.copyReplica(ctx, pth, row, live, dst); err != nil {
			lastErr = err
			// A full member is not a disagreement about the file: it is
			// a drive with no room, already marked full, and the next
			// candidate gets the copy.
			if !unreachable(err) && !outOfSpace(err) {
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
		if len(live) >= p.wantReplicas(pth) || capped {
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
		if errors.Is(lastErr, errCopyUnconfirmed) {
			// Nobody knows whether that copy landed. The next pass must
			// re-list before it writes, or a copy that did land becomes
			// two.
			reason = reasonCopyUnsure + ": " + lastErr.Error()
		}
		if noMember && lastErr == nil {
			// Nothing in service can take another copy: the name may be
			// one the other members refuse. Look again in an hour, or
			// when the scan finds the situation changed.
			reason = "no-eligible-member"
			delay = time.Hour
		}
		_, _ = p.db.ExecContext(ctx, `UPDATE repair_queue SET attempts = attempts + 1, next_at = ?, reason = ? WHERE path = ?`, p.now().Add(delay).UnixNano(), reason, pth)
	}
	return made, lastErr
}

// repairTarget picks the member to receive the next copy: the best
// candidate for the path that is not already carrying a live one. It is
// candidates() minus the current holders on purpose — a second placement
// policy here is how repair stopped honouring rules and failure domains
// (docs/pool-v2.md §6.2).
func (p *Pool) repairTarget(ctx context.Context, pth string, live []replicaRow) *member {
	taken := map[string]bool{}
	for _, r := range live {
		taken[r.member] = true
	}
	for _, m := range p.candidates(ctx, pth) {
		if !taken[m.name] {
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
// reasonCopyUnsure marks a repair whose server-side copy failed without
// saying whether it landed; errCopyUnconfirmed is how copyReplica says so
// to the queue.
const reasonCopyUnsure = "server-copy-unsure"

var errCopyUnconfirmed = errors.New("pool: server-side copy is unconfirmed")

// serverCopy asks the destination member to duplicate the file from a
// replica it can reach itself: the same failure domain is the same
// account, where a backend copies without the bytes travelling through
// this machine. It reports whether it handled the copy at all, so the
// caller falls back to sending the bytes when it did not.
func (p *Pool) serverCopy(ctx context.Context, pth string, row entryRow, live []replicaRow, dst *member, dirID string) (provider.Entry, bool, error) {
	sc, ok := dst.p.(provider.ServerCopier)
	if !ok || !dst.p.Capabilities().ServerCopy {
		return provider.Entry{}, false, nil
	}
	var srcID string
	for _, r := range live {
		m := p.byName[r.member]
		if m == nil || m.name == dst.name || m.domain != dst.domain {
			continue
		}
		if !m.usable(p.probeInterval()) {
			continue
		}
		srcID = r.remoteID
		break
	}
	if srcID == "" {
		return provider.Entry{}, false, nil
	}
	e, err := sc.Copy(ctx, srcID, dirID, path.Base(pth))
	switch {
	case err == nil:
		dst.note(nil)
		return e, true, nil
	case errors.Is(err, provider.ErrUnsupported), errors.Is(err, provider.ErrNotFound):
		// The backend cannot copy this, or the source moved under us.
		// Sending the bytes is still an option.
		dst.note(nil)
		return provider.Entry{}, false, nil
	case outOfSpace(err):
		return provider.Entry{}, true, p.noteWrite(dst, err)
	}
	// Anything else — a timeout, a 5xx — leaves it unknown whether the
	// copy landed. Do not send the bytes now: re-list first, next pass.
	dst.note(err)
	return provider.Entry{}, true, fmt.Errorf("%w: %s to %s: %w", errCopyUnconfirmed, pth, dst.name, err)
}

func (p *Pool) copyReplica(ctx context.Context, pth string, row entryRow, live []replicaRow, dst *member) error {
	dirID, err := p.ensureDir(ctx, dst, parentOf(pth))
	if err != nil {
		return err
	}
	if e, handled, err := p.serverCopy(ctx, pth, row, live, dst, dirID); handled {
		if err != nil {
			return err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.tx(ctx, func(tx *sql.Tx) error {
			return upsertReplica(tx, pth, parentOf(pth), dst.name, e, row.ctoken, "live", p.now().UnixNano())
		})
	}
	src, err := p.openSource(ctx, pth, row, live)
	if err != nil {
		return err
	}
	defer src.close()
	name := path.Base(pth)
	var e provider.Entry
	caps := dst.p.Capabilities()
	if sp, ok := dst.p.(provider.SinglePutter); ok && caps.SinglePutMax > 0 && src.size <= caps.SinglePutMax {
		e, err = sp.PutFile(ctx, dirID, name, io.NewSectionReader(src, 0, src.size), src.size, src.hashes)
		err = p.noteWrite(dst, err)
		if err != nil {
			return err
		}
	} else {
		sess, err := dst.p.BeginUpload(ctx, dirID, name, src.size, src.hashes)
		err = p.noteWrite(dst, err)
		if err != nil {
			if refusedName(err) {
				p.learnDenial(ctx, dst, name)
			}
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
				err = p.noteWrite(dst, err)
				if err != nil {
					return err
				}
				parts = append(parts, pt)
			}
			e, err = dst.p.CompleteUpload(ctx, sess, parts)
			err = p.noteWrite(dst, err)
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

// underReplicated counts files below the current target, and separately
// those below min_replicas, for status.
func (p *Pool) underReplicated(ctx context.Context) (under int, belowMin int, err error) {
	rows, err := p.db.QueryContext(ctx, `SELECT path, ctoken FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile))
	if err != nil {
		return 0, 0, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var pending []struct{ path, ctoken string }
	for rows.Next() {
		var pth, ct string
		if err := rows.Scan(&pth, &ct); err != nil {
			return 0, 0, err
		}
		pending = append(pending, struct{ path, ctoken string }{pth, ct})
	}
	for _, f := range pending {
		live, err := p.liveReplicas(ctx, f.path, f.ctoken)
		if err != nil {
			return under, belowMin, err
		}
		if target, _ := p.targetFor(f.path); len(live) < target {
			under++
		}
		if len(live) < p.minReplicas(f.path) {
			belowMin++
		}
	}
	return under, belowMin, nil
}

var _ = sort.Strings
