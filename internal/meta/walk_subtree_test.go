package meta

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// walkFixture builds /a, /a/b.txt, /a/c, /a/c/d.txt, /e.txt and returns the
// inode of /a.
func walkFixture(t *testing.T, s *Store) uint64 {
	t.Helper()
	ctx := context.Background()
	a, err := s.Upsert(ctx, dir(RootIno, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, file(a.Ino, "b.txt", 1)); err != nil {
		t.Fatal(err)
	}
	c, err := s.Upsert(ctx, dir(a.Ino, "c"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, file(c.Ino, "d.txt", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, file(RootIno, "e.txt", 3)); err != nil {
		t.Fatal(err)
	}
	return a.Ino
}

func TestWalkSubtreeVisitsEveryDescendantWithItsPath(t *testing.T) {
	s, _ := openTest(t)
	walkFixture(t, s)
	var got []string
	err := s.WalkSubtree(context.Background(), RootIno, "/", func(n Node, p string) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/a", "/a/b.txt", "/a/c", "/a/c/d.txt", "/e.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("visited %q, want %q", got, want)
	}

	// A walk rooted below the root joins names onto the base it is given.
	got = nil
	a, err := s.Lookup(context.Background(), RootIno, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WalkSubtree(context.Background(), a.Ino, "/a", func(n Node, p string) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a/b.txt", "/a/c", "/a/c/d.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visited %q, want %q", got, want)
	}
}

func TestWalkSubtreeSkipDir(t *testing.T) {
	s, _ := openTest(t)
	walkFixture(t, s)
	var got []string
	err := s.WalkSubtree(context.Background(), RootIno, "/", func(n Node, p string) error {
		got = append(got, p)
		if p == "/a" {
			return SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a", "/e.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visited %q, want %q", got, want)
	}

	// Any other error stops the walk and comes back to the caller as is.
	boom := errors.New("boom")
	got = nil
	err = s.WalkSubtree(context.Background(), RootIno, "/", func(n Node, p string) error {
		got = append(got, p)
		if p == "/a/c" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if want := []string{"/a", "/a/b.txt", "/a/c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visited %q before the error, want %q", got, want)
	}
}

func TestWalkSubtreePagesLargeDirectories(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const count = 1200
	big, err := s.Upsert(ctx, dir(RootIno, "big"))
	if err != nil {
		t.Fatal(err)
	}
	children := make([]Node, count)
	for i := range children {
		children[i] = file(big.Ino, fmt.Sprintf("f%05d", i), 1)
	}
	if err := s.PutDir(ctx, big.Ino, children, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	var order []string
	err = s.WalkSubtree(ctx, big.Ino, "/big", func(n Node, p string) error {
		seen[p]++
		order = append(order, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != count || len(seen) != count {
		t.Fatalf("visited %d paths (%d distinct), want %d", len(order), len(seen), count)
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("%s visited %d times", p, n)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Fatalf("not in name order at %d: %q then %q", i, order[i-1], order[i])
		}
	}
}
