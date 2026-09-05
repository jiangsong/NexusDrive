package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/meta"
)

func TestStaleDirectoryListingCannotResurrectDeletedOrRenamedFile(t *testing.T) {
	for _, action := range []string{"delete", "rename"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			e.fake.Seed("original", []byte("retained bytes"))
			if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
				t.Fatal(err)
			}
			root, err := e.fs.StatPath(ctx, "/ali")
			if err != nil {
				t.Fatal(err)
			}
			n, err := e.store.Get(ctx, root.Ino)
			if err != nil {
				t.Fatal(err)
			}
			m := e.mount()
			m.Provider = &staleLister{Provider: e.fake, during: func() {
				var err error
				if action == "delete" {
					err = e.fs.Remove(ctx, root.Ino, "original", false)
				} else {
					err = e.fs.Rename(ctx, root.Ino, "original", root.Ino, "renamed")
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			err = e.fs.fetchDir(ctx, m, root.Ino, n, true)
			if got, err := e.store.Lookup(ctx, root.Ino, "original"); err == nil {
				t.Fatalf("old listing resurrected %s file: %+v", action, got)
			} else if !errors.Is(err, meta.ErrNotFound) {
				t.Fatal(err)
			}
			if !errors.Is(err, meta.ErrListingChanged) {
				t.Fatalf("stale snapshot not fenced: %v", err)
			}
			if action == "rename" {
				if b, err := e.fs.ReadFileRange(ctx, "/ali/renamed", 0, 100); err != nil || string(b) != "retained bytes" {
					t.Fatalf("renamed content lost: %q %v", b, err)
				}
			}
		})
	}
}

func TestStaleListingAfterPendingRenameKeepsJournalTargetAndReadableBytes(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("remote", []byte("remote"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/pending", []byte("pending content"), false); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.fs.StatPath(ctx, "/ali/pending")
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Get(ctx, root.Ino)
	if err != nil {
		t.Fatal(err)
	}
	m := e.mount()
	m.Provider = &staleLister{Provider: e.fake, during: func() {
		if err := e.fs.Rename(ctx, root.Ino, "pending", root.Ino, "renamed"); err != nil {
			t.Fatal(err)
		}
	}}
	if err := e.fs.fetchDir(ctx, m, root.Ino, n, true); !errors.Is(err, meta.ErrListingChanged) {
		t.Fatalf("old listing not fenced: %v", err)
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/renamed", 0, 100); err != nil || string(got) != "pending content" {
		t.Fatalf("local content lost: %q %v", got, err)
	}
	queued, err := e.j.ByIno(ctx, pending.Ino)
	if err != nil || len(queued) != 1 || queued[0].Name != "renamed" {
		t.Fatalf("journal retarget changed: %+v %v", queued, err)
	}
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/renamed", 0, 100); err != nil || string(got) != "pending content" {
		t.Fatalf("landed content lost: %q %v", got, err)
	}
}
