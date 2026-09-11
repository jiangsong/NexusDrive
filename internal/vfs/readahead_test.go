package vfs

import (
	"path/filepath"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// TestReadaheadRequestMustBeBlockMultiple checks that New rejects a
// ReadaheadRequest that is not a positive multiple of the cache's block
// size, instead of silently truncating it at readahead time.
func TestReadaheadRequestMustBeBlockMultiple(t *testing.T) {
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })

	cases := []struct {
		name string
		req  int64
		ok   bool
	}{
		{"zero means derive", 0, true},
		{"exact multiple", 4096 * 3, true},
		{"not a multiple", 5000, false},
		{"negative", -4096, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(Options{Meta: store, Cache: ca, ReadaheadRequest: c.req})
			if c.ok && err != nil {
				t.Fatalf("New(ReadaheadRequest=%d) = %v, want no error", c.req, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("New(ReadaheadRequest=%d) = nil error, want a rejection", c.req)
			}
		})
	}
}

// TestContiguousGroups checks the grouping helper that lets a readahead run
// split at gaps left by keys another flight already owns.
func TestContiguousGroups(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		want [][]int64
	}{
		{"empty", nil, nil},
		{"single", []int64{5}, [][]int64{{5}}},
		{"one run", []int64{2, 3, 4}, [][]int64{{2, 3, 4}}},
		{"gap splits", []int64{2, 3, 5, 6, 7}, [][]int64{{2, 3}, {5, 6, 7}}},
		{"all separate", []int64{1, 3, 5}, [][]int64{{1}, {3}, {5}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contiguousGroups(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("contiguousGroups(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if len(got[i]) != len(c.want[i]) {
					t.Fatalf("contiguousGroups(%v) group %d = %v, want %v", c.in, i, got[i], c.want[i])
				}
				for j := range got[i] {
					if got[i][j] != c.want[i][j] {
						t.Fatalf("contiguousGroups(%v) group %d = %v, want %v", c.in, i, got[i], c.want[i])
					}
				}
			}
		})
	}
}
