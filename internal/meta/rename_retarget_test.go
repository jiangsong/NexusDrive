package meta

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// On a path-addressed backend a rename changes the address of every
// descendant, so the new name and the new ids are one fact about the tree.
// Committing them in two transactions publishes a state that was never true:
// the directory answers to its new name while its children still point at a
// path that no longer exists. A reader in that window gets ENOENT on files
// that are present, and a crash there leaves it that way for good.
func TestRenameAndRetargetPublishNoIntermediateState(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj"))
	if err != nil {
		t.Fatal(err)
	}
	var last uint64
	for i := 0; i < 300; i++ {
		n, err := s.Upsert(ctx, withID(file(proj.Ino, fmt.Sprintf("f%03d.txt", i), 4), fmt.Sprintf("/proj/f%03d.txt", i)))
		if err != nil {
			t.Fatal(err)
		}
		last = n.Ino
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var torn string
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d, err := s.Get(ctx, proj.Ino)
			if err != nil {
				continue
			}
			child, err := s.Get(ctx, last)
			if err != nil {
				continue
			}
			if d.Name == "proj2" && !strings.HasPrefix(child.RemoteID, "/proj2/") {
				mu.Lock()
				if torn == "" {
					torn = fmt.Sprintf("directory renamed to %q while its child still answers to %q", d.Name, child.RemoteID)
				}
				mu.Unlock()
				return
			}
		}
	}()

	if err := s.RenameAndRetarget(ctx, proj.Ino, RootIno, "proj2", "/proj2", true); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if torn != "" {
		t.Fatal(torn)
	}
	child, err := s.Get(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(child.RemoteID, "/proj2/") {
		t.Fatalf("the subtree was not retargeted: %q", child.RemoteID)
	}
}
