package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"cloudfs/internal/provider"
)

func TestStagingSpaceAdmissionPreservesContent(t *testing.T) {
	var deny bool
	var reserved, released []int64
	j, err := Open(Options{Dir: t.TempDir(), ReserveSpace: func(dir string, n int64) (func(), error) {
		if filepath.Base(dir) != "staging" {
			t.Fatalf("wrong target: %s", dir)
		}
		if deny {
			return nil, syscall.ENOSPC
		}
		reserved = append(reserved, n)
		return func() { released = append(released, n) }, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	if _, err := s.WriteAt([]byte("original"), 0); err != nil {
		t.Fatal(err)
	}
	before, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	deny = true
	for _, off := range []int64{0, 1000} {
		if n, err := s.WriteAt([]byte("replacement"), off); n != 0 || !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if err := s.Truncate(100); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("grow: %v", err)
	}
	if n, err := s.WriteAt(nil, 1000); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}
	if s.Size() != 8 {
		t.Fatalf("size: %d", s.Size())
	}
	after, err := s.Hashes()
	if err != nil || after[provider.HashSHA256] != before[provider.HashSHA256] {
		t.Fatalf("hash changed: %v %v", after, err)
	}
	if err := s.Truncate(3); err != nil {
		t.Fatalf("shrink under pressure: %v", err)
	}
	if len(reserved) != 1 || len(released) != 1 || reserved[0] != 8 || released[0] != 8 {
		t.Fatalf("reservation leak: %v %v", reserved, released)
	}
}

type shortDiskWrite struct{ *os.File }

func (f shortDiskWrite) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.File.WriteAt(p[:min(3, len(p))], off)
	if err != nil {
		return n, err
	}
	return n, syscall.ENOSPC
}

func TestPartialDiskFailureTracksAcceptedBytesAndReleasesSpace(t *testing.T) {
	var held atomic.Int64
	j, err := Open(Options{Dir: t.TempDir(), ReserveSpace: func(_ string, n int64) (func(), error) {
		held.Add(n)
		return func() { held.Add(-n) }, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	s.f = shortDiskWrite{s.f.(*os.File)}
	if n, err := s.WriteAt([]byte("abcdef"), 0); n != 3 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write: %d %v", n, err)
	}
	if s.Size() != 3 || held.Load() != 0 {
		t.Fatalf("size=%d held=%d", s.Size(), held.Load())
	}
	hashes, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("abc"))
	if hashes[provider.HashSHA256] != hex.EncodeToString(want[:]) {
		t.Fatalf("partial hash: %v", hashes)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("closed"), 3); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	if held.Load() != 0 {
		t.Fatal("failed syscall leaked reservation")
	}
}

func TestAdoptedAndSparseStagingRequireWriteSpace(t *testing.T) {
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	p := filepath.Join(t.TempDir(), "adopted")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests []int64
	j.SetSpaceReserver(func(dir string, n int64) (func(), error) {
		if dir != filepath.Dir(p) {
			t.Fatalf("wrong adopted volume: %s", dir)
		}
		requests = append(requests, n)
		return func() {}, nil
	})
	s, err := j.AdoptStaging(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	if err := s.Truncate(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("hole"), 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(requests) != "[1021 4 3]" {
		t.Fatalf("requests: %v", requests)
	}
	for _, off := range []int64{-1, math.MaxInt64} {
		if _, err := s.WriteAt([]byte("x"), off); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("range: %v", err)
		}
	}
	if err := s.Truncate(-1); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("negative truncate: %v", err)
	}
}

func TestStagingConcurrentWritesKeepHashesConsistent(t *testing.T) {
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.WriteAt(bytes.Repeat([]byte{byte(i)}, 32), int64(i*32)); err != nil {
				t.Error(err)
			}
			_ = s.Size()
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	got, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != 1024 || got[provider.HashSHA256] != hex.EncodeToString(want[:]) {
		t.Fatalf("size/hash mismatch: %d %v", s.Size(), got)
	}
}
