package pool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
)

// hiddenPrefix marks the pool's own bookkeeping files on a member; they are
// never part of the namespace.
const hiddenPrefix = ".cloudfs-pool"

func hidden(name string) bool { return strings.HasPrefix(name, hiddenPrefix) }

// obs is one observation of a name in a directory: what one member reports
// there, or what the index last saw when the member cannot be asked.
type obs struct {
	m        *member
	e        provider.Entry
	snapshot bool
	// ctoken is the token the index already assigned to this replica when
	// the observation matches what it recorded; "" when it must be derived.
	ctoken string
}

type entryRow struct {
	path, parent string
	kind         provider.Kind
	size         int64
	mtimeNS      int64
	ctoken       string
	hashType     string
	hash         string
	conflictOf   string
}

type replicaRow struct {
	path, parent, member, remoteID, version string
	size, mtimeNS                           int64
	hashType, hash, ctoken, state           string
	memberName                              string
}

// List returns the whole merged directory in one page. ListStream is the
// preferred path; the VFS uses it because Caps.StreamList is set.
func (p *Pool) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if cursor != "" {
		return nil, "", fmt.Errorf("pool: unknown cursor %q", cursor)
	}
	pth, err := p.pathOf(ctx, dirID)
	if err != nil {
		return nil, "", err
	}
	entries, err := p.listDir(ctx, pth)
	return entries, "", err
}

// ListStream implements provider.StreamLister.
func (p *Pool) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	pth, err := p.pathOf(ctx, dirID)
	if err != nil {
		return err
	}
	entries, err := p.listDir(ctx, pth)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := visit(e); err != nil {
			return err
		}
	}
	return nil
}

// Stat implements provider.Provider. Directories and files come from the
// index; a path the index has never seen is looked for by listing its parent.
func (p *Pool) Stat(ctx context.Context, id string) (provider.Entry, error) {
	pth, err := p.pathOf(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if pth == "/" {
		return provider.Entry{ID: rootID, Name: "", Kind: provider.KindDir, Version: dirToken(rootID), ModTime: p.now()}, nil
	}
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return provider.Entry{}, err
	}
	if !ok {
		if _, err := p.listDir(ctx, parentOf(pth)); err != nil {
			return provider.Entry{}, err
		}
		row, ok, err = p.entryAt(ctx, pth)
		if err != nil {
			return provider.Entry{}, err
		}
		if !ok {
			return provider.Entry{}, fmt.Errorf("%w: %s", provider.ErrNotFound, pth)
		}
	}
	return p.entryFromRow(ctx, row)
}

func (p *Pool) entryAt(ctx context.Context, pth string) (entryRow, bool, error) {
	var r entryRow
	var kind int
	err := p.db.QueryRowContext(ctx, `SELECT path, parent, kind, size, mtime_ns, ctoken, hash_type, hash, conflict_of FROM entries WHERE path = ?`, pth).
		Scan(&r.path, &r.parent, &kind, &r.size, &r.mtimeNS, &r.ctoken, &r.hashType, &r.hash, &r.conflictOf)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("pool: %w", err)
	}
	r.kind = provider.Kind(kind)
	return r, true, nil
}

func (p *Pool) entryFromRow(ctx context.Context, r entryRow) (provider.Entry, error) {
	id, err := p.idFor(ctx, r.path)
	if err != nil {
		return provider.Entry{}, err
	}
	parentID, err := p.idFor(ctx, r.parent)
	if err != nil {
		return provider.Entry{}, err
	}
	e := provider.Entry{ID: id, ParentID: parentID, Name: path.Base(r.path), Kind: r.kind, Size: r.size, ModTime: time.Unix(0, r.mtimeNS), Version: r.ctoken}
	if r.kind == provider.KindDir {
		e.Version = dirToken(id)
		e.Size = 0
	} else if r.hashType != "" {
		e.Hashes = provider.Hashes{provider.HashType(r.hashType): r.hash}
	}
	return e, nil
}

// memberListing is what one member said about a directory.
type memberListing struct {
	m       *member
	entries []provider.Entry
	dirID   string
	holds   bool  // the member has this directory
	err     error // the member could not be asked
}

// listDir merges one directory across the members and refreshes the index
// for it. It never writes to a member: a divergence is recorded and shown,
// and only the repair worker ever acts on one.
func (p *Pool) listDir(ctx context.Context, pth string) ([]provider.Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Ask every member in parallel. A member that does not hold the
	// directory contributes nothing; one that cannot be reached contributes
	// what the index last saw of it, so a drive being down never empties a
	// directory that other drives still serve.
	results := make([]memberListing, len(p.members))
	var wg sync.WaitGroup
	probe := p.probeInterval()
	for i, m := range p.members {
		if !m.usable(probe) {
			// Known to be down: do not wait on it again; what the index
			// last saw of it stands in until a probe brings it back.
			results[i] = memberListing{m: m, err: fmt.Errorf("%w: member %s is %s", provider.ErrUnavailable, m.name, m.state())}
			continue
		}
		wg.Add(1)
		go func(i int, m *member) {
			defer wg.Done()
			res := memberListing{m: m}
			id, err := p.dirID(ctx, m, pth)
			if err != nil {
				if !errors.Is(err, provider.ErrNotFound) {
					res.err = err
				}
				results[i] = res
				return
			}
			res.dirID = id
			entries, err := m.listAll(ctx, id)
			if err != nil {
				if errors.Is(err, provider.ErrNotFound) {
					p.forgetDirID(ctx, m, pth)
				} else {
					res.err = err
					res.holds = true
				}
				results[i] = res
				return
			}
			res.holds = true
			res.entries = entries
			results[i] = res
		}(i, m)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	answered, unreachable := 0, 0
	for _, r := range results {
		switch {
		case r.err != nil:
			unreachable++
		case r.holds:
			answered++
		}
	}
	if answered == 0 {
		if unreachable == 0 {
			// Every member answered, and none has the directory.
			if pth == "/" {
				return nil, nil
			}
			if err := p.tx(ctx, func(tx *sql.Tx) error { return dropSubtree(tx, pth) }); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %s", provider.ErrNotFound, pth)
		}
		// No member that might hold it could be asked. The index may still
		// describe the directory, and the VFS prefers a stale listing to an
		// error — but only if it has one; a directory never enumerated is
		// genuinely unavailable, not empty.
		if known, err := p.indexKnows(ctx, pth); err != nil {
			return nil, err
		} else if !known {
			return nil, fmt.Errorf("%w: listing %s", provider.ErrUnavailable, pth)
		}
	}

	existingReplicas, err := p.replicasUnder(ctx, pth)
	if err != nil {
		return nil, err
	}
	existingDirs, err := p.memberDirsUnder(ctx, pth)
	if err != nil {
		return nil, err
	}
	pendingUnder, err := p.pendingOpsUnder(ctx, pth)
	if err != nil {
		return nil, err
	}
	moves, err := p.pendingMoves(ctx)
	if err != nil {
		return nil, err
	}

	// Observations by name.
	byName := map[string][]obs{}
	keepRows := map[replicaKey]bool{}
	// A member that owes the pool a tree operation here has not shown the
	// file yet, and its absence from the member's listing is expected: the
	// row is what the op will act on, so the reconciliation must not take
	// it away.
	for k := range pendingUnder {
		keepRows[k] = true
	}
	for _, r := range results {
		switch {
		case r.err != nil:
			for _, row := range existingReplicas {
				if row.member != r.m.name {
					continue
				}
				if row.state == "stale" {
					keepRows[replicaKey{row.path, r.m.name}] = true
					continue
				}
				e := provider.Entry{ID: row.remoteID, Name: row.memberName, Kind: provider.KindFile, Size: row.size, ModTime: time.Unix(0, row.mtimeNS), Version: row.version}
				if row.hashType != "" {
					e.Hashes = provider.Hashes{provider.HashType(row.hashType): row.hash}
				}
				byName[path.Base(row.path)] = append(byName[path.Base(row.path)], obs{m: r.m, e: e, snapshot: true, ctoken: row.ctoken})
			}
			for dirPath, id := range existingDirs[r.m.name] {
				name := path.Base(dirPath)
				byName[name] = append(byName[name], obs{m: r.m, e: provider.Entry{ID: id, Name: name, Kind: provider.KindDir}, snapshot: true})
			}
		case r.holds:
			for _, e := range r.entries {
				if hidden(e.Name) || e.Name == "" || strings.ContainsAny(e.Name, "/\x00") {
					continue
				}
				physical := joinPath(pth, e.Name)
				row, known := existingReplicas[replicaKey{physical, r.m.name}]
				if !known {
					row, known = p.physicalRowFor(existingReplicas, pth, e.Name, r.m.name)
				}
				if dst, owed := moves[replicaKey{physical, r.m.name}]; owed {
					// This member owes the pool a move of exactly this
					// file, out of this directory. What it shows here is
					// the old location, not a new file.
					keepRows[replicaKey{dst, r.m.name}] = true
					continue
				}
				if pendingUnder[replicaKey{physical, r.m.name}] {
					// A tree operation on this member is still to be
					// replayed; what it shows here is the past, not a
					// new observation. Its index row stays as it is.
					if known {
						keepRows[replicaKey{row.path, r.m.name}] = true
					}
					continue
				}
				ob := obs{m: r.m, e: e}
				name := e.Name
				if known && row.version == e.Version && row.size == e.Size && row.mtimeNS == e.ModTime.UnixNano() {
					ob.ctoken = row.ctoken
					switch row.state {
					case "stale":
						// Overwritten through the pool since; repair
						// refreshes it. Not a conflict, not a replica.
						keepRows[replicaKey{row.path, r.m.name}] = true
						continue
					case "pending":
						// Renamed or moved through the pool, not yet on
						// this member: it belongs to the entry at the
						// new path.
						name = path.Base(row.path)
					}
				}
				byName[name] = append(byName[name], ob)
			}
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	dirID, err := p.idFor(ctx, pth)
	if err != nil {
		return nil, err
	}
	now := p.now().UnixNano()
	var out []provider.Entry
	produced := map[string]bool{}
	seenReplica := keepRows
	seenDir := map[replicaKey]bool{}

	err = p.tx(ctx, func(tx *sql.Tx) error {
		for _, name := range names {
			obsList := byName[name]
			sort.SliceStable(obsList, func(i, j int) bool { return obsList[i].m.order < obsList[j].m.order })
			childPath := joinPath(pth, name)

			isDir := false
			for _, ob := range obsList {
				if ob.e.Kind == provider.KindDir {
					isDir = true
					break
				}
			}
			if isDir {
				var mtime time.Time
				for _, ob := range obsList {
					if ob.e.Kind != provider.KindDir {
						recordDivergence(tx, childPath, ob.m.name, "file-vs-dir", "a file where other members have a directory", now)
						continue
					}
					if ob.e.ModTime.After(mtime) {
						mtime = ob.e.ModTime
					}
					if !ob.snapshot {
						p.rememberDirIDTx(tx, ob.m, childPath, ob.e.ID, now)
					}
					seenDir[replicaKey{childPath, ob.m.name}] = true
				}
				id, err := idForTx(tx, childPath)
				if err != nil {
					return err
				}
				if err := upsertEntry(tx, entryRow{path: childPath, parent: pth, kind: provider.KindDir, mtimeNS: mtime.UnixNano(), ctoken: dirToken(id)}, now); err != nil {
					return err
				}
				produced[childPath] = true
				out = append(out, provider.Entry{ID: id, ParentID: dirID, Name: name, Kind: provider.KindDir, ModTime: mtime, Version: dirToken(id)})
				continue
			}

			groups := groupByContent(obsList)
			winner := groups[0]
			if len(groups) > 1 && absDuration(winner.newest().e.ModTime.Sub(groups[1].newest().e.ModTime)) <= mtimeTolerance {
				for _, ob := range groups[1].obs {
					recordDivergence(tx, childPath, ob.m.name, "ambiguous", "two contents with indistinguishable mtimes; the first-declared member wins", now)
				}
			}
			id, err := idForTx(tx, childPath)
			if err != nil {
				return err
			}
			e, err := p.publishGroup(tx, winner, childPath, pth, id, dirID, name, "", now, seenReplica)
			if err != nil {
				return err
			}
			produced[childPath] = true
			out = append(out, e)
			for _, g := range groups[1:] {
				cname := conflictName(name, g.newest().e.ModTime, g.obs[0].m.name)
				cpath := joinPath(pth, cname)
				cid, err := idForTx(tx, cpath)
				if err != nil {
					return err
				}
				ce, err := p.publishGroup(tx, g, cpath, pth, cid, dirID, cname, childPath, now, seenReplica)
				if err != nil {
					return err
				}
				for _, ob := range g.obs {
					recordDivergence(tx, childPath, ob.m.name, "conflict", "older content than the winning replica; shown as "+cname, now)
				}
				produced[cpath] = true
				out = append(out, ce)
			}
		}

		// Forget what a reachable member no longer has, and remember that
		// its children were enumerated.
		for _, r := range results {
			if r.err != nil {
				continue
			}
			if r.holds {
				if _, err := tx.Exec(`UPDATE member_dirs SET listed_at = ? WHERE member = ? AND path = ?`, now, r.m.name, pth); err != nil {
					return err
				}
			}
			// A file this member lost that reappears under another name
			// with the same hash was renamed in the vendor's app. The pool
			// does not guess which side to follow; it says so.
			byHash := map[string]string{}
			for _, e := range r.entries {
				if e.Kind != provider.KindFile {
					continue
				}
				if _, known := existingReplicas[replicaKey{joinPath(pth, e.Name), r.m.name}]; known {
					continue
				}
				if ht, hv := bestHash(e.Hashes); ht != "" {
					byHash[string(ht)+":"+hv] = e.Name
				}
			}
			for key, row := range existingReplicas {
				if key.member == r.m.name && !seenReplica[key] {
					if _, err := tx.Exec(`DELETE FROM replicas WHERE path = ? AND member = ?`, key.path, key.member); err != nil {
						return err
					}
					if newName, ok := byHash[row.hashType+":"+row.hash]; ok && row.hash != "" {
						recordDivergence(tx, key.path, r.m.name, "renamed-out-of-band", "the member now has the same content as "+newName+"; rename it through the pool or keep both", now)
					}
				}
			}
			for dirPath := range existingDirs[r.m.name] {
				if !seenDir[replicaKey{dirPath, r.m.name}] {
					p.forgetDirIDTx(tx, r.m, dirPath)
				}
			}
		}
		// Drop index entries nothing produced any more, with their subtrees.
		rows, err := tx.Query(`SELECT path, kind FROM entries WHERE parent = ?`, pth)
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var ep string
			var kind int
			if err := rows.Scan(&ep, &kind); err != nil {
				rows.Close()
				return err
			}
			if !produced[ep] {
				gone = append(gone, ep)
			}
		}
		rows.Close()
		for _, ep := range gone {
			if err := dropSubtree(tx, ep); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type replicaKey struct{ path, member string }

// physicalRowFor finds the index row of a member's replica whose real name
// on the member is name but which the pool indexes under another path: a
// conflict copy, or a rename that member has not applied yet.
// pendingMoves maps a member's not-yet-applied rename or move by the path
// the file still occupies on that member. The index row travelled to the
// destination with the entry, out of the source directory's parent-scoped
// view, so without this a listing of the source would see the member's
// leftover file, find nothing that claims it, and publish the same content
// a second time under a new id. An out-of-band move by the vendor's own app
// has no such op and stays a new observation, as documented.
func (p *Pool) pendingMoves(ctx context.Context) (map[replicaKey]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, args FROM pending_ops WHERE op IN ('rename', 'move') AND state = 'pending'`)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	out := map[replicaKey]string{}
	for rows.Next() {
		var member, raw string
		if err := rows.Scan(&member, &raw); err != nil {
			return nil, err
		}
		var args map[string]string
		if json.Unmarshal([]byte(raw), &args) != nil {
			continue
		}
		if args["from"] == "" || args["to"] == "" {
			continue
		}
		out[replicaKey{args["from"], member}] = args["to"]
	}
	return out, rows.Err()
}

func (p *Pool) physicalRowFor(rows map[replicaKey]replicaRow, pth, name, member string) (replicaRow, bool) {
	for k, r := range rows {
		if k.member == member && r.memberName == name && r.parent == pth && path.Base(k.path) != name {
			return r, true
		}
	}
	return replicaRow{}, false
}

// group is one distinct content seen under a name.
type group struct {
	obs   []obs
	token string
}

func (g *group) newest() obs {
	best := g.obs[0]
	for _, ob := range g.obs[1:] {
		if ob.e.ModTime.After(best.e.ModTime) {
			best = ob
		}
	}
	return best
}

func (g *group) entries() []provider.Entry {
	out := make([]provider.Entry, len(g.obs))
	for i, ob := range g.obs {
		out[i] = ob.e
	}
	return out
}

// groupByContent partitions observations into contents and orders the
// groups newest first (ties by the first-declared member), so groups[0] is
// the content the pool publishes under the name.
func groupByContent(obsList []obs) []*group {
	var groups []*group
	for _, ob := range obsList {
		placed := false
		for _, g := range groups {
			if (ob.ctoken != "" && g.token != "" && ob.ctoken == g.token) || sameContent(ob.e, g.obs[0].e) {
				g.obs = append(g.obs, ob)
				if g.token == "" {
					g.token = ob.ctoken
				}
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, &group{obs: []obs{ob}, token: ob.ctoken})
		}
	}
	for _, g := range groups {
		if g.token == "" {
			g.token = tokenFor(g.entries(), g.newest().e)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		ni, nj := groups[i].newest(), groups[j].newest()
		if !ni.e.ModTime.Equal(nj.e.ModTime) {
			return ni.e.ModTime.After(nj.e.ModTime)
		}
		return groups[i].obs[0].m.order < groups[j].obs[0].m.order
	})
	return groups
}

// publishGroup writes one content group to the index as the entry at
// entryPath and returns what the pool reports for it.
func (p *Pool) publishGroup(tx *sql.Tx, g *group, entryPath, parent, id, parentID, name, conflictOf string, now int64, seen map[replicaKey]bool) (provider.Entry, error) {
	auth := g.newest()
	hashes := provider.Hashes{}
	for _, ht := range preferredHashes {
		for _, ob := range g.obs {
			if v := ob.e.Hashes[ht]; v != "" {
				hashes[ht] = v
				break
			}
		}
	}
	ht, hv := bestHash(hashes)
	row := entryRow{path: entryPath, parent: parent, kind: provider.KindFile, size: auth.e.Size, mtimeNS: auth.e.ModTime.UnixNano(), ctoken: g.token, hashType: string(ht), hash: hv, conflictOf: conflictOf}
	if err := upsertEntry(tx, row, now); err != nil {
		return provider.Entry{}, err
	}
	for _, ob := range g.obs {
		key := replicaKey{entryPath, ob.m.name}
		seen[key] = true
		if ob.snapshot {
			continue
		}
		state := "live"
		if conflictOf == "" && ob.e.Name != name {
			state = "pending" // the member still shows the old name
		}
		if err := upsertReplica(tx, entryPath, parent, ob.m.name, ob.e, g.token, state, now); err != nil {
			return provider.Entry{}, err
		}
	}
	e := provider.Entry{ID: id, ParentID: parentID, Name: name, Kind: provider.KindFile, Size: auth.e.Size, ModTime: auth.e.ModTime, Version: g.token}
	if len(hashes) > 0 {
		e.Hashes = hashes
	}
	return e, nil
}

// conflictName renders "notes.md" as "notes (conflict 2026-09-06 nas).md".
// The date is the losing content's own mtime, so the name is stable from
// one listing to the next.
func conflictName(name string, mtime time.Time, member string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (conflict %s %s)%s", base, mtime.UTC().Format("2006-01-02"), member, ext)
}

func upsertEntry(tx *sql.Tx, r entryRow, now int64) error {
	_, err := tx.Exec(`INSERT INTO entries(path, parent, kind, size, mtime_ns, ctoken, hash_type, hash, conflict_of, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET parent = excluded.parent, kind = excluded.kind, size = excluded.size, mtime_ns = excluded.mtime_ns,
		ctoken = excluded.ctoken, hash_type = excluded.hash_type, hash = excluded.hash, conflict_of = excluded.conflict_of, updated_at = excluded.updated_at`,
		r.path, r.parent, int(r.kind), r.size, r.mtimeNS, r.ctoken, r.hashType, r.hash, r.conflictOf, now)
	return err
}

func recordDivergence(tx *sql.Tx, pth, member, kind, detail string, now int64) {
	_, _ = tx.Exec(`INSERT INTO divergences(path, member, kind, detail, seen_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(path, member, kind) DO UPDATE SET detail = excluded.detail, seen_at = excluded.seen_at`, pth, member, kind, detail, now)
}

// dropSubtree removes an index entry and everything under it.
func dropSubtree(tx *sql.Tx, pth string) error {
	like := strings.ReplaceAll(strings.ReplaceAll(pth, "%", "\\%"), "_", "\\_") + "/%"
	for _, q := range []string{
		`DELETE FROM entries WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		`DELETE FROM replicas WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		`DELETE FROM member_dirs WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		`DELETE FROM divergences WHERE path = ? OR path LIKE ? ESCAPE '\'`,
	} {
		if _, err := tx.Exec(q, pth, like); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) replicasUnder(ctx context.Context, pth string) (map[replicaKey]replicaRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT path, parent, member, remote_id, version, size, mtime_ns, hash_type, hash, ctoken, state, member_name FROM replicas WHERE parent = ?`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	out := map[replicaKey]replicaRow{}
	for rows.Next() {
		var r replicaRow
		if err := rows.Scan(&r.path, &r.parent, &r.member, &r.remoteID, &r.version, &r.size, &r.mtimeNS, &r.hashType, &r.hash, &r.ctoken, &r.state, &r.memberName); err != nil {
			return nil, fmt.Errorf("pool: %w", err)
		}
		out[replicaKey{r.path, r.member}] = r
	}
	return out, rows.Err()
}

// memberDirsUnder returns member → (dir path → id) for the children of pth.
func (p *Pool) memberDirsUnder(ctx context.Context, pth string) (map[string]map[string]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, path, remote_id FROM member_dirs WHERE parent = ?`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var m, dp, id string
		if err := rows.Scan(&m, &dp, &id); err != nil {
			return nil, fmt.Errorf("pool: %w", err)
		}
		if out[m] == nil {
			out[m] = map[string]string{}
		}
		out[m][dp] = id
	}
	return out, rows.Err()
}

// indexKnows reports whether the directory's children were ever enumerated
// on some member, i.e. whether the index holds a snapshot worth serving.
func (p *Pool) indexKnows(ctx context.Context, pth string) (bool, error) {
	var n int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_dirs WHERE path = ? AND listed_at > 0`, pth).Scan(&n); err != nil {
		return false, fmt.Errorf("pool: %w", err)
	}
	return n > 0, nil
}

func (p *Pool) rememberDirIDTx(tx *sql.Tx, m *member, pth, id string, now int64) {
	m.mu.Lock()
	m.dirIDs[pth] = id
	m.mu.Unlock()
	_, _ = tx.Exec(`INSERT INTO member_dirs(member, path, parent, remote_id, verified_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(member, path) DO UPDATE SET remote_id = excluded.remote_id, verified_at = excluded.verified_at`, m.name, pth, dirParent(pth), id, now)
}

func (p *Pool) forgetDirIDTx(tx *sql.Tx, m *member, pth string) {
	m.mu.Lock()
	for k := range m.dirIDs {
		if k == pth || strings.HasPrefix(k, pth+"/") {
			delete(m.dirIDs, k)
		}
	}
	m.mu.Unlock()
	like := strings.ReplaceAll(strings.ReplaceAll(pth, "%", "\\%"), "_", "\\_") + "/%"
	_, _ = tx.Exec(`DELETE FROM member_dirs WHERE member = ? AND (path = ? OR path LIKE ? ESCAPE '\')`, m.name, pth, like)
}

// pendingOpsUnder returns the (physical path, member) pairs under pth that
// have a tree operation waiting to be replayed on that member.
func (p *Pool) pendingOpsUnder(ctx context.Context, pth string) (map[replicaKey]bool, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT member, path, args FROM pending_ops WHERE parent = ? AND state = 'pending'`, pth)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	out := map[replicaKey]bool{}
	for rows.Next() {
		var m, physical, args string
		if err := rows.Scan(&m, &physical, &args); err != nil {
			return nil, err
		}
		out[replicaKey{physical, m}] = true
	}
	return out, rows.Err()
}
