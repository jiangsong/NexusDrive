package meta

import (
	"context"
	"testing"
)

// TestHotReadQueriesArePrepared: readdir, the delta refresher and retention
// run the same four SELECTs over and over. Issued through the database handle
// directly, SQLite parses the text again on every call; through the statement
// cache it is parsed once for the life of the store.
func TestHotReadQueriesArePrepared(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, dir(RootIno, "hot"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Upsert(ctx, file(d.Ino, "a.txt", 1))
	if err != nil {
		t.Fatal(err)
	}
	f.Remote, f.RemoteID = "r", "rid-1"
	if _, err := s.Upsert(ctx, f); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"Children", func() error { _, err := s.Children(ctx, d.Ino); return err }},
		{"ByRemoteID", func() error { _, err := s.ByRemoteID(ctx, "r", "rid-1"); return err }},
		{"Aliases", func() error { _, err := s.Aliases(ctx, "r", "rid-1"); return err }},
		{"UncommittedDirs", func() error { _, err := s.UncommittedDirs(ctx); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := s.prepares.Load()
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if got := s.prepares.Load() - before; got != 1 {
				t.Fatalf("first %s prepared %d statements, want exactly 1", tc.name, got)
			}
			const n = 50
			before = s.prepares.Load()
			for i := 0; i < n; i++ {
				if err := tc.call(); err != nil {
					t.Fatal(err)
				}
			}
			if got := s.prepares.Load() - before; got != 0 {
				t.Fatalf("%d further %s calls prepared %d statements, want 0", n, tc.name, got)
			}
		})
	}
}
