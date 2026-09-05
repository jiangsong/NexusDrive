package vfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
)

// TestAFinishedUploadStopsHoldingItsBytesOnDisk. The staged blob is a hard
// link shared with the read cache. While the queue kept its own link, the
// bytes of every file ever uploaded stayed on disk for the life of the
// installation: the cache could evict its link and reclaim nothing, because
// the journal still named the object. That is a disk leak proportional to
// everything the user has ever written, and nothing reports it.
func TestAFinishedUploadStopsHoldingItsBytesOnDisk(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	const files = 3
	for i := 0; i < files; i++ {
		body := fmt.Sprintf("content of file %d", i)
		if _, err := e.fs.WriteFile(ctx, fmt.Sprintf("/ali/f%d", i), []byte(body), true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}

	objects, err := os.ReadDir(e.j.ObjectsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		names := []string{}
		var held int64
		for _, o := range objects {
			names = append(names, o.Name())
			if info, err := o.Info(); err == nil {
				held += info.Size()
			}
		}
		t.Fatalf("%d finished uploads left %d objects (%d bytes) in the queue's object store: %v;"+
			" the cache can never reclaim content the queue still names", files, len(objects), held, names)
	}

	// Releasing the queue's link must not take the data with it: the cache
	// holds the other link, and reading back must not go to the backend.
	before := e.fake.Calls("ReadRange")
	for i := 0; i < files; i++ {
		want := fmt.Sprintf("content of file %d", i)
		got, err := e.fs.ReadFileRange(ctx, fmt.Sprintf("/ali/f%d", i), 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("after the queue released its link the file reads %q, want %q", got, want)
		}
	}
	if fetched := e.fake.Calls("ReadRange") - before; fetched != 0 {
		t.Fatalf("reading back a just-uploaded file made %d backend requests; the cached copy was lost", fetched)
	}

	// The completed rows themselves are still there — a strict-mode writer
	// waiting on one has to be able to see it finish.
	all, err := e.j.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != files {
		t.Fatalf("%d completed rows remain, want %d", len(all), files)
	}
	for _, u := range all {
		if u.BlobPath != "" {
			t.Fatalf("completed upload %s still names a blob at %q", u.ID, u.BlobPath)
		}
	}
}

// TestAFinishedServerlessCopyReleasesItsPayloadToo. A copy that the backend
// cannot do server-side is staged locally and handed to the upload queue, so
// it holds the same kind of hard link — and a copied file is by definition one
// whose bytes already exist elsewhere. Releasing it only at the next restart
// would mean a batch copy needs twice the disk until the daemon is bounced.
func TestAFinishedServerlessCopyReleasesItsPayloadToo(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	body := bytes.Repeat([]byte("copy-me"), 300)
	e.fake.Seed("source", body)
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	copies, err := os.ReadDir(e.j.CopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 0 {
		t.Fatalf("a completed copy left %d payloads staged: %v", len(copies), copies)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the copy lost its content when the payload was released: %d bytes, %v", len(got), err)
	}
}
