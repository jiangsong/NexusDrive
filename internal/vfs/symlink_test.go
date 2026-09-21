package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

func TestSymlinkRestartAndCopy(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	target := "../" + strings.Repeat("directory/", 25) + "missing"
	a, err := e.fs.Symlink(ctx, e.nodeOf(t, "/ali").Ino, "link", target)
	if err != nil {
		t.Fatal(err)
	}
	// Reopen persisted metadata and use an empty cache. Recovery must restore
	// the acknowledged link from the journal without contacting the provider.
	store, err := meta.Open(filepath.Join(e.dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{Dir: t.TempDir(), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	opt := e.fs.opt
	opt.Meta, opt.Cache = store, ca
	fs, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	fs.SetWriteBackend(e.j, nil)
	before := e.fake.TotalCalls()
	if err := fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	if got, err := fs.Readlink(ctx, a.Ino); err != nil || string(got) != target {
		t.Fatalf("restart: %q %v", got, err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("restart fetched unuploaded target")
	}
	copied, err := fs.Copy(ctx, "/ali/link", "/ali/copied")
	if err != nil || !copied.IsSymlink {
		t.Fatalf("copy: %+v %v", copied, err)
	}
	if got, err := fs.Readlink(ctx, copied.Ino); err != nil || string(got) != target {
		t.Fatalf("copy target: %q %v", got, err)
	}
}

func TestSymlinkCollisionDoesNotOverwrite(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("same", []byte("ordinary file"))
	e.fake.Seed("same.rclonelink", []byte("target"))
	root := e.nodeOf(t, "/ali")
	if err := e.fs.fetchDir(ctx, e.mount(), root.Ino, root, false); err == nil {
		t.Fatal("ambiguous namespace accepted")
	}
	if children, err := e.store.Children(ctx, root.Ino); err != nil || len(children) != 0 {
		t.Fatalf("partial listing published: %+v %v", children, err)
	}
	if got, _ := e.fake.Content("same"); string(got) != "ordinary file" {
		t.Fatal("ordinary file changed")
	}
}

func TestSymlinkReplacementQueuesVictimBeforeUpload(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.up.Stop()
	ctx := context.Background()
	e.fake.Seed("dest", []byte("remote victim"))
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Symlink(ctx, root.Ino, "source", "target"); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "source", root.Ino, "dest"); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].IsDelete() || rows[0].Name != "dest.rclonelink" {
		t.Fatalf("claim = %+v; want only the victim delete ordered as dest.rclonelink", rows)
	}
}

func TestSymlinkCancelAndDiscard(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	a, err := e.fs.Symlink(ctx, e.nodeOf(t, "/ali").Ino, "link", "target")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.ByIno(ctx, a.Ino)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	id := rows[0].ID
	if _, err := e.fs.CancelUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.DiscardUpload(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(ctx, a.Ino); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("discard left link: %v", err)
	}
}

func TestSymlinkRoundTrip(t *testing.T) {
	for _, pathIDs := range []bool{false, true} {
		t.Run(map[bool]string{false: "opaque", true: "path"}[pathIDs], func(t *testing.T) {
			e := newEnv(t, envOpt{pathIDs: pathIDs})
			ctx := context.Background()
			root := e.nodeOf(t, "/ali")
			a, err := e.fs.Symlink(ctx, root.Ino, "vite", "../vite/bin/vite.js")
			if err != nil {
				t.Fatal(err)
			}
			if !a.IsSymlink || a.IsDir || a.Size != int64(len("../vite/bin/vite.js")) {
				t.Fatalf("attr: %+v", a)
			}
			check := func(name string) {
				t.Helper()
				n := e.nodeOf(t, "/ali/"+name)
				if n.Kind != provider.KindSymlink {
					t.Fatalf("kind: %+v", n)
				}
				b, err := e.fs.Readlink(ctx, n.Ino)
				if err != nil || string(b) != "../vite/bin/vite.js" {
					t.Fatalf("target %q: %v", b, err)
				}
			}
			check("vite")
			if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
				t.Fatal(err)
			}
			check("vite")
			if err := e.fs.Rename(ctx, root.Ino, "vite", root.Ino, "renamed"); err != nil {
				t.Fatal(err)
			}
			check("renamed")
			if _, err := e.up.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok := e.fake.IDOf("renamed.rclonelink"); !ok {
				t.Fatal("encoded object missing")
			}
			if err := e.fs.Refresh(ctx, root.Ino); err != nil {
				t.Fatal(err)
			}
			check("renamed")
			if err := e.fs.Rename(ctx, root.Ino, "renamed", root.Ino, "final"); err != nil {
				t.Fatal(err)
			}
			check("final")
			if err := e.fs.Refresh(ctx, root.Ino); err != nil {
				t.Fatal(err)
			}
			check("final")
			if err := e.fs.Remove(ctx, root.Ino, "final", false); err != nil {
				t.Fatal(err)
			}
			if _, err := e.up.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok := e.fake.IDOf("final.rclonelink"); ok {
				t.Fatal("encoded object survived unlink")
			}
		})
	}
}

func TestSymlinkRemoteDiscovery(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("link.rclonelink", []byte("../missing"))
	if _, err := e.fs.StatPath(ctx, "/ali/link"); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/link")
	b, err := e.fs.Readlink(ctx, n.Ino)
	if err != nil || string(b) != "../missing" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := e.fs.Open(ctx, n.Ino, true); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("write link: %v", err)
	}
	if _, err := e.fs.Lookup(ctx, n.ParentIno, "link.rclonelink"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wire name visible: %v", err)
	}
}

func TestSymlinkValidation(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	root := e.nodeOf(t, "/ali")
	for _, target := range []string{"", "bad\x00target", strings.Repeat("x", maxLinkTarget+1)} {
		if _, err := e.fs.Symlink(ctx, root.Ino, "link", target); err == nil {
			t.Fatalf("accepted target %q", target)
		}
	}
	for _, name := range []string{"x.rclonelink", ".", "..", "a/b"} {
		if _, err := e.fs.Create(ctx, root.Ino, name); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	e.fake.Seed("bad.rclonelink", []byte("x\x00y"))
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/bad")
	if _, err := e.fs.Readlink(ctx, n.Ino); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("malformed link: %v", err)
	}
}

func TestSymlinkReadonly(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeReadonly})
	if _, err := e.fs.Symlink(context.Background(), e.nodeOf(t, "/ali").Ino, "link", "target"); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
}

func TestSymlinkInterruptedPublication(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	fault := errors.New("simulated publication failure")
	e.fs.commitFault = func() error { return fault }
	if _, err := e.fs.Symlink(ctx, e.nodeOf(t, "/ali").Ino, "link", "target"); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if rows, err := e.j.Claim(ctx, "ali", 10); err != nil || len(rows) != 0 {
		t.Fatalf("unpublished link uploaded: %v %v", rows, err)
	}
	e.fs.commitFault = nil
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/link")
	if got, err := e.fs.Readlink(ctx, n.Ino); err != nil || string(got) != "target" {
		t.Fatalf("recovery: %q %v", got, err)
	}
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("link.rclonelink"); !ok || string(got) != "target" {
		t.Fatalf("remote %q %v", got, ok)
	}
}

func TestSymlinkFailedStagingLeavesNameAvailable(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	root := e.nodeOf(t, "/ali")
	stage := e.j.StagingDir()
	if err := os.Rename(stage, stage+".saved"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Symlink(ctx, root.Ino, "link", "target"); err == nil {
		t.Fatal("creation succeeded without staging")
	}
	if _, err := e.store.Lookup(ctx, root.Ino, "link"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("failed link occupied name: %v", err)
	}
	if err := os.Rename(stage+".saved", stage); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Symlink(ctx, root.Ino, "link", "target"); err != nil {
		t.Fatal(err)
	}
}
