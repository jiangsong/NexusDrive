package vfs

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func TestReaderSeesUnflushedPackAndCommit(t *testing.T) {
	for _, closeWriter := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "release"}[closeWriter], func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			dir, err := e.store.Resolve(ctx, "/ali")
			if err != nil {
				t.Fatal(err)
			}
			w, err := e.fs.Create(ctx, dir.Ino, "tmp_pack")
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("pack-data"), 500)
			if _, err := e.fs.Write(ctx, w, payload, 0); err != nil {
				t.Fatal(err)
			}
			r, err := e.fs.Open(ctx, w.Ino, false)
			if err != nil {
				t.Fatal(err)
			}
			defer e.fs.Release(ctx, r)
			check := func() {
				t.Helper()
				got := make([]byte, len(payload))
				n, err := e.fs.Read(ctx, r, got, 0)
				if err != nil || n != len(payload) || !bytes.Equal(got, payload) {
					t.Fatalf("read %d bytes, %v; want %d staged bytes", n, err, len(payload))
				}
			}
			check()
			tail := []byte("tail")
			if _, err := e.fs.Write(ctx, w, tail, int64(len(payload))); err != nil {
				t.Fatal(err)
			}
			payload = append(payload, tail...)
			check()
			if closeWriter {
				err = e.fs.Release(ctx, w)
			} else {
				err = e.fs.Sync(ctx, w)
				defer e.fs.Release(ctx, w)
			}
			if err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestReaderOpenedBeforeWriterAdoptsCommittedVersion(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	a, err := e.fs.StatPath(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.fs.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, r)
	w, err := e.fs.Open(ctx, a.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Truncate(ctx, w, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, w, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, w); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	if n, err := e.fs.Read(ctx, r, got, 0); err != nil || n != len(got) || string(got) != "new" {
		t.Fatalf("read %q (%d bytes): %v", got, n, err)
	}
}

func TestVersionedReadIgnoresUncommittedWriter(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	a, err := e.fs.StatPath(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	w, err := e.fs.Open(ctx, a.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, w)
	if err := e.fs.Truncate(ctx, w, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, w, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	got, current, err := e.fs.ReadFileRangeAtVersion(ctx, "/ali/file", a.Version, 0, 0)
	if err != nil || current != a.Version || string(got) != "old" {
		t.Fatalf("versioned read %q at %q: %v; want old at %q", got, current, err, a.Version)
	}
}

// A commit closes staging before publishing the blob. Readers must wait for
// that transition rather than using the closed fd or the old metadata size.
func TestPackReadersDuringCommit(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "release"}[release], func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			dir, err := e.store.Resolve(ctx, "/ali")
			if err != nil {
				t.Fatal(err)
			}
			w, err := e.fs.Create(ctx, dir.Ino, "pack")
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("pack"), 1200)
			if _, err := e.fs.Write(ctx, w, payload, 0); err != nil {
				t.Fatal(err)
			}
			r, err := e.fs.Open(ctx, w.Ino, false)
			if err != nil {
				t.Fatal(err)
			}
			defer e.fs.Release(ctx, r)
			entered, resume := make(chan struct{}), make(chan struct{})
			e.fs.commitFault = func() error { close(entered); <-resume; return nil }
			done := make(chan error, 1)
			go func() {
				if release {
					done <- e.fs.Release(ctx, w)
				} else {
					done <- e.fs.Sync(ctx, w)
				}
			}()
			<-entered
			const readers = 8
			results := make(chan error, readers)
			for i := 0; i < readers; i++ {
				go func() {
					got := make([]byte, len(payload))
					n, err := e.fs.Read(ctx, r, got, 0)
					if err == nil && (n != len(payload) || !bytes.Equal(got, payload)) {
						err = fmt.Errorf("read %d bytes; want %d committed bytes", n, len(payload))
					}
					results <- err
				}()
			}
			close(resume)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			for i := 0; i < readers; i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if !release {
				if err := e.fs.Release(ctx, w); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
