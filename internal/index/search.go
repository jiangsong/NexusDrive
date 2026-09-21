package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"cloudfs/internal/embed"
	"cloudfs/internal/textract"
)

// SearchQuery is one content search (docs/agent-roadmap.md §3.7).
type SearchQuery struct {
	Query string
	// Roots restricts hits to these subtrees. The caller has already
	// intersected them with its read scope. The semantics are those of
	// meta.SearchWithin: nil is unrestricted, an empty non-nil slice
	// permits nothing, and "/" among the roots lifts the restriction.
	Roots []string
	// TopK caps the number of hits; 0 takes defaultTopK.
	TopK int
	// ScanLimit bounds ranked candidates inspected before Accept. It defaults
	// to TopK; callers that filter hits can raise it to refill the requested
	// result count without an unbounded scan.
	ScanLimit int
	// Accept filters fully resolved hits before they count toward TopK or the
	// response byte budget. A nil function accepts every hit.
	Accept func(Hit) bool
	// Mode is hybrid, keyword or vector; "" is hybrid when an embedder and
	// vectors are available and keyword otherwise. hybrid and vector run
	// as keyword, reporting Degraded, when the embedder is missing or
	// unhealthy or nothing is embedded yet (docs/agent-roadmap.md §3.7).
	Mode string
	// MaxSnippetBytes bounds each snippet; 0 takes defaultSnippetBytes.
	MaxSnippetBytes int
	// MaxBytes bounds the whole response, charged per hit by appendHit;
	// 0 is unlimited.
	MaxBytes int64
}

// Hit is one matching chunk. Path is the file's current path in the live
// tree when the caller supplied a Current, so a rename is visible before
// the indexer has caught up; Stale says the indexed text belongs to an
// older version than meta holds now (or the file is gone).
type Hit struct {
	Path       string  `json:"path"`
	Version    string  `json:"version,omitempty"`
	Seq        int     `json:"seq"`
	StartOff   int64   `json:"start_off"`
	EndOff     int64   `json:"end_off"`
	Heading    string  `json:"heading,omitempty"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	OffsetKind string  `json:"offset_kind"` // file | text
	Stale      bool    `json:"stale"`
}

// SearchResult is what Search returns. Truncated is set when a budget
// (response bytes or the short-query row scan) stopped the search before
// it could say whether more hits exist.
type SearchResult struct {
	Hits      []Hit  `json:"hits"`
	ModeUsed  string `json:"mode_used"`
	Degraded  string `json:"degraded,omitempty"`
	Truncated bool   `json:"truncated"`
	// Docs is the number of documents whose text is searchable (state ok);
	// Pending is the length of the extraction queue.
	Docs    int `json:"docs"`
	Pending int `json:"pending"`
}

// Current reports what meta holds now for (remote, remoteID): the node's
// live path and version. ok is false when no such node exists any more.
// A nil Current leaves hits at their indexed path and never marks them
// stale.
type Current func(remote, remoteID string) (path, version string, ok bool)

const (
	// shortQueryRowBudget is how many chunks a query too short for the
	// trigram index may scan before answering Truncated.
	shortQueryRowBudget = 20000
	// minTrigramRunes is the shortest term the trigram tokenizer can match.
	minTrigramRunes = 3
	// defaultTopK and defaultSnippetBytes fill zero query fields.
	defaultTopK         = 10
	defaultSnippetBytes = 1024
	// hitOverhead approximates the JSON framing of one hit when charging
	// the response budget.
	hitOverhead = 64
	// modeKeyword is the mode every index can run.
	modeKeyword = "keyword"
)

// DegradedNoEmbedding is SearchResult.Degraded for hybrid and vector
// requests while no embedding backend is configured.
const DegradedNoEmbedding = "semantic search is not configured; results use keyword matching"

// ErrEmptyQuery is returned when the query holds no term.
var ErrEmptyQuery = errors.New("index: empty query")

// Search runs a keyword search over the chunks of ok documents under
// q.Roots. Terms of three or more runes go through the FTS5 trigram index
// ranked by bm25; a query with a shorter term (two Chinese characters,
// say) cannot use trigrams and falls back to a substring scan over at most
// shortQueryRowBudget chunks, reporting Truncated rather than an empty
// "no match" when the budget runs out first. hybrid and vector requests
// run as keyword with Degraded set: there is no embedder here.
func (s *Store) Search(ctx context.Context, q SearchQuery, current Current) (SearchResult, error) {
	return s.searchWithRowBudget(ctx, q, current, nil, shortQueryRowBudget)
}

// SearchWith is Search with an embedder for the vector and hybrid modes
// (hybrid.go). A nil e is Search. When e cannot be used (unhealthy, no
// vectors yet, the query fails to embed) the search runs as keyword and
// says why in Degraded; ModeUsed always names the mode that ran.
func (s *Store) SearchWith(ctx context.Context, q SearchQuery, current Current, e embed.Embedder) (SearchResult, error) {
	return s.searchWithRowBudget(ctx, q, current, e, shortQueryRowBudget)
}

// searchWithRowBudget is Search with the short-query scan budget as a
// parameter, so a test can exhaust it with a handful of rows.
func (s *Store) searchWithRowBudget(ctx context.Context, q SearchQuery, current Current, e embed.Embedder, budget int) (SearchResult, error) {
	res := SearchResult{Hits: []Hit{}, ModeUsed: modeKeyword}
	terms := strings.Fields(q.Query)
	if len(terms) == 0 {
		if _, _, err := resolveMode(q.Mode, false, "", false); err != nil {
			return SearchResult{}, err
		}
		return SearchResult{}, ErrEmptyQuery
	}
	if q.TopK <= 0 {
		q.TopK = defaultTopK
	}
	if q.ScanLimit < q.TopK {
		q.ScanLimit = q.TopK
	}
	if q.MaxSnippetBytes <= 0 {
		q.MaxSnippetBytes = defaultSnippetBytes
	}
	st, err := s.Stats(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	res.Docs, res.Pending = st.DocsOK, st.Pending
	semantic, why := s.semanticCheck(ctx, e, st)
	short := shortQuery(terms)
	mode, degraded, err := resolveMode(q.Mode, semantic, why, short)
	if err != nil {
		return SearchResult{}, err
	}
	res.Degraded = degraded

	scope, ok := newScope(q.Roots)
	if !ok {
		return res, nil
	}
	if mode != modeKeyword {
		degraded, err := s.searchSemantic(ctx, q, terms, scope, st, current, e, mode, &res)
		if err != nil {
			return SearchResult{}, err
		}
		if degraded == "" {
			return res, nil
		}
		// The vector side failed after all: keyword, and say so.
		res.Degraded, res.ModeUsed = degraded, modeKeyword
	}
	if short {
		err = s.scanShort(ctx, q, terms, scope, current, budget, &res)
	} else {
		err = s.searchFTS(ctx, q, terms, scope, current, &res)
	}
	if err != nil {
		return SearchResult{}, err
	}
	return res, nil
}

// shortQuery reports whether any term is too short for the trigram index.
// FTS5's trigram tokenizer matches nothing for a phrase under three
// runes, and the phrases are ANDed, so one short term empties the result.
func shortQuery(terms []string) bool {
	for _, t := range terms {
		if utf8.RuneCountInString(t) < minTrigramRunes {
			return true
		}
	}
	return false
}

// scope is the root filter: the SQL fragment that bounds the query and the
// Go check that re-validates a live path the SQL never saw.
type scope struct {
	roots []string // nil when unrestricted
	where string   // "" when unrestricted, else "(... OR ...)"
	args  []any
}

// newScope builds the filter. ok is false when the roots permit nothing.
func newScope(roots []string) (scope, bool) {
	if roots == nil {
		return scope{}, true
	}
	if len(roots) == 0 {
		return scope{}, false
	}
	var sc scope
	var parts []string
	for _, r := range roots {
		r = path.Clean("/" + r)
		if r == "/" {
			return scope{}, true
		}
		sc.roots = append(sc.roots, r)
		parts = append(parts, `(d.path = ? OR substr(d.path, 1, length(?) + 1) = ? || '/')`)
		sc.args = append(sc.args, r, r, r)
	}
	sc.where = "(" + strings.Join(parts, " OR ") + ")"
	return sc, true
}

// sql prefixes the fragment with AND for a WHERE clause.
func (sc scope) sql() string {
	if sc.where == "" {
		return ""
	}
	return " AND " + sc.where
}

// allows reports whether p lies under one of the roots.
func (sc scope) allows(p string) bool {
	if sc.roots == nil {
		return true
	}
	for _, r := range sc.roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// hitRow is one chunk row before it becomes a Hit.
type hitRow struct {
	seq              int
	startOff, endOff int64
	heading, text    string
	path, remote     string
	remoteID         string
	version, kind    string
	score            float64
}

const hitColumns = `c.seq, c.start_off, c.end_off, c.heading, c.text, d.path, d.remote, d.remote_id, d.version, d.kind`

func scanHitRow(rows *sql.Rows, withScore bool) (hitRow, error) {
	var r hitRow
	dest := []any{&r.seq, &r.startOff, &r.endOff, &r.heading, &r.text, &r.path, &r.remote, &r.remoteID, &r.version, &r.kind}
	if withScore {
		dest = append(dest, &r.score)
	}
	if err := rows.Scan(dest...); err != nil {
		return hitRow{}, fmt.Errorf("index: %w", err)
	}
	return r, nil
}

// searchFTS ranks chunks by bm25 through the trigram index.
func (s *Store) searchFTS(ctx context.Context, q SearchQuery, terms []string, sc scope, current Current, res *SearchResult) error {
	match := MatchQuery(strings.Join(terms, " "))
	args := append([]any{match, int(DocOK)}, sc.args...)
	args = append(args, q.ScanLimit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+hitColumns+`, bm25(chunks_fts) AS score
		FROM chunks_fts
		JOIN chunks c ON c.id = chunks_fts.rowid
		JOIN documents d ON d.id = c.doc_id
		WHERE chunks_fts MATCH ? AND d.state = ?`+sc.sql()+`
		ORDER BY score, d.path, c.seq LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var spent int64
	scanned := 0
	for rows.Next() {
		if scanned == q.ScanLimit {
			res.Truncated = true
			return nil
		}
		r, err := scanHitRow(rows, true)
		if err != nil {
			return err
		}
		scanned++
		// bm25 is negative and lower for better matches; expose the
		// natural "higher is better" form.
		r.score = -r.score
		if !appendHit(res, r, terms, q, sc, current, &spent) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// scanShort answers a query the trigram index cannot: it walks up to
// budget chunks in path order and keeps those containing every term
// (case-insensitively). Reaching the budget with rows left over and fewer
// than TopK hits is reported as Truncated.
func (s *Store) scanShort(ctx context.Context, q SearchQuery, terms []string, sc scope, current Current, budget int, res *SearchResult) error {
	args := append([]any{int(DocOK)}, sc.args...)
	args = append(args, budget+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+hitColumns+`
		FROM chunks c
		JOIN documents d ON d.id = c.doc_id
		WHERE d.state = ?`+sc.sql()+`
		ORDER BY d.path, c.seq LIMIT ?`, args...)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	lowered := make([]string, len(terms))
	for i, t := range terms {
		lowered[i] = strings.ToLower(t)
	}
	scanned := 0
	var spent int64
	for rows.Next() {
		if scanned == budget {
			// The extra row past the budget proves there was more to scan.
			if len(res.Hits) < q.TopK {
				res.Truncated = true
			}
			return nil
		}
		r, err := scanHitRow(rows, false)
		if err != nil {
			return err
		}
		scanned++
		if !containsAll(strings.ToLower(r.text), lowered) {
			continue
		}
		r.score = 1
		if !appendHit(res, r, terms, q, sc, current, &spent) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

func containsAll(text string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}

// appendHit resolves the row against the live tree, charges the response
// budget and appends the hit. It returns false once the budget is spent
// (Truncated is then set) or TopK is reached, so the caller stops.
func appendHit(res *SearchResult, r hitRow, terms []string, q SearchQuery, sc scope, current Current, spent *int64) bool {
	h := Hit{
		Path: r.path, Seq: r.seq, StartOff: r.startOff, EndOff: r.endOff,
		Heading: r.heading, Score: r.score, OffsetKind: offsetKind(r.kind), Version: r.version,
	}
	if current != nil {
		livePath, liveVersion, ok := current(r.remote, r.remoteID)
		h.Stale = !ok || liveVersion != r.version
		if ok && livePath != "" && livePath != r.path {
			// The indexer has not applied the rename yet. Show the live
			// path, but only if it still sits inside the caller's scope:
			// the SQL filter judged the old path.
			if !sc.allows(livePath) {
				return true
			}
			h.Path = livePath
		}
	}
	h.Snippet = snippet(r.text, terms, q.MaxSnippetBytes)
	if q.Accept != nil && !q.Accept(h) {
		return true
	}
	if q.MaxBytes > 0 {
		cost := int64(len(h.Path) + len(h.Heading) + len(h.Snippet) + hitOverhead)
		if *spent+cost > q.MaxBytes {
			res.Truncated = true
			return false
		}
		*spent += cost
	}
	res.Hits = append(res.Hits, h)
	return len(res.Hits) < q.TopK
}

// offsetKind says what a hit's offsets address for a document kind.
func offsetKind(kind string) string {
	switch textract.Kind(kind) {
	case textract.KindDocx, textract.KindXlsx, textract.KindPptx, textract.KindPDF:
		return "text"
	}
	return "file"
}

// snippet cuts a window of at most max bytes out of text, centred on the
// first occurrence of the first term found (case-insensitively), with both
// ends on rune boundaries.
func snippet(text string, terms []string, max int) string {
	if len(text) <= max {
		return text
	}
	at, width := firstMatch(text, terms)
	start := at - (max-width)/2
	if start > len(text)-max {
		start = len(text) - max
	}
	if start < 0 {
		start = 0
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return truncateRunes(text[start:], max)
}

// firstMatch locates the earliest term occurrence in text and returns its
// byte offset and length; (0, 0) when nothing is found (a heading-only
// match, or a fold the lowercase comparison cannot reproduce).
func firstMatch(text string, terms []string) (int, int) {
	lower := strings.ToLower(text)
	// ToLower can change byte lengths for a few scripts; offsets into the
	// lowered copy are only offsets into text when the lengths agree.
	foldable := len(lower) == len(text)
	best, width := -1, 0
	for _, t := range terms {
		var i int
		if foldable {
			i = strings.Index(lower, strings.ToLower(t))
		} else {
			i = strings.Index(text, t)
		}
		if i >= 0 && (best < 0 || i < best) {
			best, width = i, len(t)
		}
	}
	if best < 0 {
		return 0, 0
	}
	return best, width
}
