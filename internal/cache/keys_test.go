package cache

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countDirs(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			n++
		}
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

func indexLines(t *testing.T, c *Cache) int {
	t.Helper()
	f, err := os.Open(c.keysPath())
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}

// TestReloadNeedsNoDirectoryPerCachedFile: a restart rebuilds the index by
// walking blocks/, and on a cold rotating disk every directory in that tree
// is a seek. Recording each file's identity in its own blocks/aa/bb/ leaf
// created one directory per file ever cached — tens of thousands for a cache
// holding only whole files — and a restart took minutes before the mount
// appeared. The identities live in one sequential file instead.
func TestReloadNeedsNoDirectoryPerCachedFile(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	const files = 40
	for i := range files {
		k := FileKey{Remote: "r", RemoteID: fmt.Sprintf("id-%d", i), Version: "v"}
		if err := c.PutWhole(k, bytes.NewReader(bytes.Repeat([]byte("w"), 100)), 100); err != nil {
			t.Fatal(err)
		}
	}
	if n := countDirs(t, filepath.Join(dir, "blocks")); n != 0 {
		t.Fatalf("%d directories under blocks/ for %d whole files with no blocks; each one is a seek on every restart", n, files)
	}
	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c2.Keys()); got != files {
		t.Fatalf("after reload Keys() has %d entries, want %d", got, files)
	}
}

// TestReloadMigratesLegacyKeyFiles: a cache written by an earlier build has a
// .key sidecar in each file's leaf directory. One restart adopts them into the
// index and removes the sidecars and the directories they alone kept alive,
// so only that restart pays for the walk.
func TestReloadMigratesLegacyKeyFiles(t *testing.T) {
	dir := t.TempDir()
	k := FileKey{Remote: "r", RemoteID: "legacy-id", Version: "v3"}
	fh := k.hash()
	leaf := filepath.Join(dir, "blocks", fh[:2], fh[2:4])
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(leaf, fh+".key")
	if err := os.WriteFile(legacy, []byte(strings.Join([]string{k.Remote, k.RemoteID, k.Version}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sidecar whose name does not hash from its contents is garbage from a
	// different key scheme; it must not become a key, only be removed.
	bogus := filepath.Join(leaf, strings.Repeat("0", 32)+".key")
	if err := os.WriteFile(bogus, []byte("x\ny\nz"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Keys(); len(got) != 1 || got[0] != k {
		t.Fatalf("Keys() after migrating a legacy sidecar = %+v, want [%+v]", got, k)
	}
	for _, p := range []string{legacy, bogus} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after migration (err=%v)", p, err)
		}
	}
	if n := countDirs(t, filepath.Join(dir, "blocks")); n != 0 {
		t.Fatalf("%d directories left under blocks/ after migrating sidecars that were their only content", n)
	}
	c.Close()

	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Keys(); len(got) != 1 || got[0] != k {
		t.Fatalf("Keys() on the restart after migration = %+v, want [%+v]", got, k)
	}
}

// TestReloadPrunesLeafDirectoriesLeftByEviction: evicting a block unlinks
// the file and leaves its aa/bb directory behind, and a cache that has been
// running for a while has tens of thousands of them holding nothing. They
// are seeks on the next cold reload, so reload removes the empty ones and
// keeps only the directories that hold a block.
func TestReloadPrunesLeafDirectoriesLeftByEviction(t *testing.T) {
	dir := t.TempDir()
	for _, leaf := range []string{"0a/1b", "0a/2c", "ff/ee"} {
		if err := os.MkdirAll(filepath.Join(dir, "blocks", leaf), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if n := countDirs(t, filepath.Join(dir, "blocks")); n != 0 {
		t.Fatalf("%d directories under blocks/ after a reload that found only empty ones", n)
	}
	k := FileKey{Remote: "r", RemoteID: "with-block", Version: "v"}
	if err := c.Put(k, 0, bytes.Repeat([]byte("b"), 64), 128); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if n := countDirs(t, filepath.Join(dir, "blocks")); n != 2 {
		t.Fatalf("%d directories under blocks/ with one block cached, want its leaf and parent only", n)
	}
	if _, ok := c2.Get(k, 0); !ok {
		t.Fatal("the block whose directory was kept is not readable")
	}
}

// TestKeysIndexDropsForgottenFilesOnReload: the index is append-only while
// the cache runs, so a forgotten file's line stays behind until a reload
// rewrites the file from what is still cached.
func TestKeysIndexDropsForgottenFilesOnReload(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	keep := FileKey{Remote: "r", RemoteID: "keep", Version: "v"}
	drop := FileKey{Remote: "r", RemoteID: "drop", Version: "v"}
	for _, k := range []FileKey{keep, drop} {
		if err := c.Put(k, 0, bytes.Repeat([]byte("b"), 64), 64); err != nil {
			t.Fatal(err)
		}
	}
	c.Forget(drop)
	c.Close()

	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Keys(); len(got) != 1 || got[0] != keep {
		t.Fatalf("Keys() after reload = %+v, want [%+v]", got, keep)
	}
	if n := indexLines(t, c2); n != 1 {
		t.Fatalf("index holds %d lines after reload, want 1", n)
	}
	// A reload with nothing to compact leaves the file alone; its line
	// count is the same as the number of keys either way.
	if got := c2.Keys(); len(got) != 1 {
		t.Fatalf("Keys() = %+v", got)
	}
}

// TestKeysIndexStaysBoundedUnderChurn: a daemon that runs for months, with
// files cached and forgotten all the while, must not grow the index without
// limit; it is compacted in place once the dead lines outnumber the live ones.
func TestKeysIndexStaysBoundedUnderChurn(t *testing.T) {
	old := keysCompactSlack
	keysCompactSlack = 8
	t.Cleanup(func() { keysCompactSlack = old })

	c, err := newClosingCache(t, Options{Dir: t.TempDir(), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	live := FileKey{Remote: "r", RemoteID: "live", Version: "v"}
	if err := c.Put(live, 0, bytes.Repeat([]byte("l"), 64), 64); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		k := FileKey{Remote: "r", RemoteID: fmt.Sprintf("churn-%d", i), Version: "v"}
		if err := c.Put(k, 0, bytes.Repeat([]byte("c"), 64), 64); err != nil {
			t.Fatal(err)
		}
		c.Forget(k)
	}
	if n := indexLines(t, c); n > 2*keysCompactSlack+1 {
		t.Fatalf("index holds %d lines with one live file and a compaction slack of %d", n, keysCompactSlack)
	}
	c.Close()
	c2, err := newClosingCache(t, Options{Dir: c.Dir(), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Keys(); len(got) != 1 || got[0] != live {
		t.Fatalf("Keys() after churn and reload = %+v, want [%+v]", got, live)
	}
}

// TestKeysIndexBacksOffWhenCompactionFails: a rewrite is a pass over every
// live key, taken under admitMu. When the disk refuses the temporary file,
// the index must not retry that pass on every following append but wait for
// another slack's worth of lines.
func TestKeysIndexBacksOffWhenCompactionFails(t *testing.T) {
	old := keysCompactSlack
	keysCompactSlack = 8
	t.Cleanup(func() { keysCompactSlack = old })

	c, err := newClosingCache(t, Options{Dir: t.TempDir(), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	// A directory in the temporary file's place: every rewrite fails to open it.
	if err := os.Mkdir(c.keysPath()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	const churn = 200
	for i := range churn {
		k := FileKey{Remote: "r", RemoteID: fmt.Sprintf("churn-%d", i), Version: "v"}
		if err := c.Put(k, 0, bytes.Repeat([]byte("c"), 64), 64); err != nil {
			t.Fatal(err)
		}
		c.Forget(k)
	}
	c.keys.mu.Lock()
	attempts := c.keys.compactions
	c.keys.mu.Unlock()
	// Two appends per iteration, one attempt per slack's worth of them.
	if limit := 2*churn/keysCompactSlack + 1; attempts < 2 || attempts > limit {
		t.Fatalf("%d compaction attempts over %d appends with slack %d, want between 2 and %d", attempts, 2*churn, keysCompactSlack, limit)
	}
	if err := os.Remove(c.keysPath() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	// Once the disk cooperates the next due compaction lands.
	for i := range keysCompactSlack + 1 {
		k := FileKey{Remote: "r", RemoteID: fmt.Sprintf("after-%d", i), Version: "v"}
		if err := c.Put(k, 0, bytes.Repeat([]byte("c"), 64), 64); err != nil {
			t.Fatal(err)
		}
		c.Forget(k)
	}
	if n := indexLines(t, c); n > 2*keysCompactSlack {
		t.Fatalf("index holds %d lines after the disk recovered, want a compacted one", n)
	}
}
