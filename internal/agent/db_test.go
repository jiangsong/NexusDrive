package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesTheSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Owner() {
		t.Fatal("the first opener must own agent.db")
	}
	for _, table := range []string{"principals", "sessions", "audit", "meta"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d", v)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&stored); err != nil || stored != fmt.Sprint(schemaVersion) {
		t.Fatalf("meta schema_version = %q, %v", stored, err)
	}
}

// TestSchemaV2HasSessionOpsAndDeliveries: a fresh store lands on v2 with the
// two tables phase two builds on, and each takes a row with only the
// columns its writer knows about.
func TestSchemaV2HasSessionOpsAndDeliveries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	for _, table := range []string{"session_ops", "trigger_deliveries"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	for _, index := range []string{"session_ops_path", "session_ops_session", "trigger_pending"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing: %v", index, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO session_ops(session_id, audit_id, op, path, pre_state)
		VALUES ('s1', 1, 'create', '/a.txt', 'absent')`); err != nil {
		t.Fatalf("session_ops refused a row: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO trigger_deliveries(rule, path, kind, origin, first_seen, due_at)
		VALUES ('r1', '/a.txt', 'create', 'kernel', 1, 1)`); err != nil {
		t.Fatalf("trigger_deliveries refused a row: %v", err)
	}
	// One pending delivery per (rule, path): a second event for the same file
	// coalesces into the queued one instead of running the action twice.
	if _, err := s.db.Exec(`INSERT INTO trigger_deliveries(rule, path, kind, origin, first_seen, due_at)
		VALUES ('r1', '/a.txt', 'write', 'kernel', 2, 2)`); err == nil {
		t.Fatal("two pending deliveries for the same rule and path were accepted")
	}
	if _, err := s.db.Exec(`UPDATE trigger_deliveries SET state = 'done' WHERE rule = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO trigger_deliveries(rule, path, kind, origin, first_seen, due_at)
		VALUES ('r1', '/a.txt', 'write', 'kernel', 3, 3)`); err != nil {
		t.Fatalf("a finished delivery must not block the next pending one: %v", err)
	}
}

// TestOlderDatabaseMigratesToV2: a v1 agent.db left by the previous build
// gains the new tables in place and keeps its audit trail.
func TestOlderDatabaseMigratesToV2(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range schemaV1 {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta(k, v) VALUES('schema_version', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (1, 'stat', '[]', '{}', 'ok')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a v1 database must open and migrate: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after migration, want %d", v, schemaVersion)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&stored); err != nil || stored != fmt.Sprint(schemaVersion) {
		t.Fatalf("meta schema_version = %q, %v", stored, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the audit row did not survive the migration: n=%d err=%v", n, err)
	}
	if _, err := s.db.Exec(`INSERT INTO session_ops(session_id, audit_id, op, path, pre_state)
		VALUES ('s1', 1, 'create', '/a.txt', 'absent')`); err != nil {
		t.Fatalf("session_ops missing after migration: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO trigger_deliveries(rule, path, kind, origin, first_seen, due_at)
		VALUES ('r1', '/a.txt', 'create', 'kernel', 1, 1)`); err != nil {
		t.Fatalf("trigger_deliveries missing after migration: %v", err)
	}
}

func TestSecondOpenIsNotOwnerButCanWrite(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Owner() {
		t.Fatal("two owners")
	}
	if _, err := b.db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (1, 'stat', '[]', '{}', 'ok')`); err != nil {
		t.Fatalf("a non-owner stdio process must still append audit rows: %v", err)
	}
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the owner must see the non-owner's row: n=%d err=%v", n, err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want a newer-schema refusal, got %v", err)
	}
	if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("read-only open must refuse a newer schema too, got %v", err)
	}
}

func TestOpenReadOnlyMissingIsNotExist(t *testing.T) {
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenReadOnlySeesTheOwnersRows(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (1, 'stat', '[]', '{}', 'ok')`); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Owner() {
		t.Fatal("a read-only store must never own the lock")
	}
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := r.db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (2, 'stat', '[]', '{}', 'ok')`); err == nil {
		t.Fatal("a read-only store accepted a write")
	}
}

func TestWatchDoesNotBlockWhenNobodyReads(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, stop := s.Watch()
	defer stop()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.publish(Event{Kind: "audit"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow watcher")
	}
}

func TestWatchDeliversTheNewestEventsAndStopUnsubscribes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ch, stop := s.Watch()
	// Overfill the buffer: the oldest events are dropped, the newest kept.
	for i := 0; i < watchBuffer+10; i++ {
		s.publish(Event{Kind: "audit", Audit: &AuditRow{ID: int64(i)}})
	}
	first := <-ch
	if first.Audit == nil || first.Audit.ID != 10 {
		t.Fatalf("first surviving event = %+v, want id 10", first)
	}
	stop()
	if _, ok := <-ch; ok {
		// Draining is fine; the channel must eventually close.
		for range ch {
		}
	}
	// A publish after stop must not panic on the closed channel.
	s.publish(Event{Kind: "session"})
}

// TestSchemaV3IsAdditiveAndRepeatable: a v2 file gains the agent-first
// columns and tables in place, keeps its rows, and a crash between a step
// and the version write (simulated by re-running the step list on a file
// already carrying it) does not trip over ADD COLUMN.
func TestSchemaV3IsAdditiveAndRepeatable(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][]string{schemaV1, schemaV2} {
		for _, q := range step {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_ops(session_id, audit_id, op, path, pre_state) VALUES ('s1', 1, 'create', '/a.txt', 'absent')`); err != nil {
		t.Fatal(err)
	}
	// Half of v3 already applied, as a crash mid-migration leaves it.
	if _, err := db.Exec(schemaV3[0]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a half-migrated v2 database must open: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	for _, table := range []string{"changes", "read_heat"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	for _, col := range []struct{ table, name string }{{"audit", "tokens_out"}, {"session_ops", "ts"}, {"sessions", "last_change_seen"}} {
		var n int
		if err := s.db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM pragma_table_info('%s') WHERE name = ?`, col.table), col.name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s.%s missing: %v", col.table, col.name, err)
		}
	}
	var ts int64
	if err := s.db.QueryRow(`SELECT ts FROM session_ops WHERE session_id = 's1'`).Scan(&ts); err != nil || ts != 0 {
		t.Fatalf("the v2 row did not survive with a default ts: %d %v", ts, err)
	}
	id, err := s.AppendAudit(context.Background(), AuditRow{Tool: "read_text", TokensOut: 1234, Result: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Audit(context.Background(), AuditQuery{})
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].TokensOut != 1234 {
		t.Fatalf("tokens_out did not round-trip: %+v %v", rows, err)
	}
}

// TestSchemaV3MigratesV2AndBackfillsTs: a v2 database's session_ops rows
// get their time from the audit row each was recorded beside, so history
// can place them; a row whose audit row is gone stays at zero; and the
// backfill is repeatable, since it only touches rows still at zero.
func TestSchemaV3MigratesV2AndBackfillsTs(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][]string{schemaV1, schemaV2} {
		for _, q := range step {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	const when = int64(1758000000000000000)
	if _, err := db.Exec(`INSERT INTO audit(id, ts, principal_id, session_id, transport, tool, paths, args, bytes_in, bytes_out, result, error, duration_ms) VALUES (7, ?, 'p', 's1', 'stdio', 'write_file', '[]', '{}', 0, 0, 'ok', '', 1)`, when); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_ops(session_id, audit_id, op, path, pre_state) VALUES ('s1', 7, 'create', '/a.txt', 'absent'), ('s1', 99, 'create', '/orphan.txt', 'absent')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ts int64
	if err := s.db.QueryRow(`SELECT ts FROM session_ops WHERE path = '/a.txt'`).Scan(&ts); err != nil || ts != when {
		t.Fatalf("ts was not backfilled from audit: %d %v", ts, err)
	}
	if err := s.db.QueryRow(`SELECT ts FROM session_ops WHERE path = '/orphan.txt'`).Scan(&ts); err != nil || ts != 0 {
		t.Fatalf("an orphan row got a time: %d %v", ts, err)
	}
	// Running the step again changes nothing.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range schemaV3 {
		if err := applyStep(tx, q); err != nil {
			t.Fatalf("repeat %q: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT ts FROM session_ops WHERE path = '/a.txt'`).Scan(&ts); err != nil || ts != when {
		t.Fatalf("the repeat changed the time: %d %v", ts, err)
	}
}
