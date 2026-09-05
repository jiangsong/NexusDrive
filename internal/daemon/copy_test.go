package daemon

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/vfs"
)

func TestStartupResumesCopyOnlyWithBackgroundWork(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	// Every returned stack is closed, including on a failed assertion.
	t.Cleanup(func() {
		if d != nil {
			d.Close()
		}
	})
	parent, err := d.Meta.Resolve(ctx, "/demo")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := d.Meta.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accountBinding, err := config.EffectiveAccountBinding(d.Config.Remotes["demo"])
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Journal.BeginCopy(ctx, journal.CopySpec{
		MetaIdentity: identity, TargetParentIno: parent.Ino,
		SourcePath: "/demo/source", SourceRemote: "demo", SourceID: "source-id", SourceVersion: "source-version", SourceMount: "/demo", SourceRootID: "root", SourceAccountBinding: accountBinding,
		TargetPath: "/demo/dest", TargetRemote: "demo", TargetParentID: "root", TargetMount: "/demo", TargetRootID: "root", TargetAccountBinding: accountBinding, Size: 8, Mode: "writeback",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("prepared")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	d.Close()
	d, err = Open(ctx, Options{Config: cfg, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.Journal.GetCopy(ctx, id)
	if err != nil || job.State != journal.CopyReady {
		t.Fatalf("offline open resumed copy: %+v %v", job, err)
	}
	if rows, _, err := d.Journal.ListActive(ctx, "", 10); err != nil || len(rows) != 0 {
		t.Fatalf("offline open submitted upload: %+v %v", rows, err)
	}
	d.Close()
	d, err = Open(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for {
		n, err := d.Meta.Resolve(ctx, "/demo/dest")
		if err == nil && !n.Dirty && !vfs.IsLocalOnly(n.RemoteID) {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("daemon failed to resume/upload: %+v %v warning=%s", n, err, d.FS.CopyWarning())
		}
		time.Sleep(time.Millisecond)
	}
	got, err := d.FS.ReadFileRange(ctx, "/demo/dest", 0, 8)
	if err != nil || string(got) != "prepared" {
		t.Fatalf("resumed content: %q %v", got, err)
	}
}
