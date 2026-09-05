package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchLiteralUnicodeAndPendingRename(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, name := range []string{"报告_2026%.md", "ÄPFEL.txt", `a"b.txt`, "ordinary.md"} {
		if _, err := s.Upsert(ctx, file(RootIno, name, 1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, flush := range []bool{false, true} {
		if flush {
			if err := s.FlushIndex(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for q, want := range map[string]string{"报告": "报告_2026%.md", "_": "报告_2026%.md", "%": "报告_2026%.md", "äP": "ÄPFEL.txt", "äpf": "ÄPFEL.txt", `a"b`: `a"b.txt`} {
			got, err := s.Search(ctx, q, 10)
			if err != nil || len(got) != 1 || got[0].Name != want {
				t.Fatalf("flush=%v query=%q: %+v, %v", flush, q, got, err)
			}
		}
	}
	n, err := s.Upsert(ctx, file(RootIno, "old-pending", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, n.Ino, RootIno, "new-current"); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	for q, count := range map[string]int{"old": 0, "ol": 0, "new": 1, "ne": 1} {
		got, err := s.Search(ctx, q, 10)
		if err != nil || len(got) != count {
			t.Fatalf("query=%q: %+v, %v", q, got, err)
		}
	}
}

func TestSearchPathsMoveAndScopeBeforeLimit(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	work, err := s.Upsert(ctx, dir(RootIno, "work"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.Upsert(ctx, dir(work.Ino, "src"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.go", "b.go"} {
		if _, err := s.Upsert(ctx, file(src.Ino, name, 1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"work/src", "work/sr", "src/", "/work/src/"} {
		got, err := s.Search(ctx, q, 10)
		want := 3
		if strings.HasSuffix(q, "/") {
			want = 2
		}
		if err != nil || len(got) != want {
			t.Fatalf("query=%q: %+v %v", q, got, err)
		}
	}
	if err := s.Rename(ctx, work.Ino, RootIno, "moved"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Search(ctx, "work/src", 10); err != nil || len(got) != 0 {
		t.Fatalf("old path: %+v %v", got, err)
	}
	if got, err := s.Search(ctx, "moved/src/", 10); err != nil || len(got) != 2 {
		t.Fatalf("new path: %+v %v", got, err)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.Upsert(ctx, file(RootIno, fmt.Sprintf("hidden-%03d.go", i), 1)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SearchWithin(ctx, ".go", []string{"/moved/src"}, 1)
	if err != nil || len(got) != 1 || got[0].Path != "/moved/src/a.go" {
		t.Fatalf("scope: %+v %v", got, err)
	}
	for _, roots := range [][]string{{}, {"/move"}, {"/MOVED"}} {
		got, err := s.SearchWithin(ctx, ".go", roots, 10)
		if err != nil || len(got) != 0 {
			t.Fatalf("scope %v: %+v %v", roots, got, err)
		}
	}
	if err := s.Remove(ctx, work.Ino); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Search(ctx, "moved/", 10); err != nil || len(got) != 0 {
		t.Fatalf("deleted path: %+v %v", got, err)
	}
}

func TestSearchMigrationRepairsStaleIndexAndBackfillsShortTerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range migrations[:7] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=7;
INSERT INTO nodes(ino,parent_ino,name,kind) VALUES(1,1,'',1),(2,1,'current',0);
INSERT INTO name_index(rowid,name,path,ino) VALUES(2,'obsolete','',2);
INSERT INTO name_indexed(ino,name) VALUES(2,'obsolete');`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, flush := range []bool{false, true} {
		if flush {
			if err := s.FlushIndex(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		for q, count := range map[string]int{"cu": 1, "current": 1, "obsolete": 0} {
			got, err := s.Search(context.Background(), q, 10)
			if err != nil || len(got) != count {
				t.Fatalf("flush=%v query=%s: %+v %v", flush, q, got, err)
			}
		}
	}
}

func TestShortSearchUsesPostingsAt100KNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("100K-node query plan and timing")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "collection"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE seq(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM seq WHERE i<100000)
INSERT INTO nodes(parent_ino,name,kind) SELECT ?,printf('entry-%06d',i),0 FROM seq`, d.Ino)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("insert 100K nodes with synchronous short postings: %s", time.Since(start))
	stmt, args := searchSQL("99", nil, 20)
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !strings.Contains(plan.String(), "VIRTUAL TABLE INDEX") || strings.Contains(plan.String(), "SCAN n\n") || strings.Contains(stmt, "name_index_pending") {
		t.Fatalf("not indexed: %s", plan.String())
	}
	start = time.Now()
	got, err := s.Search(ctx, "99", 20)
	if err != nil || len(got) != 20 {
		t.Fatalf("search: %+v %v", got, err)
	}
	t.Logf("short query at 100K nodes: %s; plan:\n%s", time.Since(start), plan.String())
	start = time.Now()
	if err := s.Rename(ctx, d.Ino, RootIno, "renamed"); err != nil {
		t.Fatal(err)
	}
	t.Logf("rename parent of 100K nodes (no descendant index rewrite): %s", time.Since(start))
	got, err = s.Search(ctx, "99", 20)
	if err != nil || len(got) != 20 {
		t.Fatalf("after rename: %+v %v", got, err)
	}
	for _, r := range got {
		if !strings.HasPrefix(r.Path, "/renamed/") {
			t.Fatalf("stale path: %+v", r)
		}
	}
}

func TestSearchTriggerRollbackAndValidation(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	abort := errors.New("injected rollback")
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO nodes(parent_ino,name,kind) VALUES(1,'rollback-name',0)`); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	for _, table := range []string{"short_name_index", "name_index_pending"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after rollback: %d %v", table, count, err)
		}
	}
	for _, q := range []string{"a\x00b", string([]byte{0xff}), strings.Repeat("a", 4097)} {
		if _, err := s.Search(ctx, q, 10); err == nil {
			t.Fatalf("invalid query accepted: %q", q)
		}
	}
	for _, root := range []string{"relative", "/a/../b", "/a/", "/a\x00"} {
		if _, err := s.SearchWithin(ctx, "ab", []string{root}, 10); err == nil {
			t.Fatalf("invalid root accepted: %q", root)
		}
	}
}

func TestSearchSnapshotDuringRename(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	n, err := s.Upsert(ctx, file(RootIno, "report-a", 1))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if err := s.Rename(ctx, n.Ino, RootIno, fmt.Sprintf("report-%d", i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	// Always join before cleanup, including assertion failures.
	defer func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 100; i++ {
		got, err := s.Search(ctx, "report", 10)
		if err != nil || len(got) != 1 || got[0].Path != "/"+got[0].Name {
			t.Fatalf("mixed snapshot: %+v %v", got, err)
		}
	}
}
