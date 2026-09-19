package meta

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func TestExclusiveInsertDoesNotOverwriteConcurrentCreator(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	var wins atomic.Int64
	var winner atomic.Uint64
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Insert(context.Background(), Node{ParentIno: RootIno, Name: "unique", Kind: provider.KindFile, Size: int64(i)})
			if err == nil {
				wins.Add(1)
				winner.Store(uint64(i))
			} else if !errors.Is(err, ErrExists) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	n, err := s.Lookup(context.Background(), RootIno, "unique")
	if err != nil || wins.Load() != 1 || uint64(n.Size) != winner.Load() {
		t.Fatalf("lost creator: wins=%d node=%+v err=%v", wins.Load(), n, err)
	}
}

// TestInsertCompleteDirIsFreshAndKeepsRacingChildren: a directory this
// machine just created is known to be empty, so it is born with a complete
// listing — and because the listing state is written with the node, a child
// inserted the moment it appears is not pruned by any listing bookkeeping.
func TestInsertCompleteDirIsFreshAndKeepsRacingChildren(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	dir, err := s.InsertCompleteDir(ctx, Node{ParentIno: RootIno, Name: "fresh", Kind: provider.KindDir, Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.DirState(ctx, dir.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Complete || !st.ListedAt.Equal(now) {
		t.Fatalf("new directory is not born complete: %+v", st)
	}
	if !st.Fresh(now.Add(time.Second), time.Minute) {
		t.Fatalf("new directory is not fresh: %+v", st)
	}
	child, err := s.Insert(ctx, Node{ParentIno: dir.Ino, Name: "child", Kind: provider.KindFile, Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, child.Ino); err != nil {
		t.Fatalf("child inserted right after the directory is gone: %v", err)
	}
	if _, err := s.InsertCompleteDir(ctx, Node{ParentIno: RootIno, Name: "fresh", Kind: provider.KindDir}); !errors.Is(err, ErrExists) {
		t.Fatalf("second insert of the same name: %v", err)
	}
}
