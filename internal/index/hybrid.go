package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cloudfs/internal/embed"
)

// This file is the semantic half of Search (docs/agent-roadmap.md §3.7):
// deciding whether the embedder can be used at all, vector-only ranking by
// cosine over the in-memory set, and hybrid ranking that fuses the bm25
// and cosine lists by reciprocal rank. Every path that cannot run reports
// itself in SearchResult.Degraded and runs keyword instead; ModeUsed is
// never a mode that did not run.

// Search modes. modeKeyword is in search.go.
const (
	modeHybrid = "hybrid"
	modeVector = "vector"
	// rrfK is the reciprocal-rank-fusion constant; 60 is the usual value
	// and the one TODO.md T-39 names.
	rrfK = 60
	// fuseFactor is how many candidates each side of a hybrid search
	// contributes relative to TopK.
	fuseFactor = 2
)

// Degraded messages for a semantic request that ran as keyword.
const (
	degradedUnhealthy  = "embedding endpoint is unhealthy; results use keyword matching"
	degradedNoVectors  = "no chunks have been embedded yet; results use keyword matching"
	degradedQueryEmbed = "the query could not be embedded; results use keyword matching"
)

// semanticCheck says whether the vector side can run: e is present and
// healthy, vectors exist, and they were made by e's model. why is the
// Degraded text otherwise.
func (s *Store) semanticCheck(ctx context.Context, e embed.Embedder, st Stats) (ok bool, why string) {
	if e == nil {
		return false, DegradedNoEmbedding
	}
	if rec, err := s.Meta(ctx, metaEmbeddingModel); err == nil && rec != "" && rec != e.Model() {
		// The configuration changed and the worker has not re-embedded
		// yet (or this process is not the owner and never will). A query
		// in one model against vectors of another is noise.
		return false, fmt.Sprintf("vectors were embedded with model %s, the configured model is %s; results use keyword matching until the index is re-embedded", rec, e.Model())
	}
	if r, ok := e.(embed.Reporter); ok {
		if h := r.Status(); !h.Healthy {
			why := degradedUnhealthy
			switch {
			case !h.BreakerOpenUntil.IsZero():
				why = fmt.Sprintf("embedding endpoint is paused until %s; results use keyword matching", h.BreakerOpenUntil.Format(time.RFC3339))
			case h.LastError != "":
				why = fmt.Sprintf("embedding endpoint is unhealthy (%s); results use keyword matching", h.LastError)
			}
			return false, why
		}
	}
	if st.Vectors == 0 {
		return false, degradedNoVectors
	}
	return true, ""
}

// resolveMode turns the requested mode into the one to run. semantic says
// whether the vector side is available and why not. An empty mode is
// hybrid when it can be and keyword otherwise; the fallback is reported
// only when an embedder is configured, since a keyword-only index is what
// the caller of a default search on it expects. A short query (a term
// under three runes, which the trigram index cannot see) runs vector-only
// when vectors are available and the caller did not insist on keyword.
func resolveMode(requested string, semantic bool, why string, short bool) (mode, degraded string, err error) {
	switch requested {
	case "":
		if !semantic {
			if why == DegradedNoEmbedding {
				why = ""
			}
			return modeKeyword, why, nil
		}
		mode = modeHybrid
	case modeKeyword:
		return modeKeyword, "", nil
	case modeHybrid, modeVector:
		if !semantic {
			return modeKeyword, why, nil
		}
		mode = requested
	default:
		return "", "", fmt.Errorf("index: unknown search mode %q", requested)
	}
	if short {
		mode = modeVector
	}
	return mode, "", nil
}

// embedText is what the worker sends for a chunk: the heading path first,
// so a chunk under "第二章 > 范围" embeds with its context.
func embedText(p PendingEmbed) string {
	if p.Heading == "" {
		return p.Text
	}
	return p.Heading + "\n" + p.Text
}

// embedQuery embeds the query text through e and normalises it. The chars
// count toward the month like any other text sent.
func (s *Store) embedQuery(ctx context.Context, e embed.Embedder, query string) ([]float32, error) {
	vecs, err := e.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("index: the embedder returned %d vectors for the query", len(vecs))
	}
	_ = s.AddEmbedChars(ctx, int64(utf8.RuneCountInString(query)))
	return normalise(vecs[0]), nil
}

// allowedDocs is the set of ok documents inside the scope, nil when every
// document qualifies (unrestricted scope and no dirty or failed document).
func (s *Store) allowedDocs(ctx context.Context, sc scope, st Stats) (map[uint32]bool, error) {
	if sc.where == "" && st.DocsDirty == 0 && st.DocsFailed == 0 {
		return nil, nil
	}
	args := append([]any{int(DocOK)}, sc.args...)
	rows, err := s.db.QueryContext(ctx, `SELECT d.id FROM documents d WHERE d.state = ?`+sc.sql(), args...)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	out := map[uint32]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		out[uint32(id)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return out, nil
}

// vectorTopK ranks the scope's chunks by cosine against q.
func (s *Store) vectorTopK(ctx context.Context, q []float32, sc scope, st Stats, k int) ([]vecHit, error) {
	allowed, err := s.allowedDocs(ctx, sc, st)
	if err != nil {
		return nil, err
	}
	vs, release, err := s.vectors(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if vs.dim != len(q) {
		return nil, fmt.Errorf("index: the query vector has %d dimensions, the stored vectors %d; the embedding model changed", len(q), vs.dim)
	}
	return vs.cosineTopK(q, allowed, k), nil
}

// ftsTopK returns the chunk ids of the scope's best bm25 matches, best
// first.
func (s *Store) ftsTopK(ctx context.Context, terms []string, sc scope, k int) ([]int64, error) {
	match := MatchQuery(strings.Join(terms, " "))
	args := append([]any{match, int(DocOK)}, sc.args...)
	args = append(args, k)
	rows, err := s.db.QueryContext(ctx, `SELECT c.id
		FROM chunks_fts
		JOIN chunks c ON c.id = chunks_fts.rowid
		JOIN documents d ON d.id = c.doc_id
		WHERE chunks_fts MATCH ? AND d.state = ?`+sc.sql()+`
		ORDER BY bm25(chunks_fts), d.path, c.seq LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return out, nil
}

// ranked is one chunk with the score its mode gave it.
type ranked struct {
	chunkID int64
	score   float64
}

// fuseRRF merges two ranked lists by reciprocal rank: a chunk scores
// Σ 1/(rrfK + rank) over the lists it appears in, rank counted from 1.
// Ties fall back to chunk id so the order is stable.
func fuseRRF(lists ...[]int64) []ranked {
	scores := map[int64]float64{}
	for _, l := range lists {
		for i, id := range l {
			scores[id] += 1 / float64(rrfK+i+1)
		}
	}
	out := make([]ranked, 0, len(scores))
	for id, sc := range scores {
		out = append(out, ranked{chunkID: id, score: sc})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].chunkID < out[b].chunkID
	})
	return out
}

// searchSemantic runs the vector or hybrid mode and fills res. It reports
// a Degraded reason instead of an error when the query cannot be embedded
// or scored, so the caller falls back to keyword.
func (s *Store) searchSemantic(ctx context.Context, q SearchQuery, terms []string, sc scope, st Stats, current Current, e embed.Embedder, mode string, res *SearchResult) (degraded string, err error) {
	qv, err := s.embedQuery(ctx, e, q.Query)
	if err != nil {
		return fmt.Sprintf("%s (%v)", degradedQueryEmbed, err), nil
	}
	var order []ranked
	switch mode {
	case modeVector:
		hits, err := s.vectorTopK(ctx, qv, sc, st, q.TopK)
		if err != nil {
			return fmt.Sprintf("%v; results use keyword matching", err), nil
		}
		for _, h := range hits {
			order = append(order, ranked{chunkID: int64(h.chunkID), score: float64(h.score)})
		}
	case modeHybrid:
		n := fuseFactor * q.TopK
		vec, err := s.vectorTopK(ctx, qv, sc, st, n)
		if err != nil {
			return fmt.Sprintf("%v; results use keyword matching", err), nil
		}
		fts, err := s.ftsTopK(ctx, terms, sc, n)
		if err != nil {
			return "", err
		}
		byCos := make([]int64, len(vec))
		for i, h := range vec {
			byCos[i] = int64(h.chunkID)
		}
		order = fuseRRF(fts, byCos)
	}
	res.ModeUsed = mode
	if len(order) == 0 {
		return "", nil
	}
	rowsByID, err := s.hitRows(ctx, order)
	if err != nil {
		return "", err
	}
	var spent int64
	for _, r := range order {
		row, ok := rowsByID[r.chunkID]
		if !ok {
			// Deleted between scoring and fetching.
			continue
		}
		row.score = r.score
		if !appendHit(res, row, terms, q, sc, current, &spent) {
			break
		}
	}
	return "", nil
}

// hitRows fetches the chunk rows behind a ranking, keyed by chunk id.
func (s *Store) hitRows(ctx context.Context, order []ranked) (map[int64]hitRow, error) {
	out := make(map[int64]hitRow, len(order))
	// Bounded IN lists keep the statement under SQLite's parameter limit.
	const page = 200
	for start := 0; start < len(order); start += page {
		end := min(start+page, len(order))
		marks := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, r := range order[start:end] {
			marks = append(marks, "?")
			args = append(args, r.chunkID)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT c.id, `+hitColumns+`
			FROM chunks c JOIN documents d ON d.id = c.doc_id
			WHERE c.id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		for rows.Next() {
			var id int64
			var r hitRow
			if err := rows.Scan(&id, &r.seq, &r.startOff, &r.endOff, &r.heading, &r.text, &r.path, &r.remote, &r.remoteID, &r.version, &r.kind); err != nil {
				rows.Close()
				return nil, fmt.Errorf("index: %w", err)
			}
			out[id] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("index: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
