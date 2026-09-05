package meta

import (
	"path/filepath"
	"testing"
)

func TestOfflineOpenAndCloseDoNotMaintainUnrelatedIndexes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(p, Options{NoIndexMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.indexStop != nil {
		t.Fatal("offline store started index worker")
	}
	if _, err := s.Insert(t.Context(), file(RootIno, "unrelated", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(p, Options{NoIndexMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM name_index_pending`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("close maintained unrelated index: %d %v", pending, err)
	}
	if hits, err := s.Search(t.Context(), "unrelated", 10); err != nil || len(hits) != 1 {
		t.Fatalf("disabled indexing hid pending node: %+v %v", hits, err)
	}
}
