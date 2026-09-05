package meta

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedWide builds a directory of `dirs` subdirectories, each holding `each`
// files, all under one parent. It is the shape a path query expands over.
func seedWide(t *testing.T, s *Store, parent uint64, dirs, each int) {
	t.Helper()
	ctx := context.Background()
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`WITH RECURSIVE seq(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM seq WHERE i<?)
INSERT INTO nodes(parent_ino,name,kind) SELECT ?,printf('branch-%04d',i),1 FROM seq`, dirs-1, parent); err != nil {
			return err
		}
		_, err := tx.Exec(`WITH RECURSIVE seq(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM seq WHERE i<?)
INSERT INTO nodes(parent_ino,name,kind)
SELECT b.ino,printf('leaf-%05d.txt',seq.i),0 FROM nodes b, seq
WHERE b.parent_ino=? AND b.kind=1`, each-1, parent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWidePathQueryStopsAtItsBudget covers the case a path query can be given
// by accident: an anchor that matches thousands of directories, each holding
// thousands of files. Expanding every subtree and sorting the result would
// take as long as the tree is large, with the caller waiting.
//
// The budget caps the matches collected, and the answer says it is partial.
// Reporting that is the point: "nothing else matched" and "we stopped looking"
// are different answers, and a caller that cannot tell them apart will show
// the first when it means the second.
func TestWidePathQueryStopsAtItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("wide-tree expansion budget")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	root, err := s.Upsert(ctx, dir(RootIno, "wide"))
	if err != nil {
		t.Fatal(err)
	}
	// 60 000 leaves under 200 branches: every one of them matches "wide/".
	seedWide(t, s, root.Ino, 200, 300)

	// Fold the index backlog first so what is measured below is the query and
	// not the one-time merge the first long query would otherwise pay for.
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	report, err := s.SearchReport(ctx, "wide/", nil, 25)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 25 {
		t.Fatalf("got %d results, want the requested 25", len(report.Results))
	}
	if report.Complete {
		t.Fatal("a query matching 60K nodes with a 20K budget reported a complete answer")
	}
	for _, r := range report.Results {
		if !strings.Contains(r.Path, "/wide/") {
			t.Fatalf("result %q does not contain the query", r.Path)
		}
	}
	t.Logf("wide path query over 60K matching nodes: %s", elapsed)
	// Measured while writing this: ~100 ms bounded against ~460 ms with the
	// budget removed, and the unbounded figure grows with the subtree while the
	// bounded one does not. The assertion is deliberately loose — it exists to
	// catch a regression back to walking the whole subtree, not to police a
	// machine's speed.
	if elapsed > 5*time.Second {
		t.Fatalf("bounded wide query took %s; the expansion is no longer being cut", elapsed)
	}

	// Every returned row must be distinct: overlapping anchor subtrees repeat
	// nodes in the lazy expansion, and the caller is what removes them.
	seen := map[uint64]bool{}
	for _, r := range report.Results {
		if seen[r.Ino] {
			t.Fatalf("inode %d was returned twice", r.Ino)
		}
		seen[r.Ino] = true
	}
}

// TestOrdinaryQueryStillReportsCompleteness guards the budget against being so
// low that normal answers start claiming to be partial.
func TestOrdinaryQueryStillReportsCompleteness(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "work"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Upsert(ctx, file(d.Ino, fmt.Sprintf("report-%d.txt", i), 4)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.SearchReport(ctx, "report", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Results) != 5 {
		t.Fatalf("ordinary query returned %d results complete=%v", len(report.Results), report.Complete)
	}
	// A query that matches nothing is a complete answer, not a truncated one.
	report, err = s.SearchReport(ctx, "absent-name", nil, 100)
	if err != nil || !report.Complete || len(report.Results) != 0 {
		t.Fatalf("empty answer: %d results complete=%v err=%v", len(report.Results), report.Complete, err)
	}
}

// TestLongQueryFoldsTheIndexBacklogInsteadOfScanningIt covers what a bulk
// directory refresh leaves behind: tens of thousands of names queued for the
// FTS index. A long query has to consult that queue so a just-listed file is
// not invisible, and scanning it on every query makes search cost grow with
// how recently the tree was walked.
func TestLongQueryFoldsTheIndexBacklogInsteadOfScanningIt(t *testing.T) {
	if testing.Short() {
		t.Skip("index backlog")
	}
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "bulk"))
	if err != nil {
		t.Fatal(err)
	}
	// The insert trigger queues every name; the background indexer is stopped
	// for this store, so the backlog is exactly what a refresh would leave.
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE seq(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM seq WHERE i<20000)
INSERT INTO nodes(parent_ino,name,kind) SELECT ?,printf('queued-%06d.txt',i),0 FROM seq`, d.Ino)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !s.pendingIndexExceeds(ctx, searchPendingBudget) {
		t.Fatal("the backlog was not created")
	}

	report, err := s.SearchReport(ctx, "queued-000123", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Name != "queued-000123.txt" {
		t.Fatalf("a queued name must still be findable, got %+v", report.Results)
	}
	if s.pendingIndexExceeds(ctx, searchPendingBudget) {
		t.Fatal("the backlog survived the query; every later query would scan it again")
	}

	// A name queued after the fold is still visible without another fold.
	if _, err := s.Upsert(ctx, file(d.Ino, "arrived-later.txt", 4)); err != nil {
		t.Fatal(err)
	}
	report, err = s.SearchReport(ctx, "arrived-later", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("a name queued after the fold is invisible: %+v", report.Results)
	}
}

// TestShortQueriesDoNotTouchTheBacklog: one and two character queries are
// answered from the synchronously maintained postings, so they must neither
// consult nor fold the pending table.
func TestShortQueriesDoNotTouchTheBacklog(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "short"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE seq(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM seq WHERE i<?)
INSERT INTO nodes(parent_ino,name,kind) SELECT ?,printf('zz%06d',i),0 FROM seq`, searchPendingBudget+100, d.Ino)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stmt, _ := searchSQL("zz", nil, 10, matchBudget(10))
	if strings.Contains(stmt, "name_index_pending") {
		t.Fatal("a short query consulted the deferred index; its postings are written synchronously")
	}
	report, err := s.SearchReport(ctx, "zz", nil, 10)
	if err != nil || len(report.Results) != 10 {
		t.Fatalf("short query returned %d results: %v", len(report.Results), err)
	}
	if !s.pendingIndexExceeds(ctx, searchPendingBudget) {
		t.Fatal("a short query folded the backlog it never reads")
	}
}
