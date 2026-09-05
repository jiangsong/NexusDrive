package meta

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

// Search searches known metadata, not the remote directory tree. A slash in
// query selects literal path-substring matching; otherwise names are searched.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return s.SearchWithin(ctx, query, nil, limit)
}

// SearchWithin applies case-sensitive subtree boundaries before ranking and
// limiting. Nil roots is unrestricted; an empty non-nil slice permits nothing.
// Names and paths are read in one SQLite snapshot, including pending indexing.
func (s *Store) SearchWithin(ctx context.Context, query string, roots []string, limit int) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || (roots != nil && len(roots) == 0) {
		return nil, nil
	}
	if !utf8.ValidString(query) || len(query) > 4096 || strings.ContainsRune(query, 0) {
		return nil, errors.New("meta: invalid search query")
	}
	for _, root := range roots {
		if !utf8.ValidString(root) || len(root) > 4096 || !strings.HasPrefix(root, "/") || path.Clean(root) != root || strings.ContainsRune(root, 0) {
			return nil, errors.New("meta: invalid search root")
		}
	}
	if limit <= 0 {
		limit = 200
	}
	stmt, args := searchSQL(query, roots, limit)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Ino, &r.Name, &r.Path); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func searchSQL(query string, roots []string, limit int) (string, []any) {
	folded := strings.ToLower(query)
	pathQuery := strings.Contains(query, "/")
	anchor := folded
	if pathQuery {
		anchor = ""
		for _, component := range strings.Split(folded, "/") {
			if utf8.RuneCountInString(component) > utf8.RuneCountInString(anchor) {
				anchor = component
			}
		}
	}
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
			candidates = `SELECT n.ino,n.parent_ino,n.name FROM name_index f
JOIN nodes n ON n.ino=f.rowid WHERE name_index MATCH ? AND f.name=n.name
AND instr(cloudfs_search_fold(n.name),?)>0
UNION SELECT n.ino,n.parent_ino,n.name FROM name_index_pending p
JOIN nodes n ON n.ino=p.ino WHERE instr(cloudfs_search_fold(n.name),?)>0`
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
		// UNION deduplicates overlapping anchor subtrees. Paths are reconstructed
		// at query time so moving a directory does not rewrite descendant indexes.
		sql += `, expanded(ino,name,path,depth) AS (
 SELECT ino,name,path,depth FROM seeds
 UNION
 SELECT n.ino,n.name,CASE WHEN e.path='/' THEN '/'||n.name ELSE e.path||'/'||n.name END,e.depth+1
 FROM expanded e JOIN nodes n ON n.parent_ino=e.ino
 WHERE n.ino!=1 AND n.ino!=e.ino AND e.depth<512
)`
		from = "expanded"
	}
	sql += ` SELECT matches.ino,matches.name,matches.path FROM ` + from + ` matches WHERE matches.ino!=1`
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
	sql += ` ORDER BY matches.depth,matches.path,matches.ino LIMIT ?`
	args = append(args, limit)
	return sql, args
}
