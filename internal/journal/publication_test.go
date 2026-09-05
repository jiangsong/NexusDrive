package journal

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnpublishedCannotBeClaimedUntilPublished(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	u := stage(t, j, "unpublished", []byte("body"))
	u.NeedsPublish = true
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if rows, err := j.Claim(ctx, u.Remote, 10); err != nil || len(rows) != 0 {
		t.Fatalf("claimed unpublished: %v %v", rows, err)
	}
	if rows, err := j.Unpublished(ctx); err != nil || len(rows) != 1 || !rows[0].NeedsPublish {
		t.Fatalf("missing intent: %v %v", rows, err)
	}
	for range 2 {
		if err := j.MarkPublished(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := j.Claim(ctx, u.Remote, 10); err != nil || len(rows) != 1 || rows[0].NeedsPublish {
		t.Fatalf("published not claimed: %v %v", rows, err)
	}
}

func TestPublicationMigrationAndReadOnlyV3(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "journal.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(journalSchema, ",\n  needs_publish    INTEGER NOT NULL DEFAULT 0", "", 1)
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO uploads (id,remote,remote_parent_id,name,blob_path,size,state,created_at) VALUES ('old','ali','root','old','unused',0,'pending',1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx := context.Background()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := ro.Get(ctx, "old"); err != nil || u.NeedsPublish {
		t.Fatalf("v3 read: %+v %v", u, err)
	}
	if rows, _, err := ro.ListActive(ctx, "", 10); err != nil || len(rows) != 1 {
		t.Fatalf("v3 list: %v %v", rows, err)
	}
	if jobs, err := ro.CopyJobs(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("legacy copy inspection: %v %v", jobs, err)
	}
	if jobs, next, err := ro.ListCopyJobs(ctx, "", 1); err != nil || len(jobs) != 0 || next != "" {
		t.Fatalf("legacy copy pagination: %v %q %v", jobs, next, err)
	}
	var v int
	if err := ro.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 {
		t.Fatalf("read-only migrated: %d %v", v, err)
	}
	ro.Close()
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if u, err := j.Get(ctx, "old"); err != nil || u.NeedsPublish {
		t.Fatalf("migration changed legacy row: %+v %v", u, err)
	}
	if err := j.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != journalSchemaVersion {
		t.Fatalf("migration: %d %v", v, err)
	}
}
