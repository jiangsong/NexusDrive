package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesSchemaV1(t *testing.T) {
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
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&stored); err != nil || stored != "1" {
		t.Fatalf("meta schema_version = %q, %v", stored, err)
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
