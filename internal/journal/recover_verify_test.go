package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/provider"
)

// commitBlob stages content and commits a pending upload for it.
func commitBlob(t *testing.T, j *Journal, name string, content []byte) Upload {
	t.Helper()
	st, err := j.NewStaging(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteAt(content, 0); err != nil {
		t.Fatal(err)
	}
	hashes, err := st.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	if hashes[provider.HashCRC32C] == "" {
		t.Fatal("staging did not compute the recovery checksum")
	}
	blob, err := j.CommitStaging(st, hashes)
	if err != nil {
		t.Fatal(err)
	}
	u := Upload{ID: NewID(), StagingID: st.ID, Remote: "r", RemoteParentID: "root", Name: name, BlobPath: blob,
		Size: int64(len(content)), Hashes: hashes, Ino: 7}
	if err := j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestRecoverDeadLettersBlobsThatDoNotMatchTheirRow: recovery used to check
// only that the blob existed, so a blob a power loss had cut short was
// uploaded as the whole file. Size is checked in every mode; the checksum
// is read in crash mode, where a same-length torn blob is possible.
func TestRecoverDeadLettersBlobsThatDoNotMatchTheirRow(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "journal")
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ok := commitBlob(t, j, "ok.txt", []byte("all twelve b"))
	short := commitBlob(t, j, "short.txt", []byte("twelve bytes"))
	flipped := commitBlob(t, j, "flipped.txt", []byte("twelve bytes"))
	missing := commitBlob(t, j, "missing.txt", []byte("twelve bytes"))
	j.Close()
	if err := os.Truncate(short.BlobPath, 5); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(flipped.BlobPath)
	raw[3] ^= 0xff
	if err := os.WriteFile(flipped.BlobPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(missing.BlobPath)

	// Power mode: size and existence, but no read of every blob.
	j, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lost := map[string]bool{}
	for _, id := range rec.Lost {
		lost[id] = true
	}
	if lost[ok.ID] || !lost[short.ID] || !lost[missing.ID] {
		t.Fatalf("power-mode recovery lost %v; want short and missing, not ok", rec.Lost)
	}
	if lost[flipped.ID] {
		t.Fatal("power mode should not read blobs it fsynced; a same-length change is not its concern")
	}
	reason := func(id string) string {
		u, err := j.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if u.State != StateDead {
			t.Fatalf("%s is %s, want dead", id, u.State)
		}
		return u.LastError
	}
	if r := reason(short.ID); !strings.Contains(r, "truncated") || !strings.Contains(r, "5 of 12") {
		t.Fatalf("short blob reason = %q", r)
	}
	if r := reason(missing.ID); !strings.Contains(r, "missing") {
		t.Fatalf("missing blob reason = %q", r)
	}
	j.Close()

	// Crash mode: the checksum catches the torn blob too.
	j, err = Open(Options{Dir: dir, Durability: DurabilityCrash})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var mode int
	if err := j.db.QueryRow(`PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 1 {
		t.Fatalf("crash mode opened the journal with synchronous=%d, want 1 (NORMAL)", mode)
	}
	rec, err = j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 1 || rec.Lost[0] != flipped.ID {
		t.Fatalf("crash-mode recovery lost %v, want just the flipped blob", rec.Lost)
	}
	if r := reason(flipped.ID); !strings.Contains(r, "corrupt") || !strings.Contains(r, "crc32c") {
		t.Fatalf("flipped blob reason = %q", r)
	}
	if u, _ := j.Get(ctx, ok.ID); u.State != StatePending {
		t.Fatalf("the intact blob's row is %s, want pending", u.State)
	}
}

// TestDropPendingLeavesAClaimedRowForTheTombstone: between a delete's look
// at the queue and its drop, an uploader may claim the row. Dropping it then
// lets the transfer finish with nothing recording that the file was deleted,
// and the next listing brings the file back. The drop must refuse a claimed
// row so the caller tombstones it.
func TestDropPendingLeavesAClaimedRowForTheTombstone(t *testing.T) {
	ctx := context.Background()
	j, err := Open(Options{Dir: filepath.Join(t.TempDir(), "journal")})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	queued := commitBlob(t, j, "queued.txt", []byte("still pending"))
	claimed := commitBlob(t, j, "claimed.txt", []byte("on the wire"))
	rows, err := j.Claim(ctx, "r", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %v", rows, err)
	}
	if rows[0].ID != queued.ID && rows[0].ID != claimed.ID {
		t.Fatalf("claimed an unknown row %s", rows[0].ID)
	}
	inFlight, pending := rows[0].ID, queued.ID
	if inFlight == queued.ID {
		pending = claimed.ID
	}
	if err := j.DropPending(ctx, pending); err != nil {
		t.Fatalf("dropping a pending row: %v", err)
	}
	if _, err := j.Get(ctx, pending); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the pending row is still there: %v", err)
	}
	if err := j.DropPending(ctx, inFlight); !errors.Is(err, ErrInFlight) {
		t.Fatalf("dropping a claimed row: got %v, want ErrInFlight", err)
	}
	if u, err := j.Get(ctx, inFlight); err != nil || u.State != StateUploading {
		t.Fatalf("the claimed row must survive: %+v %v", u, err)
	}
}
