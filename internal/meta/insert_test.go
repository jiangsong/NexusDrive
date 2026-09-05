package meta

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

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
