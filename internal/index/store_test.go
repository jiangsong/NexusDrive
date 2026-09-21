package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/textract"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func chunksOf(text string) []textract.Chunk {
	return textract.ChunkDoc(textract.Doc{Text: text, OffsetKind: "file"}, textract.KindText, textract.DefaultChunkOptions())
}

func ftsCount(t *testing.T, db *sql.DB, phrase string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, MatchQuery(phrase)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIndexSchemaAndFTSTriggers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.UpsertDocument(ctx, Document{Remote: "demo", RemoteID: "r1", Version: "v1", Path: "/work/a.md", Kind: "markdown"},
		"cloudfs mounts drives", chunksOf("cloudfs mounts drives"))
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	if n := ftsCount(t, s.db, "mounts"); n != 1 {
		t.Fatalf("fts rows %d", n)
	}
	id2, err := s.UpsertDocument(ctx, Document{Remote: "demo", RemoteID: "r1", Version: "v2", Path: "/work/a.md", Kind: "markdown"},
		"replaced body", chunksOf("replaced body"))
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("upsert changed the id: %d -> %d", id, id2)
	}
	if n := ftsCount(t, s.db, "mounts"); n != 0 {
		t.Fatal("old chunks survived an upsert")
	}
	if n := ftsCount(t, s.db, "replaced"); n != 1 {
		t.Fatalf("new chunks not searchable: %d", n)
	}
	d, ok, err := s.DocumentByRemote(ctx, "demo", "r1")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if d.Version != "v2" || d.State != DocOK || len(d.TextHash) != 32 || d.IndexedAt.IsZero() {
		t.Fatalf("%+v", d)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version %d %v", version, err)
	}
	var recorded string
	if err := s.db.QueryRow(`SELECT value FROM index_meta WHERE key = 'schema_version'`).Scan(&recorded); err != nil || recorded != strconv.Itoa(schemaVersion) {
		t.Fatalf("index_meta schema_version %q %v", recorded, err)
	}
	if out, err := s.IntegrityCheck(ctx); err != nil || out != "ok" {
		t.Fatalf("integrity %q %v", out, err)
	}
}

func TestDeleteDocumentDropsItsChunksFromFTS(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a"}, "needle in text", chunksOf("needle in text"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "2", Version: "v", Path: "/b"}, "needle again", chunksOf("needle again")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDocument(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := ftsCount(t, s.db, "needle"); n != 1 {
		t.Fatalf("fts rows after delete %d", n)
	}
	if n, err := s.DeleteMissing(ctx, map[int64]bool{}); err != nil || n != 1 {
		t.Fatalf("DeleteMissing %d %v", n, err)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.DocsOK != 0 || st.Chunks != 0 || st.TextBytes != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if n := ftsCount(t, s.db, "needle"); n != 0 {
		t.Fatalf("fts rows after DeleteMissing %d", n)
	}
}

func TestRenamePrefixKeepsIndexedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/old/x/a.md"}, "t", chunksOf("t")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "2", Version: "v", Path: "/oldish/b.md"}, "t", chunksOf("t")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "3", Version: "v", Path: "/old"}, "t", chunksOf("t")); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.DocumentByRemote(ctx, "d", "1")
	time.Sleep(2 * time.Millisecond)
	if n, err := s.RenamePrefix(ctx, "/old", "/new"); err != nil || n != 2 {
		t.Fatalf("renamed %d %v", n, err)
	}
	after, _, _ := s.DocumentByRemote(ctx, "d", "1")
	other, _, _ := s.DocumentByRemote(ctx, "d", "2")
	exact, _, _ := s.DocumentByRemote(ctx, "d", "3")
	if after.Path != "/new/x/a.md" || !after.IndexedAt.Equal(before.IndexedAt) || other.Path != "/oldish/b.md" || exact.Path != "/new" {
		t.Fatalf("%+v %+v %+v", after, other, exact)
	}
	// Hits map back through (remote, remote_id): the chunks did not move,
	// and the document now answers at the new path.
	if d, ok, _ := s.DocumentByPath(ctx, "/new/x/a.md"); !ok || d.ID != before.ID {
		t.Fatalf("lookup by new path: %v %+v", ok, d)
	}
	if _, ok, _ := s.DocumentByPath(ctx, "/old/x/a.md"); ok {
		t.Fatal("old path still resolves")
	}
}

func TestRenamePrefixTreatsGlobCharactersLiterally(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, p := range []string{"/a[1]/x.txt", "/a1/x.txt", "/a?/y.txt"} {
		if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: fmt.Sprint(i), Version: "v", Path: p}, "t", chunksOf("t")); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.RenamePrefix(ctx, "/a[1]", "/b"); err != nil || n != 1 {
		t.Fatalf("renamed %d %v", n, err)
	}
	if d, _, _ := s.DocumentByRemote(ctx, "d", "0"); d.Path != "/b/x.txt" {
		t.Fatalf("%+v", d)
	}
	if d, _, _ := s.DocumentByRemote(ctx, "d", "1"); d.Path != "/a1/x.txt" {
		t.Fatalf("%+v", d)
	}
	if n, err := s.RenamePrefix(ctx, "/a?", "/c"); err != nil || n != 1 {
		t.Fatalf("renamed %d %v", n, err)
	}
}

func TestSetPathAndTouchVersionLeaveChunksAlone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v1", Path: "/a"}, "stable text", chunksOf("stable text"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPath(ctx, id, "/b"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchVersion(ctx, id, "v2"); err != nil {
		t.Fatal(err)
	}
	d, _, _ := s.DocumentByID(ctx, id)
	if d.Path != "/b" || d.Version != "v2" || d.State != DocOK {
		t.Fatalf("%+v", d)
	}
	if n := ftsCount(t, s.db, "stable"); n != 1 {
		t.Fatalf("fts rows %d", n)
	}
	if err := s.TouchVersion(ctx, id+100, "v3"); !errors.Is(err, ErrNotIndexed) {
		t.Fatal(err)
	}
	if err := s.SetPath(ctx, id+100, "/zzz"); !errors.Is(err, ErrNotIndexed) {
		t.Fatal(err)
	}
}

func TestIdentityChangeResetsTheIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if reset, err := s.EnsureIdentity(ctx, "meta-A"); err != nil || reset {
		t.Fatal(reset, err)
	}
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a"}, "t", chunksOf("t")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRule(ctx, Rule{Path: "/a", Source: "ui"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, 7, "/a", PendingChange); err != nil {
		t.Fatal(err)
	}
	if reset, _ := s.EnsureIdentity(ctx, "meta-A"); reset {
		t.Fatal("same identity reset")
	}
	if reset, _ := s.EnsureIdentity(ctx, "meta-B"); !reset {
		t.Fatal("new identity kept stale inodes")
	}
	if st, _ := s.Stats(ctx); st.DocsOK != 0 || st.Chunks != 0 || st.Pending != 0 {
		t.Fatalf("%+v", st)
	}
	if rules, _ := s.Rules(ctx); len(rules) != 1 {
		t.Fatalf("rules did not survive the reset: %+v", rules)
	}
	if id, _ := s.Identity(ctx); id != "meta-B" {
		t.Fatalf("identity %q", id)
	}
}

func TestConfigRulesCannotBeRemoved(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SyncConfigRules(ctx, []Rule{{Path: "/work", Source: "config", Include: []string{"*.md"}, MaxFileSize: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRule(ctx, Rule{Path: "/notes", Source: "ui"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRule(ctx, Rule{Path: "/work"}); !errors.Is(err, ErrConfigRule) {
		t.Fatalf("config rule overridden: %v", err)
	}
	if err := s.RemoveRule(ctx, "/work"); !errors.Is(err, ErrConfigRule) {
		t.Fatal(err)
	}
	rules, err := s.Rules(ctx)
	if err != nil || len(rules) != 2 {
		t.Fatalf("%+v %v", rules, err)
	}
	if rules[0].Path != "/notes" || rules[0].Source != "ui" || rules[0].Include == nil || rules[1].Include[0] != "*.md" || rules[1].MaxFileSize != 1<<20 {
		t.Fatalf("%+v", rules)
	}
	if err := s.RemoveRule(ctx, "/notes"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRule(ctx, "/notes"); !errors.Is(err, ErrNoRule) {
		t.Fatal(err)
	}
	// A new configuration drops the rules it no longer lists.
	if err := s.SyncConfigRules(ctx, []Rule{{Path: "/docs"}}); err != nil {
		t.Fatal(err)
	}
	rules, _ = s.Rules(ctx)
	if len(rules) != 1 || rules[0].Path != "/docs" || rules[0].Source != "config" {
		t.Fatalf("%+v", rules)
	}
}

func TestPendingQueueDedupesByInode(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Enqueue(ctx, 1, "/a", PendingReconcile); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, 2, "/b", PendingReconcile); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, 1, "/a-renamed", PendingChange); err != nil {
		t.Fatal(err)
	}
	items, err := s.Pending(ctx, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("%+v %v", items, err)
	}
	if items[0].Ino != 1 || items[0].Path != "/a-renamed" || items[0].Reason != PendingChange || items[1].Ino != 2 {
		t.Fatalf("%+v", items)
	}
	if err := s.Dequeue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Dequeue(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.Pending(ctx, 1); len(items) != 1 || items[0].Ino != 2 {
		t.Fatalf("%+v", items)
	}
	if st, _ := s.Stats(ctx); st.Pending != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestTextPagesThroughTheExtractedText(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	body := strings.Repeat("0123456789", 100)
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a.txt", Kind: "text"}, body, chunksOf(body)); err != nil {
		t.Fatal(err)
	}
	p1, err := s.Text(ctx, "/a.txt", 0, 600)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Text(ctx, "/a.txt", p1.NextOffset, 600)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Text+p2.Text != body || !p2.EOF || p1.EOF || p1.Kind != "text" || p1.Version != "v" || p2.Version != "v" || p2.NextOffset != int64(len(body)) {
		t.Fatalf("%d %d %v", len(p1.Text), len(p2.Text), p2.EOF)
	}
	if _, err := s.Text(ctx, "/missing", 0, 10); !errors.Is(err, ErrNotIndexed) {
		t.Fatal(err)
	}
	tail, err := s.Text(ctx, "/a.txt", int64(len(body))+5, 10)
	if err != nil || !tail.EOF || tail.Text != "" || tail.Version != "v" {
		t.Fatalf("%+v %v", tail, err)
	}
}

func TestTextPagesEndOnRuneBoundaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	body := strings.Repeat("中文", 50) // 300 bytes, 100 runes
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/c.txt", Kind: "text"}, body, chunksOf(body)); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	var off int64
	for pages := 0; ; pages++ {
		p, err := s.Text(ctx, "/c.txt", off, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(p.Text, "文") && !strings.HasSuffix(p.Text, "中") {
			t.Fatalf("page %d cut a rune: %q", pages, p.Text)
		}
		got.WriteString(p.Text)
		off = p.NextOffset
		if p.EOF {
			break
		}
		if pages > 10 {
			t.Fatal("no progress")
		}
	}
	if got.String() != body {
		t.Fatal("pages do not reassemble the text")
	}
	if p, _ := s.Text(ctx, "/c.txt", 0, 1); p.Text != "中" || p.NextOffset != 3 {
		t.Fatalf("a max under one rune must still advance: %+v", p)
	}
}

func TestFailedDocumentsPageAndRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tick := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	for i := 1; i <= 3; i++ {
		d := Document{Remote: "d", RemoteID: fmt.Sprint(i), Version: "v", Path: fmt.Sprintf("/f/%d.pdf", i), Ino: uint64(10 + i), Kind: "pdf"}
		if err := s.MarkFailed(ctx, d, errors.New("garbled")); err != nil {
			t.Fatal(err)
		}
	}
	page1, cursor, err := s.Failed(ctx, "", 2)
	if err != nil || len(page1) != 2 || cursor == "" {
		t.Fatalf("%+v %q %v", page1, cursor, err)
	}
	if page1[0].Path != "/f/3.pdf" || page1[1].Path != "/f/2.pdf" || page1[0].Error != "garbled" || page1[0].Kind != "pdf" {
		t.Fatalf("%+v", page1)
	}
	page2, cursor2, err := s.Failed(ctx, cursor, 2)
	if err != nil || len(page2) != 1 || cursor2 != "" || page2[0].Path != "/f/1.pdf" {
		t.Fatalf("%+v %q %v", page2, cursor2, err)
	}
	if _, _, err := s.Failed(ctx, "not-a-cursor!", 2); err == nil {
		t.Fatal("bad cursor accepted")
	}
	if st, _ := s.Stats(ctx); st.DocsFailed != 3 || st.DocsOK != 0 {
		t.Fatalf("%+v", st)
	}
	if n, err := s.RetryFailed(ctx, "/f/1.pdf"); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := s.RetryFailed(ctx, ""); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	st, _ := s.Stats(ctx)
	if st.DocsDirty != 3 || st.DocsFailed != 0 || st.Pending != 3 {
		t.Fatalf("%+v", st)
	}
	items, _ := s.Pending(ctx, 10)
	if len(items) != 3 || items[0].Reason != PendingRetry {
		t.Fatalf("%+v", items)
	}
	// A failure after a successful extraction hides the stale chunks.
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v2", Path: "/f/1.pdf", Kind: "pdf"}, "recovered text", chunksOf("recovered text")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, Document{Remote: "d", RemoteID: "1", Version: "v3", Path: "/f/1.pdf", Kind: "pdf"}, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	if n := ftsCount(t, s.db, "recovered"); n != 0 {
		t.Fatalf("stale chunks after MarkFailed: %d", n)
	}
	d, _, _ := s.DocumentByRemote(ctx, "d", "1")
	if d.State != DocFailed || d.Error != "timeout" || d.Version != "v3" {
		t.Fatalf("%+v", d)
	}
}

func TestNewerIndexSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenStore(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema opened: %v", err)
	}
	if _, err := OpenStoreReadOnly(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema opened read-only: %v", err)
	}
}

func TestReadOnlyStoreReadsButNeverWrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenStoreReadOnly(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing index: %v", err)
	}
	w, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	if _, err := w.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a"}, "shared", chunksOf("shared")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenStoreReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Owner() || !w.Owner() {
		t.Fatal("ownership")
	}
	if d, ok, err := r.DocumentByPath(ctx, "/a"); err != nil || !ok || d.RemoteID != "1" {
		t.Fatalf("%+v %v %v", d, ok, err)
	}
	if st, err := r.Stats(ctx); err != nil || st.DocsOK != 1 || st.Chunks != 1 || st.TextBytes != int64(len("shared")) {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := r.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "2", Version: "v", Path: "/b"}, "x", nil); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err := r.Enqueue(ctx, 1, "/a", PendingChange); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.db")); err != nil {
		t.Fatal(err)
	}
}

func TestSecondOpenerIsNotTheOwner(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !a.Owner() || b.Owner() {
		t.Fatalf("owner a=%v b=%v", a.Owner(), b.Owner())
	}
	// The non-owner may still read and queue work for the owner.
	if err := b.Enqueue(context.Background(), 5, "/x", PendingChange); err != nil {
		t.Fatal(err)
	}
	if items, _ := a.Pending(context.Background(), 1); len(items) != 1 || items[0].Ino != 5 {
		t.Fatalf("%+v", items)
	}
}

func TestDocumentsUnderCountsBySubtree(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, p := range []string{"/work/a.md", "/work/sub/b.md", "/works/c.md", "/work"} {
		if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: fmt.Sprint(i), Version: "v", Path: p}, "t", chunksOf("t")); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.DocumentsUnder(ctx, "/work"); n != 3 {
		t.Fatalf("/work %d", n)
	}
	if n, _ := s.DocumentsUnder(ctx, "/"); n != 4 {
		t.Fatalf("/ %d", n)
	}
	if n, _ := s.DocumentsUnder(ctx, "/nothing"); n != 0 {
		t.Fatalf("/nothing %d", n)
	}
}

func TestMatchQueryQuotesEveryWord(t *testing.T) {
	if got := MatchQuery(`  hello "world" OR NOT `); got != `"hello" """world""" "OR" "NOT"` {
		t.Fatalf("%q", got)
	}
	if got := MatchQuery("   "); got != "" {
		t.Fatalf("%q", got)
	}
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a"}, "alpha OR beta", chunksOf("alpha OR beta")); err != nil {
		t.Fatal(err)
	}
	if n := ftsCount(t, s.db, "alpha OR beta"); n != 1 {
		t.Fatalf("operators not literal: %d", n)
	}
	if n := ftsCount(t, s.db, "alpha gamma"); n != 0 {
		t.Fatalf("words not ANDed: %d", n)
	}
}

func TestBuiltinRulesFollowTheConfigurationAndCannotBeRemoved(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SyncBuiltinRules(ctx, []Rule{{Path: "/work/.agent/memory", Include: []string{"**/*.md"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRule(ctx, Rule{Path: "/work/.agent/memory"}); !errors.Is(err, ErrBuiltinRule) {
		t.Fatalf("builtin rule overridden: %v", err)
	}
	if err := s.RemoveRule(ctx, "/work/.agent/memory"); !errors.Is(err, ErrBuiltinRule) {
		t.Fatalf("builtin rule removed: %v", err)
	}
	rules, err := s.Rules(ctx)
	if err != nil || len(rules) != 1 || rules[0].Source != SourceBuiltin || rules[0].Include[0] != "**/*.md" {
		t.Fatalf("%+v %v", rules, err)
	}
	// A configuration rule at the same path wins over the derived one, and
	// a builtin sync with nothing drops the rule (memory.root unset).
	if err := s.SyncConfigRules(ctx, []Rule{{Path: "/work/.agent/memory", Include: []string{"*.txt"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncBuiltinRules(ctx, []Rule{{Path: "/work/.agent/memory", Include: []string{"**/*.md"}}}); err != nil {
		t.Fatal(err)
	}
	rules, _ = s.Rules(ctx)
	if len(rules) != 1 || rules[0].Source != SourceConfig || rules[0].Include[0] != "*.txt" {
		t.Fatalf("config must win over builtin: %+v", rules)
	}
	if err := s.SyncConfigRules(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncBuiltinRules(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if rules, _ = s.Rules(ctx); len(rules) != 0 {
		t.Fatalf("rules after both syncs emptied: %+v", rules)
	}
}
