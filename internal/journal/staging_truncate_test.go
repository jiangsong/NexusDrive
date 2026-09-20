package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/provider"
)

// countingFile is a staging file that records how often its content is read
// back, which is what re-hashing from disk costs.
type countingFile struct {
	f     *os.File
	reads int
}

func (c *countingFile) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.f.ReadAt(p, off)
}
func (c *countingFile) WriteAt(p []byte, off int64) (int, error) { return c.f.WriteAt(p, off) }
func (c *countingFile) Sync() error                              { return c.f.Sync() }
func (c *countingFile) Truncate(n int64) error                   { return c.f.Truncate(n) }
func (c *countingFile) Close() error                             { return c.f.Close() }

func newCountingStaging(t *testing.T, want []provider.HashType) (*Staging, *countingFile) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "staging.part")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	c := &countingFile{f: f}
	t.Cleanup(func() { c.Close() })
	return newStagingState(c, "staging", p, want, nil), c
}

func sameHashes(t *testing.T, got, want provider.Hashes) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hash set %v, want %v", got, want)
	}
	for ht, v := range want {
		if got[ht] != v {
			t.Errorf("%s=%s, want %s", ht, got[ht], v)
		}
	}
}

// TestTruncateToZeroKeepsStreamingHashes: truncating to zero leaves an empty
// file, whose digests are known without reading anything. Every O_TRUNC
// rewrite goes through here, and the re-hash it used to force ran on the
// close(2) path.
func TestTruncateToZeroKeepsStreamingHashes(t *testing.T) {
	want := []provider.HashType{provider.HashSHA1, provider.HashMD5, provider.HashSliceMD5, provider.HashPreSHA1}
	s, c := newCountingStaging(t, want)

	first := bytes.Repeat([]byte("first "), 1<<20/6+1)[:1<<20]
	second := bytes.Repeat([]byte("second"), 1<<20/6+1)[:1<<20]
	if _, err := s.WriteAt(first, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt(second, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	// withCRC: the recovery checksum is always computed too.
	expect, err := hashFile(bytes.NewReader(second), int64(len(second)), withCRC(want))
	if err != nil {
		t.Fatal(err)
	}
	sameHashes(t, got, expect)
	if _, ok := got[provider.HashCRC32C]; !ok {
		t.Error("recovery checksum missing from the streamed digests")
	}
	if c.reads != 0 {
		t.Errorf("rewrite after truncate to zero read the file back %d times", c.reads)
	}
}

// TestPartialTruncateStillRehashesFromDisk: dropping part of a file leaves
// digests that have absorbed bytes no longer there, so the fallback stays.
func TestPartialTruncateStillRehashesFromDisk(t *testing.T) {
	want := []provider.HashType{provider.HashSHA1, provider.HashSliceMD5}
	s, c := newCountingStaging(t, want)

	content := bytes.Repeat([]byte("payload "), 1<<20/8)
	if _, err := s.WriteAt(content, 0); err != nil {
		t.Fatal(err)
	}
	const keep = 512 << 10
	if err := s.Truncate(keep); err != nil {
		t.Fatal(err)
	}
	got, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	expect, err := hashFile(bytes.NewReader(content[:keep]), keep, withCRC(want))
	if err != nil {
		t.Fatal(err)
	}
	sameHashes(t, got, expect)
	if c.reads == 0 {
		t.Error("a mid-file truncate hashed from the streaming state, which no longer describes the file")
	}
}

// TestTruncateToCurrentSizeKeepsStreaming: a truncate that changes nothing
// must not throw the streaming state away either.
func TestTruncateToCurrentSizeKeepsStreaming(t *testing.T) {
	want := []provider.HashType{provider.HashMD5}
	s, c := newCountingStaging(t, want)
	content := []byte("unchanged content")
	if _, err := s.WriteAt(content, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(int64(len(content))); err != nil {
		t.Fatal(err)
	}
	got, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	expect, err := hashFile(bytes.NewReader(content), int64(len(content)), withCRC(want))
	if err != nil {
		t.Fatal(err)
	}
	sameHashes(t, got, expect)
	if c.reads != 0 {
		t.Errorf("a no-op truncate cost %d reads", c.reads)
	}
}
