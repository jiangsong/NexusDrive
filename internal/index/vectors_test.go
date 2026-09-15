package index

import (
	"context"
	"database/sql"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// chunkIDs returns the chunk ids of every document, in id order.
func chunkIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestSchemaV2AddsVectorTables(t *testing.T) {
	// A v1 database, laid down by the v1 migration alone, must come up at
	// v2 with its documents intact and the two new tables present.
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbName)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range migrations[0] {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO index_meta(key, value) VALUES('schema_version', '1')`,
		`PRAGMA user_version = 1`,
		`INSERT INTO documents(id, remote, remote_id, version, path, state, indexed_at) VALUES(1, 'd', 'r1', 'v1', '/a.md', 0, 1)`,
		`INSERT INTO chunks(id, doc_id, seq, start_off, end_off, text) VALUES(7, 1, 0, 0, 5, 'hello')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("user_version %d %v", version, err)
	}
	var recorded string
	if err := s.db.QueryRow(`SELECT value FROM index_meta WHERE key = 'schema_version'`).Scan(&recorded); err != nil || recorded != strconv.Itoa(schemaVersion) {
		t.Fatalf("schema_version %q %v", recorded, err)
	}
	for _, table := range []string{"vectors", "embed_pending"} {
		if !tableExists(t, s.db, table) {
			t.Fatalf("table %s missing after migration", table)
		}
	}
	if count(t, s.db, "chunks") != 1 || ftsCount(t, s.db, "hello") != 1 {
		t.Fatal("the migration lost the v1 chunks")
	}

	// Vectors follow their chunk: deleting the document cascades.
	ctx := context.Background()
	if err := s.PutVectors(ctx, "m", "int8", []int64{7}, [][]float32{{1, 0, 0, 0}}, 5); err != nil {
		t.Fatal(err)
	}
	if n, err := s.VectorCount(ctx); err != nil || n != 1 {
		t.Fatalf("vectors %d %v", n, err)
	}
	var model string
	var scale float64
	var vec []byte
	if err := s.db.QueryRow(`SELECT model, scale, vec FROM vectors WHERE chunk_id = 7`).Scan(&model, &scale, &vec); err != nil {
		t.Fatal(err)
	}
	if model != "m" || scale <= 0 || len(vec) != 4 {
		t.Fatalf("model %q scale %v vec %d bytes", model, scale, len(vec))
	}
	if dim, _ := s.Meta(ctx, metaEmbeddingDim); dim != "4" {
		t.Fatalf("embedding_dim %q, want 4 recorded by the first PutVectors", dim)
	}
	if err := s.DeleteDocument(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if count(t, s.db, "vectors") != 0 {
		t.Fatal("vectors survived their chunk")
	}
}

func TestEmbedPendingFollowsTheChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Without a cap nothing is queued: a keyword-only index must not grow
	// a queue nobody drains.
	seedDoc(t, s, "1", "/a.md", "alpha text")
	if count(t, s.db, "embed_pending") != 0 {
		t.Fatal("chunks were queued for embedding without an embedder")
	}
	s.SetEmbedCap(100)
	seedDoc(t, s, "2", "/b.md", "beta text")
	if count(t, s.db, "embed_pending") != 1 {
		t.Fatalf("embed_pending %d after an upsert with a cap", count(t, s.db, "embed_pending"))
	}
	// Re-extracting a document replaces its chunks and their queue rows.
	seedDoc(t, s, "2", "/b.md", "beta text revised")
	if count(t, s.db, "embed_pending") != 1 {
		t.Fatalf("embed_pending %d after a re-extraction", count(t, s.db, "embed_pending"))
	}
	ids := chunkIDs(t, s)
	pend, err := s.PendingEmbeds(ctx, 10, s.now())
	if err != nil || len(pend) != 1 || pend[0].ChunkID != ids[len(ids)-1] || pend[0].Text != "beta text revised" {
		t.Fatalf("%+v %v", pend, err)
	}
	// QueueEmbeds fills the gap left by the document indexed before the cap.
	n, err := s.QueueEmbeds(ctx)
	if err != nil || n != 1 {
		t.Fatalf("queued %d %v", n, err)
	}
	if count(t, s.db, "embed_pending") != 2 {
		t.Fatalf("embed_pending %d after QueueEmbeds", count(t, s.db, "embed_pending"))
	}
	// A failed document drops its chunks and their queue rows.
	if err := s.MarkFailed(ctx, Document{Remote: "d", RemoteID: "2", Version: "v2", Path: "/b.md"}, nil); err != nil {
		t.Fatal(err)
	}
	if count(t, s.db, "embed_pending") != 1 {
		t.Fatalf("embed_pending %d after MarkFailed", count(t, s.db, "embed_pending"))
	}
	// Deferred rows wait for their time; MarkEmbedded removes them.
	pend, _ = s.PendingEmbeds(ctx, 10, s.now())
	if err := s.DeferEmbeds(ctx, []int64{pend[0].ChunkID}, s.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if later, _ := s.PendingEmbeds(ctx, 10, s.now()); len(later) != 0 {
		t.Fatalf("a deferred chunk was offered early: %+v", later)
	}
	if later, _ := s.PendingEmbeds(ctx, 10, s.now().Add(2*time.Hour)); len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("deferred chunk after its time: %+v", later)
	}
	if err := s.MarkEmbedded(ctx, []int64{pend[0].ChunkID}); err != nil {
		t.Fatal(err)
	}
	if count(t, s.db, "embed_pending") != 0 {
		t.Fatal("MarkEmbedded left the row")
	}
	st, err := s.Stats(ctx)
	if err != nil || st.EmbedPending != 0 || st.Vectors != 0 {
		t.Fatalf("%+v %v", st, err)
	}
}

// unit returns v scaled to length one.
func unit(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

func TestInt8QuantisationKeepsCosineOrder(t *testing.T) {
	const dim = 64
	r := rand.New(rand.NewPCG(7, 11))
	random := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(r.NormFloat64())
		}
		return unit(v)
	}
	q := random()
	// Candidates sit at well separated angles from q: mostly q plus a
	// little noise, down to mostly noise. The float order is by alpha.
	const n = 8
	var exact, quant vectorSet
	exact.dim, quant.dim = dim, dim
	quant.quantized = true
	for i := 0; i < n; i++ {
		alpha := float32(n-i) / float32(n+1)
		noise := random()
		v := make([]float32, dim)
		for j := range v {
			v[j] = alpha*q[j] + (1-alpha)*noise[j]
		}
		v = unit(v)
		exact.add(uint32(i+1), uint32(100+i), encodeVector(v, false))
		quant.add(uint32(i+1), uint32(100+i), encodeVector(v, true))
	}
	want := exact.cosineTopK(q, nil, n)
	got := quant.cosineTopK(q, nil, n)
	if len(want) != n || len(got) != n {
		t.Fatalf("%d and %d hits", len(want), len(got))
	}
	for i := range want {
		if want[i].chunkID != got[i].chunkID || want[i].docID != got[i].docID {
			t.Fatalf("rank %d: float32 says chunk %d, int8 says chunk %d", i, want[i].chunkID, got[i].chunkID)
		}
		if math.Abs(float64(want[i].score-got[i].score)) > 0.02 {
			t.Fatalf("rank %d: cosine %v vs %v after quantisation", i, want[i].score, got[i].score)
		}
		if i > 0 && got[i].score > got[i-1].score {
			t.Fatalf("scores not descending: %v", got)
		}
	}
	if want[0].chunkID != 1 {
		t.Fatalf("the most aligned candidate should rank first, got %d", want[0].chunkID)
	}
	// The int8 encoding is one byte per dimension plus a scale; the
	// float encoding is four.
	if e := encodeVector(q, true); len(e.vec) != dim || e.scale <= 0 {
		t.Fatalf("int8 encoding: %d bytes, scale %v", len(e.vec), e.scale)
	}
	if e := encodeVector(q, false); len(e.vec) != 4*dim {
		t.Fatalf("float32 encoding: %d bytes", len(e.vec))
	}
	// A doc filter and a tombstone both hide entries.
	only := quant.cosineTopK(q, map[uint32]bool{105: true, 107: true}, 10)
	if len(only) != 2 || only[0].docID != 105 || only[1].docID != 107 {
		t.Fatalf("filtered: %+v", only)
	}
	quant.dropDoc(100)
	if top := quant.cosineTopK(q, nil, 1); len(top) != 1 || top[0].docID != 101 {
		t.Fatalf("after dropping doc 100: %+v", top)
	}
}
