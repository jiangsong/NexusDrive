package index

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the vector side of the store (docs/agent-roadmap.md §3.6,
// §3.7): how a chunk's embedding is encoded on disk, the in-memory set the
// brute-force cosine scan runs over, and the embed_pending queue.
//
// The set is loaded from the vectors table once and kept in step with the
// store's own writes: PutVectors appends, a document deletion tombstones
// its entries, and a reset drops the whole set. Other processes (a
// read-only CLI, an MCP server that does not own the index) cannot see
// those in-memory updates, so every writer also bumps index_meta
// vectors_gen and a search reloads when the generation it holds is not the
// one on disk. The owner's generation always matches its own writes, so it
// reloads only after a crash-recovery mismatch.

// index_meta keys the embedding side records.
const (
	metaEmbeddingModel = "embedding_model"
	metaEmbeddingDim   = "embedding_dim"
	metaQuantize       = "quantize"
	metaVectorsGen     = "vectors_gen"
	// metaEmbedCharsPrefix + "2026-09" counts the characters sent to the
	// embedder in that month, for the cost estimate.
	metaEmbedCharsPrefix = "embed_chars_"
)

// QuantizeInt8 and QuantizeNone are the storage forms of a vector.
const (
	QuantizeInt8 = "int8"
	QuantizeNone = "none"
)

// tombstoneReloadFraction is the share of dead entries past which the set
// is reloaded from disk instead of scanned around.
const tombstoneReloadFraction = 4

// PendingEmbed is one queued chunk with the text to embed.
type PendingEmbed struct {
	ChunkID  int64
	Heading  string
	Text     string
	Attempts int
}

// encodedVector is one vector as stored: int8 with a scale, or float32 LE
// with scale 1.
type encodedVector struct {
	scale float32
	vec   []byte
}

// encodeVector L2-normalises v and encodes it. int8 quantisation keeps the
// largest component at ±127 and records the scale that maps a quantised
// value back, so a dot product against a unit query is scale·Σ q·v.
func encodeVector(v []float32, quantize bool) encodedVector {
	unit := normalise(v)
	if !quantize {
		out := make([]byte, 4*len(unit))
		for i, x := range unit {
			binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
		}
		return encodedVector{scale: 1, vec: out}
	}
	var maxAbs float32
	for _, x := range unit {
		if a := float32(math.Abs(float64(x))); a > maxAbs {
			maxAbs = a
		}
	}
	scale := maxAbs / 127
	if scale == 0 {
		scale = 1
	}
	out := make([]byte, len(unit))
	for i, x := range unit {
		q := math.Round(float64(x / scale))
		out[i] = byte(int8(q))
	}
	return encodedVector{scale: scale, vec: out}
}

// normalise returns v scaled to unit length (a zero vector stays zero).
func normalise(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	out := make([]float32, len(v))
	if n == 0 {
		return out
	}
	n = math.Sqrt(n)
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// vectorSet is the flat in-memory copy of the vectors table. Entry i is
// chunkIDs[i], docIDs[i] and either q[i*dim:(i+1)*dim] scaled by scales[i]
// or f[i*dim:(i+1)*dim]. A tombstoned entry has docID 0 (SQLite row ids
// start at 1).
type vectorSet struct {
	dim       int
	quantized bool
	gen       int64
	q         []int8
	scales    []float32
	f         []float32
	chunkIDs  []uint32
	docIDs    []uint32
	dead      int
}

// vecHit is one scored entry.
type vecHit struct {
	chunkID uint32
	docID   uint32
	score   float32
}

func (vs *vectorSet) len() int { return len(vs.chunkIDs) }

// add appends an encoded vector. A vector of another size or form than the
// set's is refused, since the flat layout cannot hold it.
func (vs *vectorSet) add(chunkID, docID uint32, e encodedVector) bool {
	if vs.quantized {
		if len(e.vec) != vs.dim {
			return false
		}
		for _, b := range e.vec {
			vs.q = append(vs.q, int8(b))
		}
		vs.scales = append(vs.scales, e.scale)
	} else {
		if len(e.vec) != 4*vs.dim {
			return false
		}
		for i := 0; i < vs.dim; i++ {
			vs.f = append(vs.f, math.Float32frombits(binary.LittleEndian.Uint32(e.vec[4*i:])))
		}
	}
	vs.chunkIDs = append(vs.chunkIDs, chunkID)
	vs.docIDs = append(vs.docIDs, docID)
	return true
}

// dropDoc tombstones every entry of docID.
func (vs *vectorSet) dropDoc(docID uint32) {
	for i, d := range vs.docIDs {
		if d == docID {
			vs.docIDs[i] = 0
			vs.dead++
		}
	}
}

// stale reports whether enough entries are dead that a reload is cheaper
// than skipping them.
func (vs *vectorSet) stale() bool {
	return vs.dead > 0 && vs.dead*tombstoneReloadFraction > vs.len()
}

// cosineTopK scores every live entry whose document is in allowed (nil:
// every document) against the unit query q and returns the k best, best
// first, ties by chunk id.
func (vs *vectorSet) cosineTopK(q []float32, allowed map[uint32]bool, k int) []vecHit {
	if k <= 0 || len(q) != vs.dim || vs.dim == 0 {
		return nil
	}
	h := &topK{k: k}
	for i, doc := range vs.docIDs {
		if doc == 0 || allowed != nil && !allowed[doc] {
			continue
		}
		var dot float32
		if vs.quantized {
			row := vs.q[i*vs.dim : (i+1)*vs.dim]
			for j, x := range row {
				dot += float32(x) * q[j]
			}
			dot *= vs.scales[i]
		} else {
			row := vs.f[i*vs.dim : (i+1)*vs.dim]
			for j, x := range row {
				dot += x * q[j]
			}
		}
		h.offer(vecHit{chunkID: vs.chunkIDs[i], docID: doc, score: dot})
	}
	out := h.items
	sort.Slice(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].chunkID < out[b].chunkID
	})
	return out
}

// topK is a min-heap of the best k hits seen so far.
type topK struct {
	k     int
	items []vecHit
}

func (h *topK) Len() int { return len(h.items) }
func (h *topK) Less(i, j int) bool {
	// The worst hit sits at the root: lower score, or equal score and a
	// higher chunk id (the tie order the caller sorts by).
	if h.items[i].score != h.items[j].score {
		return h.items[i].score < h.items[j].score
	}
	return h.items[i].chunkID > h.items[j].chunkID
}
func (h *topK) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *topK) Push(x any)    { h.items = append(h.items, x.(vecHit)) }
func (h *topK) Pop() any      { n := len(h.items); x := h.items[n-1]; h.items = h.items[:n-1]; return x }
func (h *topK) offer(v vecHit) {
	if len(h.items) < h.k {
		heap.Push(h, v)
		return
	}
	worst := h.items[0]
	if v.score > worst.score || v.score == worst.score && v.chunkID < worst.chunkID {
		h.items[0] = v
		heap.Fix(h, 0)
	}
}

// vecChange is what one committed write did to the vectors, applied to
// the in-memory set while the writer still holds writeMu so generations
// are applied in commit order.
type vecChange struct {
	gen      int64
	dropAll  bool
	dropDocs []int64
	add      []vecEntry
}

type vecEntry struct {
	chunkID, docID int64
	enc            encodedVector
}

// vectorStore is the part of Store this file owns.
type vectorStore struct {
	// embedCap is index.max_chunks while an embedder is configured and 0
	// otherwise; 0 means chunks are never queued for embedding.
	embedCap atomic.Int64
	vecMu    sync.RWMutex
	vecs     *vectorSet
}

// SetEmbedCap turns chunk queueing on with the given max_chunks (0 turns
// it off). The Indexer sets it when an embedder is configured.
func (s *Store) SetEmbedCap(n int) { s.embedCap.Store(int64(n)) }

// writeVec is write for a transaction that changes vectors: fn fills ch
// and, once the transaction commits, ch is applied to the in-memory set.
func (s *Store) writeVec(ctx context.Context, fn func(tx *sql.Tx, ch *vecChange) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer tx.Rollback()
	var ch vecChange
	if err := fn(tx, &ch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if ch.gen != 0 {
		s.applyVecChange(ch)
	}
	return nil
}

func (s *Store) applyVecChange(ch vecChange) {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	vs := s.vecs
	if vs == nil {
		return
	}
	if ch.dropAll {
		s.vecs = nil
		return
	}
	for _, d := range ch.dropDocs {
		vs.dropDoc(uint32(d))
	}
	for _, e := range ch.add {
		if !vs.add(uint32(e.chunkID), uint32(e.docID), e.enc) {
			// A vector of another size: the set is no longer uniform,
			// reload from disk where the loader can judge every row.
			s.vecs = nil
			return
		}
	}
	vs.gen = ch.gen
	if vs.stale() {
		s.vecs = nil
	}
}

// bumpVectorsGenTx moves the on-disk generation and returns the new value.
func bumpVectorsGenTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var v string
	err := tx.QueryRowContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1
		RETURNING value`, metaVectorsGen).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	gen, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("index: vectors_gen %q: %w", v, err)
	}
	return gen, nil
}

func getMetaTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	return v, nil
}

func setMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// Meta reads one index_meta value, "" when absent.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	return getMetaTx(ctx, s.db, key)
}

// SetMeta writes one index_meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.write(ctx, func(tx *sql.Tx) error { return setMetaTx(ctx, tx, key, value) })
}

// dropDocVectorsTx removes the vectors and queue rows of document id's
// chunks and records the drop in ch when there were vectors. It runs
// before the chunks themselves are deleted.
func dropDocVectorsTx(ctx context.Context, tx *sql.Tx, id int64, ch *vecChange) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM vectors WHERE chunk_id IN (SELECT id FROM chunks WHERE doc_id = ?)`, id)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embed_pending WHERE chunk_id IN (SELECT id FROM chunks WHERE doc_id = ?)`, id); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		ch.dropDocs = append(ch.dropDocs, id)
		if ch.gen, err = bumpVectorsGenTx(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// dropAllVectorsTx empties vectors and the queue.
func dropAllVectorsTx(ctx context.Context, tx *sql.Tx, ch *vecChange) error {
	for _, q := range []string{`DELETE FROM vectors`, `DELETE FROM embed_pending`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	ch.dropAll = true
	var err error
	ch.gen, err = bumpVectorsGenTx(ctx, tx)
	return err
}

// queueChunksTx queues the chunks of document id that have no vector and
// are not queued, oldest first, as far as the cap allows (id 0: every
// document). It returns how many it queued.
func (s *Store) queueChunksTx(ctx context.Context, tx *sql.Tx, id int64) (int64, error) {
	limit := s.embedCap.Load()
	if limit <= 0 {
		return 0, nil
	}
	var have int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM vectors) + (SELECT count(*) FROM embed_pending)`).Scan(&have); err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	room := limit - have
	if room <= 0 {
		return 0, nil
	}
	where := ""
	args := []any{}
	if id != 0 {
		where = ` AND c.doc_id = ?`
		args = append(args, id)
	}
	args = append(args, room)
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embed_pending(chunk_id, attempts, next_at)
		SELECT c.id, 0, 0 FROM chunks c
		LEFT JOIN vectors v ON v.chunk_id = c.id
		LEFT JOIN embed_pending p ON p.chunk_id = c.id
		WHERE v.chunk_id IS NULL AND p.chunk_id IS NULL`+where+`
		ORDER BY c.id LIMIT ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// QueueEmbeds drops queue rows whose chunk is gone and queues every chunk
// that has no vector, up to the cap. It is the embed worker's reconcile:
// what an upsert queued is normally all there is, this catches a crash
// between the two, an index built before an embedder was configured, and
// a cap that was raised. It returns how many chunks it queued.
func (s *Store) QueueEmbeds(ctx context.Context) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM embed_pending WHERE chunk_id NOT IN (SELECT id FROM chunks)`); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		var err error
		n, err = s.queueChunksTx(ctx, tx, 0)
		return err
	})
	return n, err
}

// ResetEmbeddings switches the index to another model or storage form:
// every vector is dropped, the recorded dimension is forgotten, every
// chunk is queued again up to the cap, and chunks_fts is untouched.
func (s *Store) ResetEmbeddings(ctx context.Context, model, quantize string) error {
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		if err := dropAllVectorsTx(ctx, tx, ch); err != nil {
			return err
		}
		if err := setMetaTx(ctx, tx, metaEmbeddingModel, model); err != nil {
			return err
		}
		if err := setMetaTx(ctx, tx, metaQuantize, quantize); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM index_meta WHERE key = ?`, metaEmbeddingDim); err != nil {
			return fmt.Errorf("index: %w", err)
		}
		_, err := s.queueChunksTx(ctx, tx, 0)
		return err
	})
}

// DropAllVectors empties the vectors and the queue, keeping the recorded
// model.
func (s *Store) DropAllVectors(ctx context.Context) error {
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error { return dropAllVectorsTx(ctx, tx, ch) })
}

// PendingEmbeds returns up to limit queued chunks whose time has come,
// with their text, earliest due first.
func (s *Store) PendingEmbeds(ctx context.Context, limit int, now time.Time) ([]PendingEmbed, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.chunk_id, c.heading, c.text, p.attempts
		FROM embed_pending p JOIN chunks c ON c.id = p.chunk_id
		WHERE p.next_at <= ? ORDER BY p.next_at, p.chunk_id LIMIT ?`, now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	var out []PendingEmbed
	for rows.Next() {
		var p PendingEmbed
		if err := rows.Scan(&p.ChunkID, &p.Heading, &p.Text, &p.Attempts); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return out, nil
}

// PutVectors stores one vector per chunk id in the given form (QuantizeInt8
// or QuantizeNone), takes the chunks off the queue, records the dimension
// when none is recorded yet and adds chars to this month's count. A chunk
// deleted since it was fetched is skipped.
func (s *Store) PutVectors(ctx context.Context, model, quantize string, ids []int64, vecs [][]float32, chars int64) error {
	if len(ids) != len(vecs) {
		return fmt.Errorf("index: %d chunk ids for %d vectors", len(ids), len(vecs))
	}
	if len(ids) == 0 {
		return nil
	}
	dim := len(vecs[0])
	if dim == 0 {
		return errors.New("index: empty vector")
	}
	quantized := quantize != QuantizeNone
	return s.writeVec(ctx, func(tx *sql.Tx, ch *vecChange) error {
		recorded, err := getMetaTx(ctx, tx, metaEmbeddingDim)
		if err != nil {
			return err
		}
		if recorded == "" {
			if err := setMetaTx(ctx, tx, metaEmbeddingDim, strconv.Itoa(dim)); err != nil {
				return err
			}
		} else if recorded != strconv.Itoa(dim) {
			return fmt.Errorf("index: vectors have %d dimensions, the index records %s", dim, recorded)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO vectors(chunk_id, model, scale, vec)
			SELECT id, ?, ?, ? FROM chunks WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		defer stmt.Close()
		for i, id := range ids {
			if len(vecs[i]) != dim {
				return fmt.Errorf("index: vector %d has %d dimensions, vector 0 has %d", i, len(vecs[i]), dim)
			}
			enc := encodeVector(vecs[i], quantized)
			res, err := stmt.ExecContext(ctx, model, float64(enc.scale), enc.vec, id)
			if err != nil {
				return fmt.Errorf("index: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			var docID int64
			if err := tx.QueryRowContext(ctx, `SELECT doc_id FROM chunks WHERE id = ?`, id).Scan(&docID); err != nil {
				return fmt.Errorf("index: %w", err)
			}
			ch.add = append(ch.add, vecEntry{chunkID: id, docID: docID, enc: enc})
		}
		if err := markEmbeddedTx(ctx, tx, ids); err != nil {
			return err
		}
		if chars > 0 {
			if err := addEmbedCharsTx(ctx, tx, s.now(), chars); err != nil {
				return err
			}
		}
		ch.gen, err = bumpVectorsGenTx(ctx, tx)
		return err
	})
}

func markEmbeddedTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM embed_pending WHERE chunk_id = ?`)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	return nil
}

// MarkEmbedded takes chunks off the queue without storing a vector: what
// the worker does when it gives up on them.
func (s *Store) MarkEmbedded(ctx context.Context, ids []int64) error {
	return s.write(ctx, func(tx *sql.Tx) error { return markEmbeddedTx(ctx, tx, ids) })
}

// DeferEmbeds keeps chunks queued but not before nextAt, counting one more
// attempt on each.
func (s *Store) DeferEmbeds(ctx context.Context, ids []int64, nextAt time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `UPDATE embed_pending SET attempts = attempts + 1, next_at = ? WHERE chunk_id = ?`)
		if err != nil {
			return fmt.Errorf("index: %w", err)
		}
		defer stmt.Close()
		for _, id := range ids {
			if _, err := stmt.ExecContext(ctx, nextAt.UnixNano(), id); err != nil {
				return fmt.Errorf("index: %w", err)
			}
		}
		return nil
	})
}

// NextEmbedAt is the earliest next_at among queued chunks that are not due
// yet; zero when the queue is empty or everything is due now.
func (s *Store) NextEmbedAt(ctx context.Context, now time.Time) (time.Time, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT min(next_at) FROM embed_pending WHERE next_at > ?`, now.UnixNano()).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("index: %w", err)
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return time.Unix(0, at.Int64), nil
}

// VectorCount reports how many chunks have a vector.
func (s *Store) VectorCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM vectors`).Scan(&n); err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	return n, nil
}

// embedCharsKey is the index_meta key counting a month's embedded chars.
func embedCharsKey(at time.Time) string { return metaEmbedCharsPrefix + at.UTC().Format("2006-01") }

func addEmbedCharsTx(ctx context.Context, tx *sql.Tx, at time.Time, n int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + excluded.value`,
		embedCharsKey(at), strconv.FormatInt(n, 10))
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// AddEmbedChars adds n to the month's count of characters sent to the
// embedder (a search query, say). A read-only store counts nothing.
func (s *Store) AddEmbedChars(ctx context.Context, n int64) error {
	if s.readOnly || n <= 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error { return addEmbedCharsTx(ctx, tx, s.now(), n) })
}

// EmbedChars reports the characters sent to the embedder in the month of
// at.
func (s *Store) EmbedChars(ctx context.Context, at time.Time) (int64, error) {
	v, err := s.Meta(ctx, embedCharsKey(at))
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("index: %s: %w", embedCharsKey(at), err)
	}
	return n, nil
}

// vectors returns the in-memory set, loading it when it is missing or the
// on-disk generation moved on. The set is returned under a read lock the
// caller must release with the returned function.
func (s *Store) vectors(ctx context.Context) (*vectorSet, func(), error) {
	gen, err := s.Meta(ctx, metaVectorsGen)
	if err != nil {
		return nil, nil, err
	}
	want, _ := strconv.ParseInt(gen, 10, 64)
	// A set at a later generation than the one just read is one this
	// process wrote to after the read; it is never behind the disk.
	s.vecMu.RLock()
	if vs := s.vecs; vs != nil && vs.gen >= want {
		return vs, s.vecMu.RUnlock, nil
	}
	s.vecMu.RUnlock()
	s.vecMu.Lock()
	if vs := s.vecs; vs != nil && vs.gen >= want {
		s.vecMu.Unlock()
		s.vecMu.RLock()
		return vs, s.vecMu.RUnlock, nil
	}
	vs, err := s.loadVectors(ctx, want)
	if err != nil {
		s.vecMu.Unlock()
		return nil, nil, err
	}
	s.vecs = vs
	s.vecMu.Unlock()
	s.vecMu.RLock()
	return vs, s.vecMu.RUnlock, nil
}

// loadVectors reads the vectors table into a fresh set. The recorded
// dimension and storage form decide the layout; a row that does not fit
// (a leftover of another form) is left out.
func (s *Store) loadVectors(ctx context.Context, gen int64) (*vectorSet, error) {
	dimStr, err := s.Meta(ctx, metaEmbeddingDim)
	if err != nil {
		return nil, err
	}
	dim, _ := strconv.Atoi(dimStr)
	quant, err := s.Meta(ctx, metaQuantize)
	if err != nil {
		return nil, err
	}
	vs := &vectorSet{dim: dim, quantized: quant != QuantizeNone, gen: gen}
	rows, err := s.db.QueryContext(ctx, `SELECT v.chunk_id, c.doc_id, v.scale, v.vec
		FROM vectors v JOIN chunks c ON c.id = v.chunk_id ORDER BY v.chunk_id`)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var chunkID, docID int64
		var scale float64
		var vec []byte
		if err := rows.Scan(&chunkID, &docID, &scale, &vec); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		if vs.dim == 0 {
			// No dimension recorded (an index written before the probe
			// was recorded): take it from the first row.
			if vs.quantized {
				vs.dim = len(vec)
			} else {
				vs.dim = len(vec) / 4
			}
		}
		vs.add(uint32(chunkID), uint32(docID), encodedVector{scale: float32(scale), vec: vec})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return vs, nil
}
