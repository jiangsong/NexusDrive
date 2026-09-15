package meta

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIncompleteDirsPagesByInoAndCoverageCountsTheRoot(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var dirs []Node
	for i := 0; i < 5; i++ {
		d, err := s.Upsert(ctx, dir(RootIno, fmt.Sprintf("d%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	if _, err := s.Upsert(ctx, file(dirs[0].Ino, "f", 1)); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 3} { // an empty published listing makes a directory complete
		l, err := s.BeginDirListing(ctx, dirs[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, time.Minute, nil); err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
	var got []uint64
	for after := uint64(0); ; {
		page, err := s.IncompleteDirs(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 2 {
			t.Fatalf("page of %d rows exceeds the limit", len(page))
		}
		for _, n := range page {
			got = append(got, n.Ino)
		}
		after = page[len(page)-1].Ino
	}
	if want := []uint64{RootIno, dirs[0].Ino, dirs[2].Ino, dirs[4].Ino}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("incomplete = %v, want %v", got, want)
	}
	cov, err := s.Coverage(ctx)
	if err != nil || cov.Known != 6 || cov.Listed != 2 {
		t.Fatalf("%+v %v", cov, err)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.LastCrawl.IsZero() {
		t.Fatalf("LastCrawl not derived from dir_state: %+v %v", st, err)
	}
	// A directory a delta marked stale is incomplete again, so the next
	// sweep picks it up without any bookkeeping of its own.
	if err := s.Invalidate(ctx, dirs[1].Ino); err != nil {
		t.Fatal(err)
	}
	page, err := s.IncompleteDirs(ctx, dirs[0].Ino, 1)
	if err != nil || len(page) != 1 || page[0].Ino != dirs[1].Ino {
		t.Fatalf("stale directory not swept: %+v %v", page, err)
	}
}

func TestLastCrawlIsZeroBeforeAnyListing(t *testing.T) {
	s, _ := openTest(t)
	st, err := s.Stats(context.Background())
	if err != nil || !st.LastCrawl.IsZero() {
		t.Fatalf("%+v %v", st, err)
	}
}
