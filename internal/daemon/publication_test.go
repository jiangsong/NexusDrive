package daemon

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

func TestStartupPublishesInterruptedWriteBeforeReturning(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := d.Meta.Resolve(ctx, "/demo")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	h, err := d.FS.Create(ctx, parent.Ino, "interrupted")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	// Persist exactly the boundary reached by commitWrite before publishing
	// metadata. No live write handle or cache link survives into the new stack.
	s, err := d.Journal.NewStaging([]provider.HashType{provider.HashSHA1})
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("durable intent"), 0); err != nil {
		d.Close()
		t.Fatal(err)
	}
	hashes, err := s.Hashes()
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	blob, err := d.Journal.CommitStaging(s, hashes)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	identity, err := d.Meta.Identity(ctx)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	binding, err := config.EffectiveAccountBinding(d.Config.Remotes["demo"])
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	u := journal.Upload{ID: journal.NewID(), StagingID: s.ID, Remote: "demo", RemoteParentID: "root", Name: "interrupted", Ino: h.Ino, BlobPath: blob, Size: 14, Hashes: hashes, NeedsPublish: true,
		MetaIdentity: identity, MountPrefix: "/demo", MountRootID: "root", AccountBinding: binding}
	if err := d.Journal.Commit(ctx, u); err != nil {
		d.Close()
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.FS.ReadFileRange(ctx, "/demo/interrupted", 0, 14)
	if err != nil || string(got) != "durable intent" {
		t.Fatalf("startup read=%q %v", got, err)
	}
	row, err := d.Journal.Get(ctx, u.ID)
	if err != nil || row.NeedsPublish || row.State != journal.StatePending {
		t.Fatalf("startup gate=%+v %v", row, err)
	}
	if _, err := d.Uploader.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := d.Meta.Resolve(ctx, "/demo/interrupted")
	if err != nil || n.Dirty || vfs.IsLocalOnly(n.RemoteID) {
		t.Fatalf("final node=%+v %v", n, err)
	}
}
