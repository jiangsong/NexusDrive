package meta

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"cloudfs/internal/provider"
)

func TestCopyBindingReusesOnlyOriginalInode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity, err := s.Identity(ctx)
	if err != nil || identity == "" {
		t.Fatalf("identity=%q %v", identity, err)
	}
	n := Node{ParentIno: RootIno, Name: "dest", Kind: provider.KindFile, Remote: "ali", RemoteID: "local-copy", Version: "copy-v", Size: 7}
	var wg sync.WaitGroup
	results := make(chan Node, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.BindCopy(ctx, "copy-id", n)
			if err != nil {
				t.Error(err)
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	var ino uint64
	for got := range results {
		if ino == 0 {
			ino = got.Ino
		}
		if got.Ino != ino {
			t.Fatal("copy allocated multiple inodes")
		}
	}
	if ino == 0 {
		t.Fatal("no copy inode")
	}
	s.Close()
	s, err = Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Identity(ctx); err != nil || got != identity {
		t.Fatalf("reopen identity=%q %v", got, err)
	}
	if err := s.Remove(ctx, ino); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Insert(ctx, Node{ParentIno: RootIno, Name: "dest", Kind: provider.KindFile, Size: 99})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindCopy(ctx, "copy-id", n); !errors.Is(err, ErrCopyTargetChanged) {
		t.Fatalf("recreated copy target: %v", err)
	}
	if got, err := s.Get(ctx, replacement.Ino); err != nil || got.Size != 99 {
		t.Fatalf("overwrote replacement: %+v %v", got, err)
	}
}

func TestCopyBindingDoesNotAdoptExistingName(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Insert(ctx, Node{ParentIno: RootIno, Name: "dest", Kind: provider.KindFile}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindCopy(ctx, "another-copy", Node{ParentIno: RootIno, Name: "dest", RemoteID: "copy", Version: "v", Kind: provider.KindFile}); !errors.Is(err, ErrExists) {
		t.Fatalf("adopted existing node: %v", err)
	}
}
