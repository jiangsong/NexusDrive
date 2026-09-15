package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func appendRow(t *testing.T, s *Store, row AuditRow) int64 {
	t.Helper()
	id, err := s.AppendAudit(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuditWriteFailureIsCountedNotReturnedToTheTool(t *testing.T) {
	s := openStore(t)
	s.setAppendFault(func() error { return errors.New("disk full") })
	if _, err := s.AppendAudit(context.Background(), AuditRow{Tool: "stat", Result: "ok"}); err == nil {
		t.Fatal("fault not surfaced to caller")
	}
	if s.AuditWriteFailures() != 1 {
		t.Fatalf("failures = %d", s.AuditWriteFailures())
	}
	s.setAppendFault(nil)
	appendRow(t, s, AuditRow{Tool: "stat", Result: "ok"})
	if s.AuditWriteFailures() != 1 {
		t.Fatalf("a successful append must not change the count: %d", s.AuditWriteFailures())
	}
	rows, _, err := s.Audit(context.Background(), AuditQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v: the failed append must not leave a row", len(rows), err)
	}
}

func TestReadOnlyStoreRefusesAndCountsAppends(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.AppendAudit(context.Background(), AuditRow{Tool: "stat", Result: "ok"}); err == nil {
		t.Fatal("a read-only store accepted an audit row")
	}
	if ro.AuditWriteFailures() != 1 {
		t.Fatalf("failures = %d", ro.AuditWriteFailures())
	}
}

func TestAppendAuditFillsDefaultsAndPublishes(t *testing.T) {
	s := openStore(t)
	fixed := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	events, stop := s.Watch()
	defer stop()
	id := appendRow(t, s, AuditRow{Tool: "write_file", Result: "ok", SessionID: "s1", PrincipalID: "p1", Transport: "stdio", BytesIn: 12, DurationMS: 3})
	if id <= 0 {
		t.Fatalf("id = %d", id)
	}
	select {
	case e := <-events:
		if e.Kind != "audit" || e.Audit == nil || e.Audit.ID != id || e.Audit.Tool != "write_file" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit event published")
	}
	rows, _, err := s.Audit(context.Background(), AuditQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if !r.TS.Equal(fixed) {
		t.Fatalf("a zero TS must become the store clock: %v", r.TS)
	}
	if r.Paths == nil || len(r.Paths) != 0 || string(r.Args) != "{}" {
		t.Fatalf("nil paths/args must round-trip as empty, got paths=%#v args=%s", r.Paths, r.Args)
	}
	if r.SessionID != "s1" || r.PrincipalID != "p1" || r.Transport != "stdio" || r.BytesIn != 12 || r.DurationMS != 3 {
		t.Fatalf("row %+v", r)
	}
	if b, _ := json.Marshal(r); !json.Valid(b) {
		t.Fatalf("row does not marshal: %s", b)
	}
}

func TestAuditPagesByIDAndFilters(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rows := []AuditRow{
		{TS: base.Add(1 * time.Minute), Tool: "stat", Result: "ok", SessionID: "a", Paths: []string{"/a"}, Args: json.RawMessage(`{"path":"/a"}`)},
		{TS: base.Add(2 * time.Minute), Tool: "delete", Result: "denied", SessionID: "a", Paths: []string{"/gd/x"}},
		{TS: base.Add(3 * time.Minute), Tool: "write_file", Result: "ok", SessionID: "b", Paths: []string{"/w"}},
		{TS: base.Add(4 * time.Minute), Tool: "delete", Result: "denied", SessionID: "b", Paths: []string{"/gd/y"}},
		{TS: base.Add(5 * time.Minute), Tool: "read_text", Result: "error", SessionID: "b", Error: "not found"},
	}
	var ids []int64
	for _, r := range rows {
		ids = append(ids, appendRow(t, s, r))
	}
	ctx := context.Background()

	page1, next, err := s.Audit(ctx, AuditQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != ids[4] || page1[1].ID != ids[3] || next == "" {
		t.Fatalf("page1 = %+v next=%q", page1, next)
	}
	page2, next2, err := s.Audit(ctx, AuditQuery{Limit: 2, Cursor: next})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[1] || next2 == "" {
		t.Fatalf("page2 = %+v next=%q", page2, next2)
	}
	page3, next3, err := s.Audit(ctx, AuditQuery{Limit: 2, Cursor: next2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 || page3[0].ID != ids[0] || next3 != "" {
		t.Fatalf("page3 = %+v next=%q", page3, next3)
	}
	if _, _, err := s.Audit(ctx, AuditQuery{Cursor: "junk"}); err == nil {
		t.Fatal("a garbage cursor must be refused")
	}

	denied, _, err := s.Audit(ctx, AuditQuery{Result: "denied"})
	if err != nil || len(denied) != 2 || denied[0].Paths[0] != "/gd/y" || denied[1].Paths[0] != "/gd/x" {
		t.Fatalf("denied = %+v err=%v", denied, err)
	}
	bySession, _, err := s.Audit(ctx, AuditQuery{Session: "a"})
	if err != nil || len(bySession) != 2 {
		t.Fatalf("session a = %+v err=%v", bySession, err)
	}
	byTool, _, err := s.Audit(ctx, AuditQuery{Tool: "delete", Session: "b"})
	if err != nil || len(byTool) != 1 || byTool[0].ID != ids[3] {
		t.Fatalf("delete in b = %+v err=%v", byTool, err)
	}
	since, _, err := s.Audit(ctx, AuditQuery{Since: base.Add(4 * time.Minute)})
	if err != nil || len(since) != 2 {
		t.Fatalf("since = %+v err=%v", since, err)
	}
	if string(page3[0].Args) != `{"path":"/a"}` || page3[0].Paths[0] != "/a" {
		t.Fatalf("args/paths lost: %+v", page3[0])
	}
	if page1[0].Error != "not found" {
		t.Fatalf("error lost: %+v", page1[0])
	}

	forB, err := s.AuditForSession(ctx, "b", 2)
	if err != nil || len(forB) != 2 || forB[0].ID != ids[4] || forB[1].ID != ids[3] {
		t.Fatalf("session b tail = %+v err=%v", forB, err)
	}
	empty, _, err := s.Audit(ctx, AuditQuery{Session: "nobody"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("an empty page must be a non-nil empty slice: %#v err=%v", empty, err)
	}
}

func TestPurgeAuditDropsOldRows(t *testing.T) {
	s := openStore(t)
	appendRow(t, s, AuditRow{TS: time.Unix(100, 0), Tool: "stat", Result: "ok"})
	keep := appendRow(t, s, AuditRow{TS: time.Now(), Tool: "stat", Result: "ok"})
	n, err := s.PurgeAudit(context.Background(), time.Unix(200, 0))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, err=%v", n, err)
	}
	rows, _, err := s.Audit(context.Background(), AuditQuery{})
	if err != nil || len(rows) != 1 || rows[0].ID != keep {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
}

func TestAuditRetentionRunsOnlyInTheOwner(t *testing.T) {
	dir := t.TempDir()
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if owner.Owner() == other.Owner() {
		t.Fatal("exactly one of the two must own the lock")
	}
	old := appendRow(t, other, AuditRow{TS: time.Now().Add(-48 * time.Hour), Tool: "stat", Result: "ok"})
	fresh := appendRow(t, other, AuditRow{TS: time.Now(), Tool: "stat", Result: "ok"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan struct{})
	go func() {
		other.RunAuditRetention(ctx, 24*time.Hour, time.Millisecond)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a non-owner must not run retention")
	}
	if rows, _, _ := owner.Audit(ctx, AuditQuery{}); len(rows) != 2 {
		t.Fatalf("the non-owner purged rows: %+v", rows)
	}

	done := make(chan struct{})
	go func() {
		owner.RunAuditRetention(ctx, 24*time.Hour, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, _, err := owner.Audit(context.Background(), AuditQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].ID == fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old row %d still present: %+v", old, rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retention loop did not stop with its context")
	}
	if n, err := owner.PurgeAudit(context.Background(), time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("second purge %d err=%v", n, err)
	}
}

func TestRunAuditRetentionKeepsEverythingWhenDisabled(t *testing.T) {
	s := openStore(t)
	appendRow(t, s, AuditRow{TS: time.Unix(100, 0), Tool: "stat", Result: "ok"})
	returned := make(chan struct{})
	go func() {
		s.RunAuditRetention(context.Background(), 0, time.Millisecond)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("retain=0 must return at once instead of looping")
	}
	if rows, _, _ := s.Audit(context.Background(), AuditQuery{}); len(rows) != 1 {
		t.Fatalf("retain=0 must keep rows: %+v", rows)
	}
}
