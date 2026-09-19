package pool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// The write path. An upload streams to one member — the primary for that
// file — through a pass-through session, so the journal's part records,
// resume and progress keep working unchanged; the other replicas are made
// afterwards by repair, from the hold the pool keeps of the bytes. The tree
// changes only in CompleteUpload: a session that fails or is cancelled has
// destroyed nothing. Tree operations fan out to every member holding the
// entry; a member that cannot be reached gets a pending op to replay later,
// and the operation succeeds once one member has taken it.

// Keys the pool adds to the session it hands the uploader. The member's own
// session is flattened under memberPrefix.
const (
	sessMember   = "pool.member"
	sessPath     = "pool.path"
	sessHold     = "pool.hold"
	memberPrefix = "m."
	memberID     = "m.id"
	memberParts  = "m.part_size"
)

func packSession(m *member, pth, hold string, ms provider.UploadSession) provider.UploadSession {
	opaque := map[string]string{sessMember: m.name, sessPath: pth, sessHold: hold, memberID: ms.ID, memberParts: strconv.FormatInt(ms.PartSize, 10)}
	for k, v := range ms.Opaque {
		opaque[memberPrefix+k] = v
	}
	return provider.UploadSession{ID: "ps-" + newID()[len(idPrefix):], PartSize: ms.PartSize, Opaque: opaque}
}

func (p *Pool) unpackSession(s provider.UploadSession) (*member, string, string, provider.UploadSession, error) {
	m := p.byName[s.Opaque[sessMember]]
	if m == nil {
		return nil, "", "", provider.UploadSession{}, fmt.Errorf("%w: upload session names no member of the pool", provider.ErrNotFound)
	}
	ms := provider.UploadSession{ID: s.Opaque[memberID], Opaque: map[string]string{}}
	ms.PartSize, _ = strconv.ParseInt(s.Opaque[memberParts], 10, 64)
	for k, v := range s.Opaque {
		if strings.HasPrefix(k, memberPrefix) && k != memberID && k != memberParts {
			ms.Opaque[strings.TrimPrefix(k, memberPrefix)] = v
		}
	}
	return m, s.Opaque[sessPath], s.Opaque[sessHold], ms, nil
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || hidden(name) || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: name %q", provider.ErrUnsupported, name)
	}
	return nil
}

func (p *Pool) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if err := validName(name); err != nil {
		return provider.UploadSession{}, err
	}
	parentPath, err := p.pathOf(ctx, parentID)
	if err != nil {
		return provider.UploadSession{}, err
	}
	pth := joinPath(parentPath, name)
	var lastUnreachable, lastRefusal, lastFull error
	for _, m := range p.candidates(ctx, pth) {
		dirID, err := p.ensureDir(ctx, m, parentPath)
		if err != nil {
			if unreachable(err) {
				lastUnreachable = err
				continue
			}
			return provider.UploadSession{}, err
		}
		ms, err := m.p.BeginUpload(ctx, dirID, name, size, h)
		if err != nil {
			if outOfSpace(err) {
				// The member has no room. That is an answer about the
				// drive, not about the file: mark it full and place the
				// file on the next candidate.
				m.note(nil)
				m.markFull(p.now())
				lastFull = err
				continue
			}
			if unreachable(err) {
				m.note(err)
				lastUnreachable = err
				continue
			}
			if refusedName(err) {
				// The member will not spell this name; remember that
				// and place elsewhere.
				m.note(nil)
				p.learnDenial(ctx, m, name)
				lastRefusal = err
				continue
			}
			return provider.UploadSession{}, err
		}
		m.note(nil)
		hold, _ := p.takeHold(ctx, provider.UploadBlobLinkFrom(ctx), pth, size)
		if ms.RapidDone && ms.Entry != nil {
			e, err := p.finishUpload(ctx, m, pth, *ms.Entry, hold)
			if err != nil {
				return provider.UploadSession{}, err
			}
			p.awaitMinReplicas(ctx, pth)
			return provider.UploadSession{ID: "rapid-" + e.ID, RapidDone: true, Entry: &e}, nil
		}
		return packSession(m, pth, hold, ms), nil
	}
	if lastFull != nil {
		// Every member that could have taken it is full. The pool is out
		// of space, which the caller must hear as such: retrying cannot
		// help, and a write to a plain full drive fails the same way.
		return provider.UploadSession{}, fmt.Errorf("%w: no member of pool %s has room for %s", provider.ErrQuotaExceeded, p.name, pth)
	}
	if lastUnreachable != nil {
		return provider.UploadSession{}, fmt.Errorf("%w: no member can take %s (%v)", provider.ErrUnavailable, pth, lastUnreachable)
	}
	if lastRefusal != nil {
		return provider.UploadSession{}, lastRefusal
	}
	if len(p.candidates(ctx, pth)) == 0 {
		if err := p.whyNoMember(ctx, pth); err != nil {
			return provider.UploadSession{}, err
		}
	}
	return provider.UploadSession{}, fmt.Errorf("%w: no member can take %s", provider.ErrUnavailable, pth)
}

// PutFile is the one-request upload, offered when every member offers it
// (Capabilities().SinglePutMax). The uploader used to have no choice but the
// session protocol against a pool — begin, part, complete, three round
// trips for a 600-byte file where the member under it takes one — so a
// source tree copied onto two Drive accounts cost 3.4 s per file instead of
// 2.2 s. Placement and the bookkeeping are BeginUpload's: the same
// candidates in the same order, the same answers to a full, unreachable or
// refusing member, the same hold on the local bytes for repair, the same
// index commit. Only the member call differs.
func (p *Pool) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h provider.Hashes) (provider.Entry, error) {
	if err := validName(name); err != nil {
		return provider.Entry{}, err
	}
	parentPath, err := p.pathOf(ctx, parentID)
	if err != nil {
		return provider.Entry{}, err
	}
	pth := joinPath(parentPath, name)
	var lastUnreachable error
	for _, m := range p.candidates(ctx, pth) {
		sp, ok := m.p.(provider.SinglePutter)
		if !ok || m.p.Capabilities().SinglePutMax < size {
			// Capabilities() promised every member takes this size; a
			// member that changed its mind is skipped, not asked.
			continue
		}
		dirID, err := p.ensureDir(ctx, m, parentPath)
		if err != nil {
			if unreachable(err) {
				lastUnreachable = err
				continue
			}
			return provider.Entry{}, err
		}
		// The hold is taken before the bytes are sent: r is consumed by the
		// member and a retry on another candidate needs the reader intact,
		// which the caller's retry policy provides by seeking; the hold is a
		// hard link to the same blob and costs nothing to take early.
		hold, _ := p.takeHold(ctx, provider.UploadBlobLinkFrom(ctx), pth, size)
		me, err := sp.PutFile(ctx, dirID, name, r, size, h)
		if err != nil {
			p.dropHold(ctx, hold)
			if outOfSpace(err) {
				m.note(nil)
				m.markFull(p.now())
				// r has been read; the next candidate cannot be tried
				// with the same reader. The caller retries the whole put,
				// and placement then skips the member marked full.
				return provider.Entry{}, fmt.Errorf("%w: %w", provider.ErrRestartUpload, err)
			}
			if unreachable(err) {
				m.note(err)
				lastUnreachable = err
				return provider.Entry{}, fmt.Errorf("%w: %s on %s (%v)", provider.ErrUnavailable, pth, m.name, err)
			}
			if refusedName(err) {
				// BeginUpload moves on to the next candidate here; this
				// call cannot, r is spent. The denial is learned, so the
				// retry the queue makes places the file elsewhere — and
				// when no member will spell the name, the next attempt
				// finds no candidate and fails for the real reason.
				m.note(nil)
				p.learnDenial(ctx, m, name)
				return provider.Entry{}, fmt.Errorf("%w: %s refused the name: %w", provider.ErrTransient, m.name, err)
			}
			return provider.Entry{}, err
		}
		m.note(nil)
		e, err := p.finishUpload(ctx, m, pth, me, hold)
		if err != nil {
			return provider.Entry{}, err
		}
		p.awaitMinReplicas(ctx, pth)
		return e, nil
	}
	if lastUnreachable != nil {
		return provider.Entry{}, fmt.Errorf("%w: no member can take %s (%v)", provider.ErrUnavailable, pth, lastUnreachable)
	}
	if len(p.candidates(ctx, pth)) == 0 {
		if err := p.whyNoMember(ctx, pth); err != nil {
			return provider.Entry{}, err
		}
	}
	return provider.Entry{}, fmt.Errorf("%w: no member can take %s", provider.ErrUnavailable, pth)
}

// whyNoMember explains an empty candidate list when it is the name's
// fault, so the user sees "not supported" with a reason instead of "no
// reachable backend".
func (p *Pool) whyNoMember(ctx context.Context, pth string) error {
	parent, name := parentOf(pth), pth[strings.LastIndex(pth, "/")+1:]
	var first error
	inService := 0
	for _, m := range p.members {
		if st := m.state(); st == provider.HealthDraining || st == provider.HealthDisabled || !m.usable(p.probeInterval()) {
			continue
		}
		inService++
		if err := p.canHold(ctx, m, parent, name); err != nil && first == nil {
			first = err
		}
	}
	if inService > 0 && first != nil {
		return fmt.Errorf("%w (no member of pool %s can hold it)", first, p.name)
	}
	return nil
}

func (p *Pool) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	m, _, _, ms, err := p.unpackSession(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	pt, err := m.p.UploadPart(ctx, ms, idx, r, n)
	if outOfSpace(err) {
		m.note(nil)
		m.markFull(p.now())
		// The session is on a member that just ran out. It cannot be
		// resumed anywhere, so the caller must start the file again —
		// placement will pick a member with room.
		return provider.PartToken{}, fmt.Errorf("%w: %w", provider.ErrRestartUpload, err)
	}
	m.note(err)
	return pt, err
}

func (p *Pool) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	m, pth, hold, ms, err := p.unpackSession(s)
	if err != nil {
		return provider.Entry{}, err
	}
	me, err := m.p.CompleteUpload(ctx, ms, parts)
	if outOfSpace(err) {
		m.note(nil)
		m.markFull(p.now())
		return provider.Entry{}, fmt.Errorf("%w: %w", provider.ErrRestartUpload, err)
	}
	m.note(err)
	if err != nil {
		return provider.Entry{}, err
	}
	e, err := p.finishUpload(ctx, m, pth, me, hold)
	if err != nil {
		return provider.Entry{}, err
	}
	// Strict mode waits here, outside the index lock finishUpload takes:
	// the copies it waits for are made by repair, which needs that lock.
	p.awaitMinReplicas(ctx, pth)
	return e, nil
}

// finishUpload commits a landed upload to the index: the entry now carries
// the new content, the member that took it is its live replica, every other
// member's copy is stale until repair refreshes it, and the file is queued
// for replication.
func (p *Pool) finishUpload(ctx context.Context, m *member, pth string, me provider.Entry, hold string) (provider.Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	parent := parentOf(pth)
	name := path.Base(pth)
	ht, hv := bestHash(me.Hashes)
	ctoken := timeToken(me.Size, me.ModTime)
	if ht != "" {
		ctoken = hashToken(ht, hv)
	}
	now := p.now().UnixNano()
	var id, parentID string
	err := p.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = idForTx(tx, pth); err != nil {
			return err
		}
		if parentID, err = idForTx(tx, parent); err != nil {
			return err
		}
		if err := upsertEntry(tx, entryRow{path: pth, parent: parent, kind: provider.KindFile, size: me.Size, mtimeNS: me.ModTime.UnixNano(), ctoken: ctoken, hashType: string(ht), hash: hv}, now); err != nil {
			return err
		}
		if err := upsertReplica(tx, pth, parent, m.name, me, ctoken, "live", now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE replicas SET state = 'stale' WHERE path = ? AND member <> ? AND ctoken <> ?`, pth, m.name, ctoken); err != nil {
			return err
		}
		if hold != "" {
			if err := p.setHoldToken(tx, hold, ctoken); err != nil {
				return err
			}
		}
		if p.wantReplicas(pth) > 1 {
			if _, err := tx.Exec(`INSERT INTO repair_queue(path, reason, priority, next_at, attempts, source_hint, created_at) VALUES(?, 'under-replicated', 1, 0, 0, ?, ?)
				ON CONFLICT(path) DO UPDATE SET reason = excluded.reason, next_at = 0, attempts = 0, source_hint = excluded.source_hint`, pth, hold, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return provider.Entry{}, err
	}
	if p.wantReplicas(pth) <= 1 && hold != "" {
		p.releaseHolds(ctx, pth)
	}
	e := provider.Entry{ID: id, ParentID: parentID, Name: name, Kind: provider.KindFile, Size: me.Size, ModTime: me.ModTime, Version: ctoken}
	if ht != "" {
		e.Hashes = provider.Hashes{ht: hv}
	}
	return e, nil
}

func upsertReplica(tx *sql.Tx, pth, parent, member string, me provider.Entry, ctoken, state string, now int64) error {
	ht, hv := bestHash(me.Hashes)
	_, err := tx.Exec(`INSERT INTO replicas(path, parent, member, remote_id, version, size, mtime_ns, hash_type, hash, ctoken, state, seen_at, member_name)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, member) DO UPDATE SET remote_id = excluded.remote_id, version = excluded.version, size = excluded.size,
		mtime_ns = excluded.mtime_ns, hash_type = excluded.hash_type, hash = excluded.hash, ctoken = excluded.ctoken, state = excluded.state,
		seen_at = excluded.seen_at, member_name = excluded.member_name`,
		pth, parent, member, me.ID, me.Version, me.Size, me.ModTime.UnixNano(), string(ht), hv, ctoken, state, now, me.Name)
	if err != nil {
		return err
	}
	// The same physical file is indexed under exactly one path.
	_, err = tx.Exec(`DELETE FROM replicas WHERE member = ? AND parent = ? AND member_name = ? AND path <> ?`, member, parent, me.Name, pth)
	return err
}

// ensureDir resolves a directory on a member, creating it (and its parents)
// when the member does not have it yet. Directories are cheap; a member
// receives the skeleton of the tree lazily, as replicas land under it.
func (p *Pool) ensureDir(ctx context.Context, m *member, pth string) (string, error) {
	if pth == "/" {
		return p.ensureRoot(ctx, m)
	}
	id, err := p.dirID(ctx, m, pth)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, provider.ErrNotFound) {
		return "", err
	}
	parentID, err := p.ensureDir(ctx, m, parentOf(pth))
	if err != nil {
		return "", err
	}
	e, err := m.p.Mkdir(ctx, parentID, path.Base(pth))
	if err == nil {
		m.note(nil)
		p.rememberDirID(ctx, m, pth, e.ID)
		return e.ID, nil
	}
	if errors.Is(err, provider.ErrExists) {
		// Created out of band since the parent was enumerated; look again.
		return p.findDir(ctx, m, parentID, pth)
	}
	m.note(err)
	return "", err
}

// findDir enumerates a parent on the member and returns the id of the
// directory at pth, remembering every sibling directory on the way.
func (p *Pool) findDir(ctx context.Context, m *member, parentID, pth string) (string, error) {
	entries, err := m.listAll(ctx, parentID)
	if err != nil {
		return "", err
	}
	found := ""
	for _, e := range entries {
		if e.Kind != provider.KindDir {
			continue
		}
		p.rememberDirID(ctx, m, joinPath(parentOf(pth), e.Name), e.ID)
		if e.Name == path.Base(pth) {
			found = e.ID
		}
	}
	p.markEnumerated(ctx, m, parentOf(pth))
	if found == "" {
		return "", fmt.Errorf("%w: %s on member %s", provider.ErrNotFound, pth, m.name)
	}
	return found, nil
}

// ensureRoot resolves the member's configured root, creating the missing
// segments so a fresh drive can join without preparation.
func (p *Pool) ensureRoot(ctx context.Context, m *member) (string, error) {
	id, err := p.rootDirID(ctx, m)
	if err == nil || !errors.Is(err, provider.ErrNotFound) {
		return id, err
	}
	id = provider.RootOf(m.p)
	for _, seg := range strings.Split(strings.Trim(m.root, "/"), "/") {
		entries, err := m.listAll(ctx, id)
		if err != nil {
			return "", err
		}
		next := ""
		for _, e := range entries {
			if e.Name == seg && e.Kind == provider.KindDir {
				next = e.ID
				break
			}
		}
		if next == "" {
			e, err := m.p.Mkdir(ctx, id, seg)
			if err != nil {
				m.note(err)
				return "", err
			}
			next = e.ID
		}
		id = next
	}
	p.rememberDirID(ctx, m, "/", id)
	return id, nil
}

// fanout runs one tree operation on every listed member. It returns how
// many members took it; a member that could not be reached is given a
// pending op instead, and a member that refused it for a reason of its own
// is recorded as a divergence — the operation still stands on the others.
//
// The members are asked at once — do runs on one goroutine per member, so
// whatever it writes besides the member must be guarded. Asked in turn, a
// mkdir cost the sum of the
// members' round trips — 2.7 s on two Drive accounts behind a proxy — and
// mkdir is synchronous under the mount, so copying a source tree in spent
// most of its time here. The bookkeeping that follows each answer (health,
// pending ops, divergences) is done in declaration order once every answer
// is in, so the rows it writes land in the same order they always did.
type target struct {
	m        *member
	remoteID string
}

func (p *Pool) fanout(ctx context.Context, targets []target, op, pth string, args map[string]string, do func(t target) error) (int, error) {
	ok := 0
	var lastErr, lastUnreachable error
	now := p.now().UnixNano()
	probe := p.probeInterval()
	asked := make([]bool, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		if !t.m.usable(probe) {
			continue
		}
		asked[i] = true
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			errs[i] = do(t)
		}(i, t)
	}
	wg.Wait()
	for i, t := range targets {
		if !asked[i] {
			lastUnreachable = fmt.Errorf("member %s is %s", t.m.name, t.m.state())
			p.pendingOp(ctx, t.m, op, pth, args, now)
			continue
		}
		err := errs[i]
		switch {
		case err == nil, errors.Is(err, provider.ErrNotFound):
			t.m.note(nil)
			ok++
		case unreachable(err):
			t.m.note(err)
			lastUnreachable = err
			p.pendingOp(ctx, t.m, op, pth, args, now)
		default:
			// The member answered and refused: a name it will not take, a
			// quota, something already at the destination. Record it for a
			// person to see, and still queue the operation — the member is
			// as far behind as an unreachable one, and the replay path is
			// what eventually gives up and asks for a scrub instead of
			// leaving the old copy behind for a listing to mistake for a
			// new file.
			t.m.note(nil)
			lastErr = err
			_ = p.tx(ctx, func(tx *sql.Tx) error {
				recordDivergence(tx, pth, t.m.name, "refused-"+op, err.Error(), now)
				return nil
			})
			p.pendingOp(ctx, t.m, op, pth, args, now)
		}
	}
	if ok > 0 {
		return ok, nil
	}
	if lastErr != nil {
		return 0, lastErr
	}
	if lastUnreachable != nil {
		return 0, fmt.Errorf("%w: %s on %s (%v)", provider.ErrUnavailable, op, pth, lastUnreachable)
	}
	return 0, nil
}

func (p *Pool) pendingOp(ctx context.Context, m *member, op, pth string, args map[string]string, now int64) {
	b, _ := json.Marshal(args)
	_, _ = p.db.ExecContext(ctx, `INSERT INTO pending_ops(member, op, path, parent, args, created_at) VALUES(?, ?, ?, ?, ?, ?)`, m.name, op, pth, parentOf(pth), string(b), now)
}

// fileTargets lists the members holding a file, with their ids for it.
func (p *Pool) fileTargets(ctx context.Context, pth string) ([]target, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, remote_id FROM replicas WHERE path = ?`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []target
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		if m := p.byName[name]; m != nil {
			out = append(out, target{m, id})
		}
	}
	return out, rows.Err()
}

// dirTargets lists the members holding a directory, with their ids for it.
func (p *Pool) dirTargets(ctx context.Context, pth string) ([]target, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, remote_id FROM member_dirs WHERE path = ?`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []target
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		if m := p.byName[name]; m != nil {
			out = append(out, target{m, id})
		}
	}
	return out, rows.Err()
}

// entryTargets resolves an entry's members, listing its parent first when
// the index has not seen it.
func (p *Pool) entryTargets(ctx context.Context, pth string) (entryRow, []target, error) {
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return row, nil, err
	}
	if !ok {
		if _, err := p.listDir(ctx, parentOf(pth)); err != nil {
			return row, nil, err
		}
		if row, ok, err = p.entryAt(ctx, pth); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("%w: %s", provider.ErrNotFound, pth)
			}
			return row, nil, err
		}
	}
	var targets []target
	if row.kind == provider.KindDir {
		targets, err = p.dirTargets(ctx, pth)
	} else {
		targets, err = p.fileTargets(ctx, pth)
	}
	return row, targets, err
}

func (p *Pool) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if err := validName(name); err != nil {
		return provider.Entry{}, err
	}
	parentPath, err := p.pathOf(ctx, parentID)
	if err != nil {
		return provider.Entry{}, err
	}
	pth := joinPath(parentPath, name)
	if _, exists, err := p.entryAt(ctx, pth); err != nil {
		return provider.Entry{}, err
	} else if exists {
		return provider.Entry{}, fmt.Errorf("%w: %s", provider.ErrExists, pth)
	}
	// Every member that holds the parent gets the directory: the skeleton
	// of the tree is mirrored everywhere, only the files are placed.
	var targets []target
	var lastUnreachable error
	probe := p.probeInterval()
	dirEntryID, err := p.idFor(ctx, pth)
	if err != nil {
		return provider.Entry{}, err
	}
	mkdirArgs := map[string]string{"id": dirEntryID}
	for _, m := range p.members {
		if !m.usable(probe) {
			lastUnreachable = fmt.Errorf("member %s is %s", m.name, m.state())
			p.pendingOp(ctx, m, "mkdir", pth, mkdirArgs, p.now().UnixNano())
			continue
		}
		id, err := p.dirID(ctx, m, parentPath)
		if err != nil {
			if unreachable(err) {
				lastUnreachable = err
				p.pendingOp(ctx, m, "mkdir", pth, mkdirArgs, p.now().UnixNano())
			}
			continue
		}
		targets = append(targets, target{m, id})
	}
	if len(targets) == 0 && lastUnreachable != nil {
		return provider.Entry{}, fmt.Errorf("%w: mkdir %s (%v)", provider.ErrUnavailable, pth, lastUnreachable)
	}
	if len(targets) == 0 {
		return provider.Entry{}, fmt.Errorf("%w: parent %s", provider.ErrNotFound, parentPath)
	}
	ok, err := p.fanout(ctx, targets, "mkdir", pth, mkdirArgs, func(t target) error {
		e, err := t.m.p.Mkdir(ctx, t.remoteID, name)
		if errors.Is(err, provider.ErrExists) {
			id, ferr := p.findDir(ctx, t.m, t.remoteID, pth)
			if ferr != nil {
				return err
			}
			p.rememberDirID(ctx, t.m, pth, id)
			return nil
		}
		if err != nil {
			return err
		}
		p.rememberDirID(ctx, t.m, pth, e.ID)
		return nil
	})
	if err != nil {
		return provider.Entry{}, err
	}
	if ok == 0 {
		return provider.Entry{}, fmt.Errorf("%w: mkdir %s", provider.ErrUnavailable, pth)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var id, pid string
	err = p.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = idForTx(tx, pth); err != nil {
			return err
		}
		if pid, err = idForTx(tx, parentPath); err != nil {
			return err
		}
		return upsertEntry(tx, entryRow{path: pth, parent: parentPath, kind: provider.KindDir, mtimeNS: p.now().UnixNano(), ctoken: dirToken(id)}, p.now().UnixNano())
	})
	if err != nil {
		return provider.Entry{}, err
	}
	return provider.Entry{ID: id, ParentID: pid, Name: name, Kind: provider.KindDir, ModTime: p.now(), Version: dirToken(id)}, nil
}

func (p *Pool) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if err := validName(newName); err != nil {
		return provider.Entry{}, err
	}
	pth, err := p.pathOf(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if pth == "/" {
		return provider.Entry{}, provider.ErrUnsupported
	}
	newPath := joinPath(parentOf(pth), newName)
	row, targets, err := p.entryTargets(ctx, pth)
	if err != nil {
		return provider.Entry{}, err
	}
	args := map[string]string{"id": id, "from": pth, "to": newPath, "ctoken": row.ctoken}
	// fanout asks the members at once; the answers land in one map.
	var answers sync.Mutex
	renamed := map[string]provider.Entry{}
	ok, err := p.fanout(ctx, targets, "rename", pth, args, func(t target) error {
		e, err := t.m.p.Rename(ctx, t.remoteID, newName)
		if err == nil {
			answers.Lock()
			renamed[t.m.name] = e
			answers.Unlock()
		}
		return err
	})
	if err != nil {
		return provider.Entry{}, err
	}
	if ok == 0 && len(targets) > 0 {
		return provider.Entry{}, fmt.Errorf("%w: rename %s", provider.ErrUnavailable, pth)
	}
	return p.relocate(ctx, row, pth, newPath, renamed)
}

func (p *Pool) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	pth, err := p.pathOf(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if pth == "/" {
		return provider.Entry{}, provider.ErrUnsupported
	}
	newParent, err := p.pathOf(ctx, newParentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if newParent == pth || strings.HasPrefix(newParent, pth+"/") {
		return provider.Entry{}, fmt.Errorf("%w: cannot move %s into itself", provider.ErrUnsupported, pth)
	}
	newPath := joinPath(newParent, path.Base(pth))
	row, targets, err := p.entryTargets(ctx, pth)
	if err != nil {
		return provider.Entry{}, err
	}
	args := map[string]string{"id": id, "from": pth, "to": newPath, "ctoken": row.ctoken}
	var answers sync.Mutex
	moved := map[string]provider.Entry{}
	ok, err := p.fanout(ctx, targets, "move", pth, args, func(t target) error {
		dst, err := p.ensureDir(ctx, t.m, newParent)
		if err != nil {
			return err
		}
		e, err := t.m.p.Move(ctx, t.remoteID, dst)
		if err == nil {
			answers.Lock()
			moved[t.m.name] = e
			answers.Unlock()
		}
		return err
	})
	if err != nil {
		return provider.Entry{}, err
	}
	if ok == 0 && len(targets) > 0 {
		return provider.Entry{}, fmt.Errorf("%w: move %s", provider.ErrUnavailable, pth)
	}
	return p.relocate(ctx, row, pth, newPath, moved)
}

// relocate rewrites the index after a rename or move: the entry and its
// subtree take the new path, ids stay, and a member that has not applied
// the change yet keeps a pending replica at the new path with the old
// physical name so a later listing recognises the file.
func (p *Pool) relocate(ctx context.Context, row entryRow, oldPath, newPath string, applied map[string]provider.Entry) (provider.Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UnixNano()
	var id, pid string
	err := p.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = idForTx(tx, oldPath); err != nil {
			return err
		}
		if pid, err = idForTx(tx, parentOf(newPath)); err != nil {
			return err
		}
		// A stale index entry at the destination is replaced.
		if err := dropSubtree(tx, newPath); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM ids WHERE path = ? OR path LIKE ? ESCAPE '\'`, newPath, likePrefix(newPath)); err != nil {
			return err
		}
		if err := movePaths(tx, oldPath, newPath); err != nil {
			return err
		}
		// A member that did not apply the operation still holds the file
		// where it was. Its row travelled to the destination with the
		// entry, so the row is the only place that can say so. Names
		// cannot: a move across directories keeps the name, and reading
		// "the name still matches" as "this member is up to date" is what
		// let the copy sit there unnoticed.
		for _, m := range p.members {
			if _, did := applied[m.name]; did {
				continue
			}
			if _, err := tx.Exec(`UPDATE replicas SET state = 'pending' WHERE member = ? AND (path = ? OR path LIKE ? ESCAPE '\') AND state = 'live'
				AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.path = replicas.path AND e.conflict_of <> '')`, m.name, newPath, likePrefix(newPath)); err != nil {
				return err
			}
		}
		for name, e := range applied {
			if row.kind == provider.KindDir {
				if _, err := tx.Exec(`UPDATE member_dirs SET remote_id = ?, verified_at = ? WHERE member = ? AND path = ?`, e.ID, now, name, newPath); err != nil {
					return err
				}
				if m := p.byName[name]; m != nil {
					m.mu.Lock()
					m.dirIDs = map[string]string{}
					m.mu.Unlock()
				}
			} else {
				if _, err := tx.Exec(`UPDATE replicas SET remote_id = ?, member_name = ?, seen_at = ? WHERE member = ? AND path = ?`, e.ID, e.Name, now, name, newPath); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return provider.Entry{}, err
	}
	// Members that have not applied it keep the old physical name under a
	// pending replica; the op log brings them into line.
	if row.kind == provider.KindDir {
		for _, m := range p.members {
			m.mu.Lock()
			for k := range m.dirIDs {
				if k == oldPath || strings.HasPrefix(k, oldPath+"/") {
					delete(m.dirIDs, k)
				}
			}
			m.mu.Unlock()
		}
	}
	e := provider.Entry{ID: id, ParentID: pid, Name: path.Base(newPath), Kind: row.kind, Size: row.size, Version: row.ctoken}
	if row.kind == provider.KindDir {
		e.Version = dirToken(id)
	} else if row.hashType != "" {
		e.Hashes = provider.Hashes{provider.HashType(row.hashType): row.hash}
	}
	return e, nil
}

func likePrefix(pth string) string {
	return strings.ReplaceAll(strings.ReplaceAll(pth, "%", "\\%"), "_", "\\_") + "/%"
}

// movePaths rewrites every index row under oldPath to newPath.
func movePaths(tx *sql.Tx, oldPath, newPath string) error {
	like := likePrefix(oldPath)
	n := len(oldPath)
	for _, table := range []string{"ids", "entries", "replicas", "member_dirs", "holds", "divergences", "pending_ops", "repair_queue", "rebalance_queue"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET path = ? || substr(path, ?) WHERE path = ? OR path LIKE ? ESCAPE '\'`, newPath, n+1, oldPath, like); err != nil {
			return err
		}
	}
	for _, table := range []string{"entries", "replicas", "member_dirs", "pending_ops"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET parent = ? WHERE path = ?`, parentOf(newPath), newPath); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+table+` SET parent = ? || substr(parent, ?) WHERE path LIKE ? ESCAPE '\'`, newPath, n+1, likePrefix(newPath)); err != nil {
			return err
		}
	}
	// A replica whose physical name no longer matches its entry is pending
	// on that member (a conflict copy is the deliberate exception).
	if _, err := tx.Exec(`UPDATE replicas SET state = 'pending' WHERE (path = ? OR path LIKE ? ESCAPE '\') AND state = 'live'
		AND member_name <> substr(path, length(parent) + 1 + (parent <> '/'))
		AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.path = replicas.path AND e.conflict_of <> '')`, newPath, likePrefix(newPath)); err != nil {
		return err
	}
	return nil
}

func (p *Pool) Delete(ctx context.Context, id string) error {
	pth, err := p.pathOf(ctx, id)
	if err != nil {
		return err
	}
	if pth == "/" {
		return provider.ErrUnsupported
	}
	row, targets, err := p.entryTargets(ctx, pth)
	if err != nil {
		return err
	}
	ok, err := p.fanout(ctx, targets, "delete", pth, map[string]string{"ctoken": row.ctoken}, func(t target) error {
		return t.m.p.Delete(ctx, t.remoteID)
	})
	if err != nil {
		return err
	}
	if ok == 0 && len(targets) > 0 {
		return fmt.Errorf("%w: delete %s", provider.ErrUnavailable, pth)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseHolds(ctx, pth)
	for _, m := range p.members {
		m.mu.Lock()
		for k := range m.dirIDs {
			if k == pth || strings.HasPrefix(k, pth+"/") {
				delete(m.dirIDs, k)
			}
		}
		m.mu.Unlock()
	}
	return p.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM repair_queue WHERE path = ? OR path LIKE ? ESCAPE '\'`, pth, likePrefix(pth)); err != nil {
			return err
		}
		return dropSubtree(tx, pth)
	})
}

// awaitMinReplicas is what write_mode: strict costs a close(): the index
// is committed and the index lock released and the bytes are already durable on one member
// and in the local hold, and this waits up to min_replicas_timeout for
// the other copies to exist too.
//
// It never fails the write. A timeout leaves the file in the repair
// queue, exactly where relaxed mode leaves it; waiting is a promise about
// when close() returns, not about whether the data is safe. Re-running
// CompleteUpload against a member is not safe to retry, which is why
// strict mode repairs after the commit rather than blocking the upload
// itself.
func (p *Pool) awaitMinReplicas(ctx context.Context, pth string) {
	if p.writeMode() != config.WriteModeStrict {
		return
	}
	min := p.minReplicas(pth)
	if min <= 1 {
		return
	}
	wait, cancel := context.WithTimeout(ctx, p.minReplicasDeadline())
	defer cancel()
	for wait.Err() == nil {
		row, ok, err := p.entryAt(wait, pth)
		if err != nil || !ok {
			return
		}
		live, err := p.liveReplicas(wait, pth, row.ctoken)
		if err != nil {
			return
		}
		if len(live) >= min {
			return
		}
		if target, capped := p.targetFor(pth); capped && len(live) >= target {
			// No member left that could take another copy: waiting for
			// one cannot end any better than this.
			return
		}
		// One repair pass fills the whole target, not just the
		// threshold; stopping it at min_replicas would only make the
		// same copies happen later.
		if _, err := p.repairPath(wait, pth); err != nil && wait.Err() != nil {
			return
		}
	}
}
