package meta

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDirListingKilledCollectorCannotPublishPartialTree(t *testing.T) {
	if p := os.Getenv("CLOUDFS_LISTING_TEST_CHILD"); p != "" {
		s, err := Open(p, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root, err := s.Get(context.Background(), RootIno)
		if err != nil {
			t.Fatal(err)
		}
		l, err := s.BeginDirListing(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		for batch := range 4 {
			nodes := make([]Node, DirListingBatch)
			for i := range nodes {
				nodes[i] = file(0, fmt.Sprintf("uncommitted-%04d", batch*DirListingBatch+i), 1)
			}
			if err := l.Append(context.Background(), nodes); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println("listing-staged")
		var wait [1]byte
		_, _ = os.Stdin.Read(wait[:])
		t.Fatal("collector was not killed")
	}
	p := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutDir(context.Background(), RootIno, []Node{file(0, "original", 1)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	before, err := s.DirState(context.Background(), RootIno)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDirListingKilledCollectorCannotPublishPartialTree$")
	cmd.Env = append(os.Environ(), "CLOUDFS_LISTING_TEST_CHILD="+p)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "listing-staged" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("child did not reach staging boundary: %v", scanner.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child was not terminated")
	}
	waited = true
	s, err = Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if state, err := s.DirState(context.Background(), RootIno); err != nil || state != before {
		t.Fatalf("crashed collection changed freshness: %+v %v", state, err)
	}
	if _, err := s.Lookup(context.Background(), RootIno, "original"); err != nil {
		t.Fatal("crashed collection deleted original")
	}
	if _, err := s.Lookup(context.Background(), RootIno, "uncommitted-0000"); !errors.Is(err, ErrNotFound) {
		t.Fatal("crashed collection published a partial snapshot")
	}
	// A subsequent ordinary refresh is not blocked by the lost generation.
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(context.Background(), []Node{file(0, "recovered", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(context.Background(), time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(context.Background(), RootIno, "recovered"); err != nil {
		t.Fatal(err)
	}
}
