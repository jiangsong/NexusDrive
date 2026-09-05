package vfs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/meta"
)

func TestDirectoryReplacementRefreshClearsOldPathsAndProtectsLocalFiles(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, toFile := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%v/to-file=%v", pending, toFile), func(t *testing.T) {
				e := newEnv(t, envOpt{})
				ctx := context.Background()
				e.fake.Seed("entry/old", []byte("old bytes"))
				if _, err := e.fs.ReadFileRange(ctx, "/ali/entry/old", 0, 100); err != nil {
					t.Fatal(err)
				}
				old, err := e.store.Resolve(ctx, "/ali/entry")
				if err != nil {
					t.Fatal(err)
				}
				if pending {
					if _, err := e.fs.WriteFile(ctx, "/ali/entry/local", []byte("keep me"), false); err != nil {
						t.Fatal(err)
					}
				}
				if err := e.fake.Delete(ctx, old.RemoteID); err != nil {
					t.Fatal(err)
				}
				target := "/ali/entry/new"
				if toFile {
					e.fake.Seed("entry", []byte("new bytes"))
					target = "/ali/entry"
				} else {
					e.fake.Seed("entry/new", []byte("new bytes"))
				}
				e.clk.advance(2 * time.Minute)
				changes, cancel := e.fs.WatchChanges()
				defer cancel()
				invalidatedName, invalidatedOld := false, false
				e.fs.SetInvalidateEntry(func(parent uint64, name string) {
					if parent == old.ParentIno && name == old.Name {
						invalidatedName = true
					}
				})
				e.fs.SetInvalidate(func(ino uint64) {
					if ino == old.Ino {
						invalidatedOld = true
					}
				})
				if _, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10}); err != nil {
					t.Fatal(err)
				}
				current, err := e.store.Resolve(ctx, "/ali/entry")
				if err != nil {
					t.Fatal(err)
				}
				if pending {
					if current.Ino != old.Ino || current.RemoteID != old.RemoteID || !current.IsDir() {
						t.Fatalf("pending content rebound: %+v", current)
					}
					if got, err := e.fs.ReadFileRange(ctx, "/ali/entry/local", 0, 100); err != nil || string(got) != "keep me" {
						t.Fatalf("pending content lost: %q %v", got, err)
					}
					return
				}
				if current.Ino == old.Ino || current.IsDir() == toFile || current.RemoteID == old.RemoteID {
					t.Fatalf("replacement identity wrong: %+v", current)
				}
				if _, err := e.store.Get(ctx, old.Ino); !errors.Is(err, meta.ErrNotFound) {
					t.Fatalf("old directory inode survived: %v", err)
				}
				if !invalidatedName || !invalidatedOld {
					t.Fatalf("missing kernel invalidation: name=%v old=%v", invalidatedName, invalidatedOld)
				}
				select {
				case c := <-changes:
					if !c.Affects("/ali/entry/old") {
						t.Fatalf("missing old subtree hint: %+v", c)
					}
				default:
					t.Fatal("missing replacement event")
				}
				if _, err := e.fs.StatPath(ctx, "/ali/entry/old"); err == nil {
					t.Fatal("old path cache survived replacement")
				}
				if got, err := e.fs.ReadFileRange(ctx, target, 0, 100); err != nil || string(got) != "new bytes" {
					t.Fatalf("new object cannot be read: %q %v", got, err)
				}
			})
		}
	}
}
