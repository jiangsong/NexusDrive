package journal

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func testUploadBinding() UploadBinding {
	return UploadBinding{MetaIdentity: "meta-1", MountPrefix: "/ali", MountRootID: "root-1", AccountBinding: "account-1"}
}

func TestUploadBindingRoundTripsAndRejectsPartialRows(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	u := stage(t, j, "bound", []byte("body"))
	b := testUploadBinding()
	u.MetaIdentity, u.MountPrefix, u.MountRootID, u.AccountBinding = b.MetaIdentity, b.MountPrefix, b.MountRootID, b.AccountBinding
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := j.Get(ctx, u.ID)
	if err != nil || got.MetaIdentity != b.MetaIdentity || got.MountPrefix != b.MountPrefix || got.MountRootID != b.MountRootID || got.AccountBinding != b.AccountBinding {
		t.Fatalf("binding round trip: %+v %v", got, err)
	}
	partial := stage(t, j, "partial", []byte("body"))
	partial.MetaIdentity = "meta-only"
	if err := j.Commit(ctx, partial); err == nil || !strings.Contains(err.Error(), "incomplete upload binding") {
		t.Fatalf("partial binding accepted: %v", err)
	}
}

func TestCommitBoundRebindsResumeAndPreservesOldBindingInHistory(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	old := testUploadBinding()
	if _, err := j.db.Exec("UPDATE uploads SET meta_identity=?,mount_prefix=?,mount_root_id=?,account_binding=? WHERE id=?",
		old.MetaIdentity, old.MountPrefix, old.MountRootID, old.AccountBinding, u.ID); err != nil {
		t.Fatal(err)
	}
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := UploadBinding{MetaIdentity: "meta-2", MountPrefix: "/ali", MountRootID: "root-2", AccountBinding: "account-2"}
	if err := p.CommitBound(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, err := j.Get(ctx, u.ID)
	if err != nil || got.State != StatePending || got.MetaIdentity != next.MetaIdentity || got.MountRootID != next.MountRootID || got.AccountBinding != next.AccountBinding {
		t.Fatalf("rebound row: %+v %v", got, err)
	}
	var saved int
	if err := j.db.QueryRow("SELECT count(*) FROM upload_resume_history WHERE upload_id=? AND snapshot LIKE '%account-1%'", u.ID).Scan(&saved); err != nil || saved != 1 {
		t.Fatalf("old binding history=%d %v", saved, err)
	}
}

func TestUploadBindingV10ReadOnlyCompatibilityAndOwnerMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := journalSchema
	for _, column := range []string{
		",\n  meta_identity   TEXT NOT NULL DEFAULT ''",
		",\n  mount_prefix     TEXT NOT NULL DEFAULT ''",
		",\n  mount_root_id    TEXT NOT NULL DEFAULT ''",
		",\n  account_binding  TEXT NOT NULL DEFAULT ''",
	} {
		old = strings.Replace(old, column, "", 1)
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 10"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO uploads (id,remote,remote_parent_id,name,blob_path,size,state,created_at) VALUES ('old','ali','root','old','unused',0,'pending',1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ro.Get(context.Background(), "old")
	if err != nil || got.MetaIdentity != "" || got.AccountBinding != "" {
		t.Fatalf("v10 read: %+v %v", got, err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var version, columns int
	if err := j.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := j.db.QueryRow("SELECT count(*) FROM pragma_table_info('uploads') WHERE name IN ('meta_identity','mount_prefix','mount_root_id','account_binding')").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if version != journalSchemaVersion || columns != 4 {
		t.Fatalf("migration version=%d columns=%d", version, columns)
	}
}
