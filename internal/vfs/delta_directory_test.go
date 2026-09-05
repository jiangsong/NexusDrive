package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

func TestDeltaDirectoryDeleteProtectsPendingDescendants(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "clean"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			e.fake.Seed("parent/nested/old", []byte("remote"))
			if _, err := e.fs.ReadDirPath(ctx, "/ali/parent/nested"); err != nil {
				t.Fatal(err)
			}
			parent, err := e.store.Resolve(ctx, "/ali/parent")
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				if _, err := e.fs.WriteFile(ctx, "/ali/parent/nested/local", []byte("keep me"), false); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
				t.Fatal(err)
			}
			if err := e.fake.Delete(ctx, parent.RemoteID); err != nil {
				t.Fatal(err)
			}
			r := NewRefresher(e.fs, time.Minute)
			if _, err := r.PollOnce(ctx, e.mount()); err != nil {
				t.Fatal(err)
			}
			if pending {
				if got, err := e.fs.ReadFileRange(ctx, "/ali/parent/nested/local", 0, 100); err != nil || string(got) != "keep me" {
					t.Fatalf("delta removed pending descendant: %q %v", got, err)
				}
				if got, err := e.store.Get(ctx, parent.Ino); err != nil || got.RemoteID != parent.RemoteID {
					t.Fatalf("original parent lost: %+v %v", got, err)
				}
				if st, err := e.j.Stats(ctx); err != nil || st.Pending != 1 {
					t.Fatalf("pending upload changed: %+v %v", st, err)
				}
			} else if _, err := e.store.Get(ctx, parent.Ino); !errors.Is(err, meta.ErrNotFound) {
				t.Fatalf("clean remote deletion not applied: %v", err)
			}
			if cursor, err := e.store.Cursor(ctx, "ali"); err != nil || cursor != e.fake.Cursor() {
				t.Fatalf("cursor did not advance: %q %v", cursor, err)
			}
		})
	}
}

func TestDeltaDirectoryDeleteProtectsUncommittedCreate(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("parent/old", []byte("remote"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/parent"); err != nil {
		t.Fatal(err)
	}
	parent, err := e.store.Resolve(ctx, "/ali/parent")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, parent.Ino, "local")
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	if _, err := e.fs.Write(ctx, h, []byte("not yet committed"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.Delete(ctx, parent.RemoteID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRefresher(e.fs, time.Minute).PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.Get(ctx, h.Ino); err != nil || n.ParentIno != parent.Ino || !n.Dirty {
		t.Fatalf("uncommitted file lost: %+v %v", n, err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatalf("cannot commit protected create: %v", err)
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/parent/local", 0, 100); err != nil || string(got) != "not yet committed" {
		t.Fatalf("committed bytes lost: %q %v", got, err)
	}
}

func TestDeltaPublicationFailureRetainsCursorAndUsesInvalidationFallback(t *testing.T) {
	for _, action := range []string{"delete", "update"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			entry := e.fake.Seed("entry", []byte("original"))
			if _, err := e.fs.ReadFileRange(ctx, "/ali/entry", 0, 100); err != nil {
				t.Fatal(err)
			}
			old, err := e.store.Resolve(ctx, "/ali/entry")
			if err != nil {
				t.Fatal(err)
			}
			cursor := e.fake.Cursor()
			if err := e.store.SetCursor(ctx, "ali", cursor); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_delta BEFORE UPDATE ON nodes WHEN OLD.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`
			if action == "delete" {
				if err := e.fake.Delete(ctx, entry.ID); err != nil {
					t.Fatal(err)
				}
				trigger = `CREATE TRIGGER fail_delta BEFORE DELETE ON nodes WHEN OLD.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`
			} else {
				e.fake.Seed("entry", []byte("replacement"))
			}
			if _, err := e.store.DB().ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			all := 0
			e.fs.SetInvalidateAll(func() { all++ })
			changes, cancel := e.fs.WatchChanges()
			defer cancel()
			r := NewRefresher(e.fs, time.Minute)
			if _, err := r.PollOnce(ctx, e.mount()); err == nil {
				t.Fatal("publication failure ignored")
			}
			if got, err := e.store.Cursor(ctx, "ali"); err != nil || got != cursor {
				t.Fatalf("failed event consumed: %q %v", got, err)
			}
			if got, err := e.store.Get(ctx, old.Ino); err != nil || got.Version != old.Version {
				t.Fatalf("failed publication changed metadata: %+v %v", got, err)
			}
			key := cache.FileKey{Remote: old.Remote, RemoteID: old.RemoteID, Version: old.Version}
			if have, _ := e.cache.Present(key); have == 0 {
				t.Fatal("failed publication prematurely evicted content")
			}
			if all != 1 {
				t.Fatalf("no conservative kernel invalidation: %d", all)
			}
			select {
			case c := <-changes:
				if !c.Rescan {
					t.Fatalf("missing uncertain-outcome hint: %+v", c)
				}
			default:
				t.Fatal("missing fallback notification")
			}
			if _, err := e.store.DB().ExecContext(ctx, `DROP TRIGGER fail_delta`); err != nil {
				t.Fatal(err)
			}
			if applied, err := r.PollOnce(ctx, e.mount()); err != nil || applied != 1 {
				t.Fatalf("failed event did not replay: applied=%d err=%v", applied, err)
			}
			if cursor, err := e.store.Cursor(ctx, "ali"); err != nil || cursor != e.fake.Cursor() {
				t.Fatalf("replay cursor: %q %v", cursor, err)
			}
		})
	}
}

func TestDeltaDirectoryTypeReplacement(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "clean"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			e.fake.Seed("entry/old", []byte("old"))
			if _, err := e.fs.ReadDirPath(ctx, "/ali/entry"); err != nil {
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
			changes, cancel := e.fs.WatchChanges()
			defer cancel()
			r := NewRefresher(e.fs, time.Minute)
			next := provider.Entry{ID: old.RemoteID, Name: old.Name, Kind: provider.KindFile, Size: 5, Version: "replacement"}
			applied, err := r.apply(ctx, e.mount(), provider.Change{Op: provider.ChangeUpsert, ID: old.RemoteID, Entry: &next})
			if err != nil {
				t.Fatal(err)
			}
			current, err := e.store.Resolve(ctx, "/ali/entry")
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				if applied || current.Ino != old.Ino || !current.IsDir() || current.RemoteID != old.RemoteID {
					t.Fatalf("delta rebound pending descendant to replacement: %+v applied=%v", current, applied)
				}
				if got, err := e.fs.ReadFileRange(ctx, "/ali/entry/local", 0, 100); err != nil || string(got) != "keep me" {
					t.Fatalf("pending descendant unreadable: %q %v", got, err)
				}
			} else {
				if !applied || current.Ino == old.Ino || current.IsDir() {
					t.Fatalf("delta reused directory inode for file: %+v applied=%v", current, applied)
				}
				if _, err := e.store.Get(ctx, old.Ino); !errors.Is(err, meta.ErrNotFound) {
					t.Fatalf("old directory survived: %v", err)
				}
				if kids, err := e.store.Children(ctx, old.Ino); err != nil || len(kids) != 0 {
					t.Fatalf("stale descendants survived: %+v %v", kids, err)
				}
				select {
				case c := <-changes:
					if !c.Affects("/ali/entry/old") {
						t.Fatalf("replacement omitted subtree notification: %+v", c)
					}
				default:
					t.Fatal("missing replacement notification")
				}
			}
		})
	}
}

func TestRemoteDirectoryChangeProtectsOpenWriter(t *testing.T) {
	for _, action := range []string{"delta-delete", "delta-update", "listing-replace"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			entry := e.fake.Seed("parent/file", []byte("original"))
			if _, err := e.fs.ReadFileRange(ctx, "/ali/parent/file", 0, 100); err != nil {
				t.Fatal(err)
			}
			file, err := e.store.Resolve(ctx, "/ali/parent/file")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := e.store.Get(ctx, file.ParentIno)
			if err != nil {
				t.Fatal(err)
			}
			h, err := e.fs.Open(ctx, file.Ino, true)
			if err != nil {
				t.Fatal(err)
			}
			defer e.fs.Release(ctx, h)
			if err := e.fs.Truncate(ctx, h, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := e.fs.Write(ctx, h, []byte("local edit"), 0); err != nil {
				t.Fatal(err)
			}
			if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
				t.Fatal(err)
			}
			if action == "delta-update" {
				e.fake.Seed("parent/file", []byte("external edit"))
			} else {
				if err := e.fake.Delete(ctx, parent.RemoteID); err != nil {
					t.Fatal(err)
				}
				if action == "listing-replace" {
					e.fake.Seed("parent", []byte("replacement file"))
				}
			}
			if action == "listing-replace" {
				e.clk.advance(2 * time.Minute)
				_, err = e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10})
			} else {
				_, err = NewRefresher(e.fs, time.Minute).PollOnce(ctx, e.mount())
			}
			if err != nil {
				t.Fatal(err)
			}
			if current, err := e.store.Get(ctx, file.Ino); err != nil || current.Version != entry.Version || current.ParentIno != parent.Ino {
				t.Fatalf("remote event destroyed open writer's base: %+v %v", current, err)
			}
			if err := e.fs.Release(ctx, h); err != nil {
				t.Fatalf("cannot commit after remote mutation: %v", err)
			}
			if got, err := e.fs.ReadFileRange(ctx, "/ali/parent/file", 0, 100); err != nil || string(got) != "local edit" {
				t.Fatalf("open writer lost: %q %v", got, err)
			}
		})
	}
}
