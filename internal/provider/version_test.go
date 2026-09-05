package provider

import (
	"testing"
	"time"
)

// TestEnsureVersionNeverLeavesAFileWithoutAToken guards the block cache: two
// different contents must never share a cache key.
func TestEnsureVersionNeverLeavesAFileWithoutAToken(t *testing.T) {
	cases := []struct {
		name string
		in   Entry
		want string
	}{
		{
			name: "an existing version is left alone",
			in:   Entry{ID: "1", Version: "etag-abc", Hashes: Hashes{HashSHA1: "deadbeef"}},
			want: "etag-abc",
		},
		{
			name: "a content hash is preferred",
			in:   Entry{ID: "1", Kind: KindFile, Hashes: Hashes{HashSHA1: "deadbeef"}},
			want: "deadbeef",
		},
		{
			name: "md5 is used when sha1 is absent",
			in:   Entry{ID: "1", Kind: KindFile, Hashes: Hashes{HashMD5: "cafe"}},
			want: "cafe",
		},
		{
			name: "a directory falls back to its id",
			in:   Entry{ID: "42", Kind: KindDir},
			want: "dir-42",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := c.in
			EnsureVersion(&e)
			if e.Version != c.want {
				t.Fatalf("version = %q, want %q", e.Version, c.want)
			}
		})
	}
}

func TestEnsureVersionDistinguishesDifferentContent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// Same file, two different states: the fallback must produce two tokens,
	// otherwise a read after an edit would be served from the old blocks.
	a := Entry{ID: "1", Kind: KindFile, Size: 100, ModTime: base}
	b := Entry{ID: "1", Kind: KindFile, Size: 200, ModTime: base}
	c := Entry{ID: "1", Kind: KindFile, Size: 100, ModTime: base.Add(time.Second)}
	for _, e := range []*Entry{&a, &b, &c} {
		EnsureVersion(e)
		if e.Version == "" {
			t.Fatal("a file entry must never end up without a version")
		}
	}
	if a.Version == b.Version {
		t.Fatal("a size change must change the version")
	}
	if a.Version == c.Version {
		t.Fatal("an mtime change must change the version")
	}
}

func TestEnsureVersionIsStableForUnchangedContent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	a := Entry{ID: "1", Kind: KindFile, Size: 100, ModTime: base}
	b := Entry{ID: "1", Kind: KindFile, Size: 100, ModTime: base}
	EnsureVersion(&a)
	EnsureVersion(&b)
	if a.Version != b.Version {
		t.Fatalf("the same content produced two versions: %q vs %q", a.Version, b.Version)
	}
	// Calling it twice is a no-op.
	first := a.Version
	EnsureVersion(&a)
	if a.Version != first {
		t.Fatalf("EnsureVersion is not idempotent: %q then %q", first, a.Version)
	}
}
