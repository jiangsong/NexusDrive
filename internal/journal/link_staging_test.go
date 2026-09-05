package journal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"cloudfs/internal/provider"
)

func TestLinkedStagingSharesInodeAndCannotModifySource(t *testing.T) {
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("immutable"), 0); err != nil {
		t.Fatal(err)
	}
	hashes, _ := s.Hashes()
	object, err := j.CommitStaging(s, hashes)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(object)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	linked, err := j.LinkStaging(src, []provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := src.Stat()
	b, _ := os.Stat(linked.Path)
	if !os.SameFile(a, b) {
		t.Fatal("copy allocated a new inode")
	}
	if _, err := linked.WriteAt([]byte("bad"), 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("linked write allowed: %v", err)
	}
	if err := linked.Truncate(0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("linked truncate allowed: %v", err)
	}
	gotHashes, err := linked.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	gotObject, err := j.CommitStaging(linked, gotHashes)
	if err != nil {
		t.Fatal(err)
	}
	if gotObject != object {
		t.Fatalf("hash dedup failed: %s %s", gotObject, object)
	}
	if _, err := os.Stat(linked.Path); !os.IsNotExist(err) {
		t.Fatalf("rename no-op left staging alias: %v", err)
	}
	if data, err := os.ReadFile(object); err != nil || string(data) != "immutable" {
		t.Fatalf("source changed: %q %v", data, err)
	}
}

func TestLinkedStagingRejectsReplacedNameAndSymlinks(t *testing.T) {
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := j.LinkStaging(f, nil); err == nil {
		s.Discard()
		t.Fatal("linked replacement instead of open source")
	}
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(p, symlink); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Open(symlink)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	if s, err := j.LinkStaging(sf, nil); err == nil {
		s.Discard()
		t.Fatal("installed symlink as a durable object")
	}
	files, err := os.ReadDir(j.StagingDir())
	if err != nil || len(files) != 0 {
		t.Fatalf("failed links left staging entries: %v %v", files, err)
	}
}
