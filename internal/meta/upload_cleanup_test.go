package meta

import (
	"errors"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func localCleanupFixture(t *testing.T, s *Store) LocalVersionCleanup {
	t.Helper()
	ctx := t.Context()
	identity, err := s.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := LocalVersionCleanup{StoreIdentity: identity, Remote: "ali", RemoteID: "cloudfs-local:upload", Version: "local-upload", Size: 17}
	for _, name := range []string{"first-alias", "second-alias"} {
		n, err := s.Insert(ctx, Node{ParentIno: RootIno, Name: name, Kind: provider.KindFile, Remote: r.Remote, RemoteID: r.RemoteID, Version: r.Version, Size: r.Size, Dirty: true})
		if err != nil {
			t.Fatal(err)
		}
		r.Nodes = append(r.Nodes, n)
	}
	return r
}

func TestRemoveLocalVersionDurablyRemovesAllAliasesAndIndexes(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	r := localCleanupFixture(t, s)
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, r.Nodes[1].Ino, RootIno, "pending-alias"); err != nil {
		t.Fatal(err)
	}
	var err error
	r.Nodes, err = s.Aliases(ctx, r.Remote, r.RemoteID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO copy_bindings(copy_id,ino) VALUES ('upload',?)`, r.Nodes[0].Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER require_full_cleanup BEFORE UPDATE ON copy_cleanup_fence WHEN (SELECT synchronous FROM pragma_synchronous)!=2 BEGIN SELECT RAISE(ABORT,'cleanup is not FULL'); END`); err != nil {
		t.Fatal(err)
	}
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(ctx, r.Nodes); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveLocalVersion(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
		t.Fatalf("stale listing revived discarded version: %v", err)
	}
	for _, n := range r.Nodes {
		if _, err := s.Get(ctx, n.Ino); !errors.Is(err, ErrNotFound) {
			t.Fatalf("alias retained: %v", err)
		}
		if absent, err := s.IsAbsent(ctx, n.ParentIno, n.Name); err != nil || !absent {
			t.Fatalf("deleted name not recorded absent: %v", err)
		}
	}
	for _, query := range []string{"alias", "al"} {
		if nodes, err := s.Search(ctx, query, 10); err != nil || len(nodes) != 0 {
			t.Fatalf("search retained deleted aliases: %+v %v", nodes, err)
		}
	}
	var bindings, fence int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM copy_bindings WHERE copy_id='upload'`).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("removed anti-revival binding: %d %v", bindings, err)
	}
	if err := s.db.QueryRow(`SELECT value FROM copy_cleanup_fence WHERE id=1`).Scan(&fence); err != nil || fence != 1 {
		t.Fatalf("no persistent absence write: %d %v", fence, err)
	}
	// A retry with stale nodes is not permission to delete replacement inodes.
	if err := s.RemoveLocalVersion(ctx, r); !errors.Is(err, ErrLocalVersionChanged) {
		t.Fatalf("accepted stale snapshots: %v", err)
	}
	r.Nodes = nil
	if err := s.RemoveLocalVersion(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT value FROM copy_cleanup_fence WHERE id=1`).Scan(&fence); err != nil || fence != 0 {
		t.Fatalf("empty retry omitted durable write: %d %v", fence, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if nodes, err := reopened.Aliases(ctx, r.Remote, r.RemoteID); err != nil || len(nodes) != 0 {
		t.Fatalf("aliases returned after reopen: %+v %v", nodes, err)
	}
	if err := reopened.RemoveLocalVersion(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveLocalVersionRejectsIncompleteOrChangedAuthorization(t *testing.T) {
	for _, change := range []string{"identity", "remote-id", "version", "size", "duplicate", "missing-alias", "extra-alias", "other-remote", "renamed", "replaced", "clean", "directory", "child", "deleted", "missing-fence"} {
		t.Run(change, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := t.Context()
			r := localCleanupFixture(t, s)
			var err error
			switch change {
			case "identity":
				r.StoreIdentity = "another-store"
			case "remote-id":
				r.RemoteID = "remote-object"
			case "version":
				r.Version = "newer-version"
			case "size":
				r.Size++
			case "duplicate":
				r.Nodes = append(r.Nodes, r.Nodes[0])
			case "missing-alias":
				r.Nodes = r.Nodes[:1]
			case "extra-alias", "other-remote":
				n := r.Nodes[0]
				n.Ino, n.Name = 0, "unapproved"
				if change == "other-remote" {
					n.Remote = "other"
				}
				_, err = s.Insert(ctx, n)
			case "renamed":
				err = s.Rename(ctx, r.Nodes[1].Ino, RootIno, "renamed")
			case "replaced":
				_, err = s.db.Exec(`UPDATE nodes SET remote_id='remote-object',version='new-version',dirty=0 WHERE ino=?`, r.Nodes[1].Ino)
			case "clean":
				_, err = s.db.Exec(`UPDATE nodes SET dirty=0 WHERE ino=?`, r.Nodes[1].Ino)
			case "directory":
				_, err = s.db.Exec(`UPDATE nodes SET kind=? WHERE ino=?`, provider.KindDir, r.Nodes[1].Ino)
			case "child":
				// Direct SQL simulates a malformed stored file-with-children.
				_, err = s.db.Exec(`INSERT INTO nodes(parent_ino,name,kind) VALUES (?,'unrelated-child',?)`, r.Nodes[1].Ino, provider.KindFile)
			case "deleted":
				err = s.Remove(ctx, r.Nodes[1].Ino)
			case "missing-fence":
				_, err = s.db.Exec(`DELETE FROM copy_cleanup_fence`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveLocalVersion(ctx, r); !errors.Is(err, ErrLocalVersionChanged) {
				t.Fatalf("unsafe cleanup accepted %s: %v", change, err)
			}
			// Even errors found after deleting the first alias must roll it back.
			if _, err := s.Get(ctx, r.Nodes[0].Ino); err != nil {
				t.Fatalf("partial cleanup escaped rollback: %v", err)
			}
		})
	}
}

func TestRemoveLocalVersionRollsBackDeletionIndexesAndListingFence(t *testing.T) {
	for _, phase := range []string{"delete", "fence"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := t.Context()
			r := localCleanupFixture(t, s)
			if err := s.FlushIndex(ctx); err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, RootIno)
			if err := l.Append(ctx, r.Nodes); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_cleanup BEFORE UPDATE ON copy_cleanup_fence BEGIN SELECT RAISE(ABORT,'injected failure'); END`
			if phase == "delete" {
				trigger = `CREATE TRIGGER fail_cleanup BEFORE DELETE ON nodes WHEN old.name='second-alias' BEGIN SELECT RAISE(ABORT,'injected failure'); END`
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveLocalVersion(ctx, r); err == nil {
				t.Fatal("cleanup failure ignored")
			}
			for _, n := range r.Nodes {
				if _, err := s.Get(ctx, n.Ino); err != nil {
					t.Fatal(err)
				}
				if absent, err := s.IsAbsent(ctx, n.ParentIno, n.Name); err != nil || absent {
					t.Fatalf("failed delete left negative cache: %v", err)
				}
			}
			if results, err := s.Search(ctx, "alias", 10); err != nil || len(results) != 2 {
				t.Fatalf("rollback lost indexes: %+v %v", results, err)
			}
			if err := l.Commit(ctx, time.Minute, nil); err != nil {
				t.Fatalf("failed cleanup left a listing fence: %v", err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER fail_cleanup`); err != nil {
				t.Fatal(err)
			}
			var err error
			r.Nodes, err = s.Aliases(ctx, r.Remote, r.RemoteID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveLocalVersion(ctx, r); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoveLocalVersionAndConcurrentRenameHaveOneWinner(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, _ := openTest(t)
		ctx := t.Context()
		r := localCleanupFixture(t, s)
		other, err := Open(s.path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		removed, renamed := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			removed <- s.RemoveLocalVersion(ctx, r)
		}()
		go func() {
			<-start
			renamed <- other.Rename(ctx, r.Nodes[1].Ino, RootIno, "renamed")
		}()
		close(start)
		removeErr, renameErr := <-removed, <-renamed
		if err := other.Close(); err != nil {
			t.Fatal(err)
		}
		if removeErr == nil {
			if !errors.Is(renameErr, ErrNotFound) {
				t.Fatalf("rename bypassed removal: %v", renameErr)
			}
			if nodes, err := s.Aliases(ctx, r.Remote, r.RemoteID); err != nil || len(nodes) != 0 {
				t.Fatalf("successful removal retained aliases: %+v %v", nodes, err)
			}
		} else {
			if !errors.Is(removeErr, ErrLocalVersionChanged) || renameErr != nil {
				t.Fatalf("unexpected race result: remove=%v rename=%v", removeErr, renameErr)
			}
			if n, err := s.Lookup(ctx, RootIno, "renamed"); err != nil || n.Ino != r.Nodes[1].Ino {
				t.Fatalf("removed the renamed version: %+v %v", n, err)
			}
			if _, err := s.Get(ctx, r.Nodes[0].Ino); err != nil {
				t.Fatalf("partial deletion after lost race: %v", err)
			}
		}
	}
}
