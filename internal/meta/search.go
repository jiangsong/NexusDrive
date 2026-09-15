package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"cloudfs/internal/provider"
)

// Work budgets. A search must answer in bounded time on a tree the caller has
// no control over: a one-character query matches most of it, and a path query
// expands every subtree under every anchor match.
const (
	// searchPendingBudget is how many not-yet-indexed names a long query will
	// scan before folding them in instead. The background indexer normally
	// keeps this near zero; a bulk directory refresh does not, and paying the
	// merge once beats paying the scan on every query until it catches up.
	searchPendingBudget = 2048
	// searchMatchBudget caps the per-query budget however large the caller's
	// limit is, so one request cannot spend unbounded time.
	searchMatchBudget = 20000
	// searchMatchFloor is the budget for a small limit. Ordering is by depth,
	// so collecting well beyond the limit is what makes the returned page the
	// shallowest matches rather than the first ones the expansion happened to
	// reach; collecting the whole tree to sort it is what this avoids.
	searchMatchFloor = 2000
)

// matchBudget scales the work a query may spend with what it asked for.
func matchBudget(limit int) int {
	budget := limit * 20
	if budget < searchMatchFloor {
		budget = searchMatchFloor
	}
	if budget > searchMatchBudget {
		budget = searchMatchBudget
	}
	return budget
}

// SearchReport is a search answer together with whether it is the whole
// answer. Callers that show results to a person should say so when it is not:
// "nothing else matched" and "we stopped looking" are different statements.
type SearchReport struct {
	Results []SearchResult
	// Complete is false when the search hit its work budget. The results
	// returned are real matches; there may be more that were not collected.
	Complete bool
}

// Search searches known metadata, not the remote directory tree, with the
// ParseQuery grammar: bare words are AND-ed name substrings and a word with
// a slash is a path substring.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return s.SearchWithin(ctx, query, nil, limit)
}

// SearchWithin applies case-sensitive subtree boundaries before ranking and
// limiting. Nil roots is unrestricted; an empty non-nil slice permits nothing.
// Names and paths are read in one SQLite snapshot, including pending indexing.
func (s *Store) SearchWithin(ctx context.Context, query string, roots []string, limit int) ([]SearchResult, error) {
	report, err := s.SearchReport(ctx, query, roots, limit)
	return report.Results, err
}

// SearchReport is SearchWithin plus the completeness of the answer.
func (s *Store) SearchReport(ctx context.Context, query string, roots []string, limit int) (SearchReport, error) {
	if strings.TrimSpace(query) == "" {
		return SearchReport{Complete: true}, nil
	}
	f, err := ParseQuery(query)
	if err != nil {
		return SearchReport{}, err
	}
	return s.Find(ctx, SearchQuery{Filter: f, Roots: roots, Limit: limit})
}

// Find answers a parsed query. Filters reach SQL before the budgeted set is
// collected, so the budget still counts matches and only matches; the sort
// then orders what was collected, which is the whole answer exactly when
// Complete says so. An empty filter answers nothing rather than everything.
func (s *Store) Find(ctx context.Context, q SearchQuery) (SearchReport, error) {
	if err := validateQuery(q); err != nil {
		return SearchReport{}, err
	}
	if q.Filter.Empty() || (q.Roots != nil && len(q.Roots) == 0) {
		return SearchReport{Complete: true}, nil
	}
	if q.Limit <= 0 {
		q.Limit = 200
	}
	if anchor, short := q.Filter.anchor(); anchor != "" && !short && s.pendingIndexExceeds(ctx, searchPendingBudget) {
		// Long queries consult the pending table because a name must not be
		// invisible while it waits. Folding the backlog in first keeps that
		// scan bounded. A read-only store cannot write; the scan below still
		// answers correctly, just more slowly.
		_ = s.FlushIndex(ctx)
	}
	budget := matchBudget(q.Limit)
	stmt, args := searchSQL(q, budget)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return SearchReport{}, err
	}
	defer rows.Close()
	report := SearchReport{Complete: true}
	// A path query expands each anchor subtree independently, so a node under
	// two overlapping anchors arrives twice, and a name both indexed and
	// pending arrives twice. Dropping the repeat here is what lets the
	// expansion stay lazy in SQL.
	seen := make(map[uint64]struct{}, q.Limit)
	for rows.Next() {
		var r SearchResult
		var kind int
		var mtimeNS int64
		var collected int
		if err := rows.Scan(&r.Ino, &r.Name, &r.Path, &kind, &r.Size, &mtimeNS, &r.Remote, &r.RemoteID, &r.Version, &collected); err != nil {
			return SearchReport{}, err
		}
		r.Kind = provider.Kind(kind)
		r.MTime = time.Unix(0, mtimeNS)
		if collected >= budget {
			report.Complete = false
		}
		if _, duplicate := seen[r.Ino]; duplicate {
			continue
		}
		seen[r.Ino] = struct{}{}
		report.Results = append(report.Results, r)
		if len(report.Results) >= q.Limit {
			// More rows were collected than fit the page; whether anything is
			// missing beyond them is what Complete already reports.
			break
		}
	}
	if err := rows.Err(); err != nil {
		return SearchReport{}, err
	}
	return report, nil
}

// ExplainFind returns the EXPLAIN QUERY PLAN detail lines for a query, so a
// test can assert that no shape of it scans the nodes table.
func (s *Store) ExplainFind(ctx context.Context, q SearchQuery) ([]string, error) {
	if err := validateQuery(q); err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 200
	}
	stmt, args := searchSQL(q, matchBudget(q.Limit))
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

func validateQuery(q SearchQuery) error {
	valid := func(s string) bool {
		return utf8.ValidString(s) && len(s) <= 4096 && !strings.ContainsRune(s, 0)
	}
	for _, t := range q.Filter.Terms {
		if !valid(t.Text) {
			return errors.New("meta: invalid search query")
		}
	}
	for _, p := range append(append([]string{}, q.Filter.Paths...), q.Filter.NotPaths...) {
		if !valid(p) {
			return errors.New("meta: invalid search query")
		}
	}
	for _, e := range append(append([]string{}, q.Filter.Ext...), q.Filter.NotExt...) {
		if e == "" || !valid(e) || strings.ContainsAny(e, "/.") {
			return errors.New("meta: invalid search extension")
		}
	}
	switch q.Filter.Kind {
	case "", "dir", "file":
	default:
		return errors.New("meta: search kind must be dir or file")
	}
	if _, err := orderBy(q.Sort); err != nil {
		return err
	}
	for _, root := range q.Roots {
		if !valid(root) || !strings.HasPrefix(root, "/") || path.Clean(root) != root {
			return errors.New("meta: invalid search root")
		}
	}
	return nil
}

// orderBy maps a sort key to its ORDER BY clause over the bounded set. The
// default is the old order: shallowest first, then path.
func orderBy(sort string) (string, error) {
	reverse := strings.HasPrefix(sort, "-")
	var keys []string
	switch strings.TrimPrefix(sort, "-") {
	case "":
		keys = []string{"depth", "path", "ino"}
	case "name":
		keys = []string{"cloudfs_search_fold(name)", "path"}
	case "size":
		keys = []string{"size DESC", "path"}
	case "mtime":
		keys = []string{"mtime_ns DESC", "path"}
	case "path":
		keys = []string{"path"}
	default:
		return "", fmt.Errorf("meta: unknown sort %q", sort)
	}
	if reverse {
		for i, k := range keys {
			if strings.HasSuffix(k, " DESC") {
				keys[i] = strings.TrimSuffix(k, " DESC")
			} else {
				keys[i] = k + " DESC"
			}
		}
	}
	return strings.Join(keys, ","), nil
}

// pendingIndexExceeds reports whether more than n names are waiting to enter
// the FTS index, without counting the whole table.
func (s *Store) pendingIndexExceeds(ctx context.Context, n int) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM name_index_pending LIMIT 1 OFFSET ?`, n).Scan(&one)
	return err == nil
}

// searchSQL builds the query. Three candidate shapes: a trigram lookup for an
// anchor of three or more runes, the short-name postings for one or two, and
// the root alone when nothing anchors the query. An anchor taken from a path
// component, or no anchor at all, expands subtrees; a name anchor does not,
// because every match must carry it in its own name.
//
// Everything downstream of the index cursor streams. A candidate's path is
// rebuilt by a correlated recursive subquery, one parent-chain walk per row,
// rather than by a recursive CTE over all candidates: SQLite runs a
// recursive CTE's initial select to completion before it yields a row, so
// that form would materialise every name the anchor matches — a quarter of
// a million for "*.pdf" on a million-node tree — before the budget could
// stop it. Node filters sit in the candidate CTE when there is no expansion,
// so a path is only rebuilt for a row that passes them, and in the bounded
// CTE otherwise; path filters and roots always sit in bounded, after the
// join with nodes, so the LIMIT that is the budget counts matches only.
func searchSQL(q SearchQuery, budget int) (string, []any) {
	f := q.Filter
	anchor, fromPath, prefix := f.anchorSource()
	expand := anchor == "" || fromPath
	nodeWhere, nodeArgs := nodeFilterSQL(f)
	var args []any
	var sql string
	if anchor == "" {
		sql = `WITH RECURSIVE seeds(ino,name,path) AS (SELECT 1,'','/')`
	} else {
		var candidates string
		if utf8.RuneCountInString(anchor) < 3 {
			candidates = `SELECT n.ino,n.parent_ino,n.name FROM short_name_index f
JOIN nodes n ON n.ino=f.rowid WHERE short_name_index MATCH ?
AND instr(cloudfs_search_fold(n.name),?)>0`
			args = append(args, shortNameToken(anchor), anchor)
			if !expand {
				candidates += nodeWhere
				args = append(args, nodeArgs...)
			}
		} else {
			// UNION ALL keeps both halves streaming: the distinct form
			// would materialise every trigram hit before the first row
			// reaches the budget. A name that is both indexed and pending
			// can arrive twice; Find drops the repeat. CROSS JOIN pins the
			// order: the deferred index is the small side, and letting
			// SQLite drive from nodes instead turns this into a full table
			// scan that calls the folding function once per node.
			indexed := `SELECT n.ino,n.parent_ino,n.name FROM name_index f
JOIN nodes n ON n.ino=f.rowid WHERE name_index MATCH ? AND f.name=n.name
AND instr(cloudfs_search_fold(n.name),?)>0`
			pending := `SELECT n.ino,n.parent_ino,n.name FROM name_index_pending p
CROSS JOIN nodes n ON n.ino=p.ino WHERE instr(cloudfs_search_fold(n.name),?)>0`
			args = append(args, `"`+strings.ReplaceAll(anchor, `"`, `""`)+`"`, anchor)
			if !expand {
				indexed += nodeWhere
				pending += nodeWhere
				args = append(args, nodeArgs...)
			}
			args = append(args, anchor)
			if !expand {
				args = append(args, nodeArgs...)
			}
			candidates = indexed + "\nUNION ALL " + pending
		}
		// LIMIT -1 is no limit; it is here so the flattener leaves seeds a
		// co-routine and the chain below runs once per candidate rather
		// than once per reference to its path. A chain that never reaches
		// the root (an orphan) yields NULL, which bounded drops.
		sql = `WITH RECURSIVE candidates AS (` + candidates + `),
seeds(ino,name,path) AS (
 SELECT c.ino,c.name,(SELECT path FROM (WITH RECURSIVE chain(ino,path,depth) AS (
  SELECT c.parent_ino,'/'||c.name,1
  UNION ALL
  SELECT n.parent_ino,'/'||n.name||chain.path,chain.depth+1
  FROM chain JOIN nodes n ON n.ino=chain.ino WHERE chain.ino!=1 AND chain.depth<512
 ) SELECT path FROM chain WHERE ino=1))
 FROM candidates c WHERE c.ino!=1 LIMIT -1
)`
	}
	from := "seeds"
	if expand {
		// UNION ALL, not UNION: the distinct form makes SQLite materialise the
		// whole expansion before the outer query reads a row, so a budget on
		// the result cannot stop a wide subtree from being walked in full.
		// With UNION ALL the recursion is consumed lazily and the budget bites.
		// Overlapping anchor subtrees can therefore repeat a node, which the
		// caller drops while reading — cheap, because it only ever reads as far
		// as the budget.
		// Paths are reconstructed at query time so moving a directory does not
		// rewrite descendant indexes.
		//
		// A path anchor prunes the seeds first: a match must contain the path
		// term, whose components up to the anchor lie inside the path of the
		// seed the match descends from (or is), so a seed without that prefix
		// has nothing under it worth walking.
		sql += `, expanded(ino,name,path,depth) AS (
 SELECT ino,name,path,length(path)-length(replace(path,'/','')) FROM seeds WHERE path IS NOT NULL`
		if prefix != "" {
			sql += ` AND instr(cloudfs_search_fold(path),?)>0`
			args = append(args, prefix)
		}
		sql += `
 UNION ALL
 SELECT n.ino,n.name,CASE WHEN e.path='/' THEN '/'||n.name ELSE e.path||'/'||n.name END,e.depth+1
 FROM expanded e JOIN nodes n ON n.parent_ino=e.ino
 WHERE n.ino!=1 AND n.ino!=e.ino AND e.depth<512
)`
		from = "expanded"
	}
	// The budget is applied to matching rows, not to visited ones: filtering
	// inside the bounded set is what lets SQLite stop pulling from the
	// recursive term once enough matches exist, instead of expanding every
	// subtree and sorting the lot. count(*) OVER () then reports how many were
	// collected, which is how a cut budget becomes visible to the caller
	// rather than looking like "no more matches". CROSS JOIN pins nodes as
	// the inner side: SQLite otherwise prefers to scan the whole table and
	// probe the expansion through an automatic index.
	sql += `, bounded AS (SELECT matches.ino,matches.name,matches.path,
 length(matches.path)-length(replace(matches.path,'/','')) AS depth,
 n.kind,n.size,n.mtime_ns,n.remote,n.remote_id,n.version
 FROM ` + from + ` matches CROSS JOIN nodes n ON n.ino=matches.ino WHERE matches.ino!=1 AND matches.path IS NOT NULL`
	if expand {
		sql += nodeWhere
		args = append(args, nodeArgs...)
	}
	pathWhere, pathArgs := pathFilterSQL(f)
	sql += pathWhere
	args = append(args, pathArgs...)
	if q.Roots != nil {
		encoded, _ := json.Marshal(q.Roots)
		sql += ` AND EXISTS (SELECT 1 FROM json_each(?) allowed WHERE allowed.value='/'
 OR matches.path=allowed.value OR substr(matches.path,1,length(allowed.value)+1)=allowed.value||'/')`
		args = append(args, string(encoded))
	}
	order, _ := orderBy(q.Sort)
	sql += ` LIMIT ?)
 SELECT ino,name,path,kind,size,mtime_ns,remote,remote_id,version,count(*) OVER () FROM bounded ORDER BY ` + order
	args = append(args, budget)
	return sql, args
}

// nodeFilterSQL renders the filters that need only the node row, as AND
// clauses over an alias n of nodes.
func nodeFilterSQL(f Filter) (string, []any) {
	var b strings.Builder
	var args []any
	for _, t := range f.Terms {
		switch {
		case t.Glob && t.Negate:
			b.WriteString(` AND NOT (cloudfs_search_fold(n.name) GLOB ?)`)
			args = append(args, globPattern(t.Text))
		case t.Glob:
			b.WriteString(` AND cloudfs_search_fold(n.name) GLOB ?`)
			args = append(args, globPattern(t.Text))
		case t.Negate:
			b.WriteString(` AND instr(cloudfs_search_fold(n.name),?)=0`)
			args = append(args, strings.ToLower(t.Text))
		default:
			b.WriteString(` AND instr(cloudfs_search_fold(n.name),?)>0`)
			args = append(args, strings.ToLower(t.Text))
		}
	}
	extClause := func(exts []string) string {
		clauses := make([]string, 0, len(exts))
		for _, e := range exts {
			clauses = append(clauses, `cloudfs_search_fold(n.name) GLOB ?`)
			args = append(args, "*."+globLiteral(e))
		}
		return strings.Join(clauses, " OR ")
	}
	if len(f.Ext) > 0 {
		b.WriteString(` AND (` + extClause(f.Ext) + `)`)
	}
	if len(f.NotExt) > 0 {
		b.WriteString(` AND NOT (` + extClause(f.NotExt) + `)`)
	}
	if f.MinSize >= 0 {
		b.WriteString(` AND n.size>=?`)
		args = append(args, f.MinSize)
	}
	if f.MaxSize >= 0 {
		b.WriteString(` AND n.size<=?`)
		args = append(args, f.MaxSize)
	}
	if !f.ModifiedAfter.IsZero() {
		b.WriteString(` AND n.mtime_ns>=?`)
		args = append(args, f.ModifiedAfter.UnixNano())
	}
	if !f.ModifiedBefore.IsZero() {
		b.WriteString(` AND n.mtime_ns<?`)
		args = append(args, f.ModifiedBefore.UnixNano())
	}
	switch f.Kind {
	case "dir":
		b.WriteString(` AND n.kind=?`)
		args = append(args, int(provider.KindDir))
	case "file":
		b.WriteString(` AND n.kind=?`)
		args = append(args, int(provider.KindFile))
	}
	return b.String(), args
}

// pathFilterSQL renders the filters that need the reconstructed path, as
// AND clauses over matches.path.
func pathFilterSQL(f Filter) (string, []any) {
	var b strings.Builder
	var args []any
	for _, p := range f.Paths {
		b.WriteString(` AND instr(cloudfs_search_fold(matches.path),?)>0`)
		args = append(args, strings.ToLower(p))
	}
	for _, p := range f.NotPaths {
		b.WriteString(` AND instr(cloudfs_search_fold(matches.path),?)=0`)
		args = append(args, strings.ToLower(p))
	}
	return b.String(), args
}
