package meta

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
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

// Search searches known metadata, not the remote directory tree. A slash in
// query selects literal path-substring matching; otherwise names are searched.
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
	query = strings.TrimSpace(query)
	if query == "" || (roots != nil && len(roots) == 0) {
		return SearchReport{Complete: true}, nil
	}
	if !utf8.ValidString(query) || len(query) > 4096 || strings.ContainsRune(query, 0) {
		return SearchReport{}, errors.New("meta: invalid search query")
	}
	for _, root := range roots {
		if !utf8.ValidString(root) || len(root) > 4096 || !strings.HasPrefix(root, "/") || path.Clean(root) != root || strings.ContainsRune(root, 0) {
			return SearchReport{}, errors.New("meta: invalid search root")
		}
	}
	if limit <= 0 {
		limit = 200
	}
	anchor, _ := searchAnchor(query)
	if utf8.RuneCountInString(anchor) >= 3 && s.pendingIndexExceeds(ctx, searchPendingBudget) {
		// Long queries consult the pending table because a name must not be
		// invisible while it waits. Folding the backlog in first keeps that
		// scan bounded. A read-only store cannot write; the scan below still
		// answers correctly, just more slowly.
		_ = s.FlushIndex(ctx)
	}
	budget := matchBudget(limit)
	stmt, args := searchSQL(query, roots, limit, budget)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return SearchReport{}, err
	}
	defer rows.Close()
	report := SearchReport{Complete: true}
	// A path query expands each anchor subtree independently, so a node under
	// two overlapping anchors arrives twice. Dropping the repeat here is what
	// lets the expansion stay lazy in SQL.
	seen := make(map[uint64]struct{}, limit)
	for rows.Next() {
		var r SearchResult
		var collected int
		if err := rows.Scan(&r.Ino, &r.Name, &r.Path, &collected); err != nil {
			return SearchReport{}, err
		}
		if collected >= budget {
			report.Complete = false
		}
		if _, duplicate := seen[r.Ino]; duplicate {
			continue
		}
		seen[r.Ino] = struct{}{}
		report.Results = append(report.Results, r)
		if len(report.Results) >= limit {
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

// pendingIndexExceeds reports whether more than n names are waiting to enter
// the FTS index, without counting the whole table.
func (s *Store) pendingIndexExceeds(ctx context.Context, n int) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM name_index_pending LIMIT 1 OFFSET ?`, n).Scan(&one)
	return err == nil
}

// searchAnchor picks the term the index lookup is driven by: the whole query
// for a name search, and the longest component for a path search, since only a
// component can be matched against an index of names.
func searchAnchor(query string) (anchor string, pathQuery bool) {
	folded := strings.ToLower(query)
	if !strings.Contains(query, "/") {
		return folded, false
	}
	for _, component := range strings.Split(folded, "/") {
		if utf8.RuneCountInString(component) > utf8.RuneCountInString(anchor) {
			anchor = component
		}
	}
	return anchor, true
}

func searchSQL(query string, roots []string, limit, budget int) (string, []any) {
	folded := strings.ToLower(query)
	anchor, pathQuery := searchAnchor(query)
	var args []any
	var sql string
	if anchor == "" {
		sql = `WITH RECURSIVE seeds(ino,name,path,depth) AS (SELECT 1,'','/',0)`
	} else {
		var candidates string
		if utf8.RuneCountInString(anchor) < 3 {
			candidates = `SELECT n.ino,n.parent_ino,n.name FROM short_name_index f
JOIN nodes n ON n.ino=f.rowid WHERE short_name_index MATCH ?
AND instr(cloudfs_search_fold(n.name),?)>0`
			args = append(args, shortNameToken(anchor), anchor)
		} else {
			// CROSS JOIN pins the order: the deferred index is the small side,
			// and letting SQLite drive from nodes instead turns this into a
			// full table scan that calls the folding function once per node.
			candidates = `SELECT n.ino,n.parent_ino,n.name FROM name_index f
JOIN nodes n ON n.ino=f.rowid WHERE name_index MATCH ? AND f.name=n.name
AND instr(cloudfs_search_fold(n.name),?)>0
UNION SELECT n.ino,n.parent_ino,n.name FROM name_index_pending p
CROSS JOIN nodes n ON n.ino=p.ino WHERE instr(cloudfs_search_fold(n.name),?)>0`
			args = append(args, `"`+strings.ReplaceAll(anchor, `"`, `""`)+`"`, anchor, anchor)
		}
		sql = `WITH RECURSIVE candidates AS (` + candidates + `),
up(candidate,ino,parent_ino,full_path,depth) AS (
 SELECT ino,ino,parent_ino,'/'||name,1 FROM candidates WHERE ino!=1
 UNION ALL
 SELECT u.candidate,n.ino,n.parent_ino,
 CASE WHEN n.ino=1 THEN u.full_path ELSE '/'||n.name||u.full_path END,
 u.depth+CASE WHEN n.ino=1 THEN 0 ELSE 1 END
 FROM up u JOIN nodes n ON n.ino=u.parent_ino WHERE u.ino!=1 AND u.depth<512
), seeds(ino,name,path,depth) AS (
 SELECT u.candidate,n.name,u.full_path,u.depth FROM up u
 JOIN nodes n ON n.ino=u.candidate WHERE u.ino=1
)`
	}
	from := "seeds"
	if pathQuery {
		// UNION ALL, not UNION: the distinct form makes SQLite materialise the
		// whole expansion before the outer query reads a row, so a budget on
		// the result cannot stop a wide subtree from being walked in full.
		// With UNION ALL the recursion is consumed lazily and the budget bites.
		// Overlapping anchor subtrees can therefore repeat a node, which the
		// caller drops while reading — cheap, because it only ever reads as far
		// as the budget.
		// Paths are reconstructed at query time so moving a directory does not
		// rewrite descendant indexes.
		sql += `, expanded(ino,name,path,depth) AS (
 SELECT ino,name,path,depth FROM seeds
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
	// rather than looking like "no more matches".
	sql += `, bounded AS (SELECT matches.ino,matches.name,matches.path,matches.depth
 FROM ` + from + ` matches WHERE matches.ino!=1`
	if pathQuery {
		sql += ` AND instr(cloudfs_search_fold(matches.path),?)>0`
		args = append(args, folded)
	}
	if roots != nil {
		encoded, _ := json.Marshal(roots)
		sql += ` AND EXISTS (SELECT 1 FROM json_each(?) allowed WHERE allowed.value='/'
 OR matches.path=allowed.value OR substr(matches.path,1,length(allowed.value)+1)=allowed.value||'/')`
		args = append(args, string(encoded))
	}
	sql += ` LIMIT ?)
 SELECT ino,name,path,count(*) OVER () FROM bounded ORDER BY depth,path,ino`
	args = append(args, budget)
	return sql, args
}
