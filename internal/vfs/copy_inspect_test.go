package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/test/fakeprovider"
)

func TestCopyInspectionRejectsRemountsAndShadowedPathsWithoutIO(t *testing.T) {
	e := publicationEnv(t, t.TempDir())
	ctx := context.Background()
	e.fake.Seed("source", []byte("content"))
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	id := jobs[0].ID
	before := e.fake.TotalCalls()
	info, err := e.fs.InspectCopy(ctx, id)
	if err != nil || info.ID != id || info.State != journal.CopySubmitted || info.UploadID != id {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	original := append([]Mount(nil), e.fs.mounts...)
	defer func() { e.fs.mounts = original }()
	for _, mutate := range []func(){
		func() { e.fs.mounts[0].RootID = "another-root" },
		func() { e.fs.mounts[0].Remote = "another-account" },
		func() { e.fs.mounts[0].AccountBinding = "another-generation" },
		func() {
			shadow := e.fs.mounts[0]
			shadow.Prefix = "/ali/source"
			e.fs.mounts = append([]Mount{shadow}, e.fs.mounts...)
		},
		func() {
			shadow := e.fs.mounts[0]
			shadow.Prefix = "/ali/dest"
			e.fs.mounts = append([]Mount{shadow}, e.fs.mounts...)
		},
	} {
		e.fs.mounts = append([]Mount(nil), original...)
		mutate()
		if _, err := e.fs.InspectCopy(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old namespace job exposed: %v", err)
		}
		page, next, err := e.fs.InspectCopyPage(ctx, "", 10)
		if err != nil || len(page) != 0 || next != "" {
			t.Fatalf("old namespace job listed: %+v %q %v", page, next, err)
		}
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("inspection queried provider")
	}
}

func TestCopyInspectionMissingJournalAndLimits(t *testing.T) {
	e := publicationEnv(t, t.TempDir())
	ctx := context.Background()
	for _, limit := range []int{-1, 0, 1001} {
		if _, _, err := e.fs.InspectCopyPage(ctx, "", limit); err == nil {
			t.Fatal("invalid scan limit accepted")
		}
	}
	e.fs.SetWriteBackend(nil, nil)
	if _, err := e.fs.InspectCopy(ctx, journal.NewID()); err == nil {
		t.Fatal("inspection without journal succeeded")
	}
	if _, _, err := e.fs.InspectCopyPage(ctx, "", 10); err == nil {
		t.Fatal("listing without journal succeeded")
	}
}

func TestLegacyCopyWithoutAccountBindingsIsNotVisible(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	identity, err := e.store.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.j.BeginCopy(ctx, journal.CopySpec{
		MetaIdentity: identity, SourceMount: "/ali", SourceRootID: fakeprovider.RootID,
		TargetMount: "/ali", TargetRootID: fakeprovider.RootID,
		SourcePath: "/ali/source", SourceRemote: "ali", SourceID: "source",
		TargetPath: "/ali/dest", TargetRemote: "ali", TargetParentID: "root", Mode: "writeback",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	before := e.fake.TotalCalls()
	if _, err := e.fs.InspectCopy(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy copy visible: %v", err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("legacy inspection queried provider")
	}
}

func TestCopyRecoveryDoesNotFollowSourceOutsideRecordedPath(t *testing.T) {
	dir := killCopyPreparation(t, "download")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	e.fake.Seed("source", copyRecoveryBody())
	source := e.nodeOf(t, "/ali/source")
	if err := e.fs.Rename(ctx, source.ParentIno, source.Name, source.ParentIno, "private-source"); err != nil {
		t.Fatal(err)
	}
	before := e.fake.ReadBytes()
	if _, err := e.fs.ResumeCopy(ctx, job.ID); !errors.Is(err, ErrCopySourceChanged) {
		t.Fatalf("recovery followed source ID outside its recorded path: %v", err)
	}
	if e.fake.ReadBytes() != before {
		t.Fatal("moved source contents downloaded")
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.Checkpoint != job.Checkpoint {
		t.Fatalf("failed recovery lost retained checkpoint: %+v %v", current, err)
	}
}

func TestReadyCopyRecoveryDoesNotRequireSourcePath(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	source, err := e.store.Resolve(ctx, "/ali/source")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Remove(ctx, source.Ino); err != nil {
		t.Fatal(err)
	}
	// The fresh provider also has no source object. Complete retained bytes
	// must still publish without another source lookup or download.
	if _, err := e.fs.ResumeCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if e.fake.ReadBytes() != 0 {
		t.Fatal("ready recovery downloaded source")
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopySubmitted {
		t.Fatalf("ready copy did not submit: %+v %v", current, err)
	}
}
