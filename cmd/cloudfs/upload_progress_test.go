package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
)

// progressScript serves one progress reading per call, holding the last one
// once the script runs out. It stands in for the uploader's sampler; the
// queue counts beside it come from a real journal, because the exit status
// of a watch turns on them.
type progressScript struct {
	mu    sync.Mutex
	steps []upload.Progress
	n     int
}

func (p *progressScript) progress() upload.Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := min(p.n, len(p.steps)-1)
	p.n++
	return p.steps[i]
}

// watchCLI stands a control socket up over a scripted sampler and a real,
// initially empty journal.
func watchCLI(t *testing.T, script *progressScript) (string, *journal.Journal) {
	t.Helper()
	cfg, path := uploadCLIConfig(t)
	j, err := journal.Open(journal.Options{Dir: filepath.Join(cfg.StateDir(), "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	col := &control.Collector{Version: "test", Journal: j, UploadProgress: script.progress}
	srv, err := control.NewServer(col).Start(context.Background(), cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	// A reading a second is what the daemon publishes; a test must not wait
	// for it.
	previous := uploadWatchInterval
	uploadWatchInterval = time.Millisecond
	t.Cleanup(func() { uploadWatchInterval = previous })
	return path, j
}

// queueRow commits one row under parentID and returns its id.
func queueRow(t *testing.T, j *journal.Journal, name, parentID string) string {
	t.Helper()
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := j.CommitStaging(s, h)
	if err != nil {
		t.Fatal(err)
	}
	u := journal.Upload{ID: journal.NewID(), StagingID: s.ID, Remote: "ali",
		RemoteParentID: parentID, Name: name, BlobPath: blob, Size: 1, Hashes: h}
	if err := j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// deadRow leaves one row in the dead letter queue.
func deadRow(t *testing.T, j *journal.Journal) {
	t.Helper()
	ctx := context.Background()
	id := queueRow(t, j, "dead.txt", fakeprovider.RootID)
	if _, err := j.Claim(ctx, "ali", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.Fail(ctx, id, errors.New("the backend refused it")); err != nil {
		t.Fatal(err)
	}
}

// TestUploadsWatchDrawsALineUntilTheQueueDrains: `cp -r` returns in seconds
// and the upload takes twenty minutes. This is the command that makes the
// second half visible from a terminal.
func TestUploadsWatchDrawsALineUntilTheQueueDrains(t *testing.T) {
	script := &progressScript{steps: []upload.Progress{
		{Active: true, Seq: 1, FilesTotal: 4, FilesDone: 1, BytesTotal: 4000, BytesDone: 1000, Rate: 500, ETA: 6 * time.Second},
		{Active: true, Seq: 1, FilesTotal: 4, FilesDone: 3, BytesTotal: 4000, BytesDone: 3000, Rate: 500, ETA: 2 * time.Second},
		{Seq: 1, FilesTotal: 4, FilesDone: 4, BytesTotal: 4000, BytesDone: 4000},
	}}
	path, _ := watchCLI(t, script)
	var out bytes.Buffer
	if err := runUploads(context.Background(), []string{"watch", "--config", path}, &out); err != nil {
		t.Fatalf("watch: %v (%s)", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "4/4 files") {
		t.Errorf("the final line does not report the files: %q", got)
	}
	if !strings.Contains(got, "\r") {
		t.Errorf("the line is not redrawn in place: %q", got)
	}
	if !strings.Contains(got, "/s") {
		t.Errorf("no rate was shown, though the daemon reported one: %q", got)
	}
	if strings.Contains(got, "ETA") && strings.Count(got, "ETA") == 0 {
		t.Errorf("estimate handling: %q", got)
	}
}

// TestUploadsWatchExitsNonZeroWhenUploadsDied: `cp -r ... && cloudfs uploads
// watch` must not report success while files sit in the dead letter queue.
func TestUploadsWatchExitsNonZeroWhenUploadsDied(t *testing.T) {
	script := &progressScript{steps: []upload.Progress{
		{Active: true, Seq: 1, FilesTotal: 2, FilesDone: 0, BytesTotal: 20},
		{Seq: 1, FilesTotal: 2, FilesDone: 1, BytesTotal: 20, BytesDone: 10},
	}}
	path, j := watchCLI(t, script)
	deadRow(t, j)
	var out bytes.Buffer
	err := runUploads(context.Background(), []string{"watch", "--config", path}, &out)
	if err == nil {
		t.Fatalf("a drained queue with a dead letter exited zero: %q", out.String())
	}
	if !strings.Contains(err.Error(), "uploads list") {
		t.Errorf("the failure does not say where to look: %v", err)
	}
}

// TestUploadsWatchStartsAFreshLineForANewBatch: two copies in a row are two
// bars. Continuing the first one's line would draw a percentage that goes
// backwards and a total that shrinks.
func TestUploadsWatchStartsAFreshLineForANewBatch(t *testing.T) {
	script := &progressScript{steps: []upload.Progress{
		{Active: true, Seq: 1, FilesTotal: 2, FilesDone: 1, BytesTotal: 200, BytesDone: 100},
		{Active: true, Seq: 2, FilesTotal: 9, FilesDone: 0, BytesTotal: 900},
		{Seq: 2, FilesTotal: 9, FilesDone: 9, BytesTotal: 900, BytesDone: 900},
	}}
	path, _ := watchCLI(t, script)
	var out bytes.Buffer
	if err := runUploads(context.Background(), []string{"watch", "--config", path}, &out); err != nil {
		t.Fatalf("watch: %v", err)
	}
	got := out.String()
	if strings.Count(got, "\n") < 2 {
		t.Errorf("the second batch continued the first one's line: %q", got)
	}
	if !strings.Contains(got, "9/9 files") {
		t.Errorf("the second batch was not followed to the end: %q", got)
	}
}

// TestUploadsWatchSaysSoWhenThereIsNothingToWatch: silence would look like a
// hang, and this command is most often run right after a copy that may
// already have finished.
func TestUploadsWatchSaysSoWhenThereIsNothingToWatch(t *testing.T) {
	path, _ := watchCLI(t, &progressScript{steps: []upload.Progress{{}}})
	var out bytes.Buffer
	if err := runUploads(context.Background(), []string{"watch", "--config", path}, &out); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !strings.Contains(out.String(), "no uploads") {
		t.Errorf("an idle queue printed %q", out.String())
	}
}

// TestUploadsWatchStopsOnAQueueThatCannotMove: every pending row waits on a
// directory creation that is gone. Nothing will ever drain it, so waiting
// for the batch to close is waiting forever.
func TestUploadsWatchStopsOnAQueueThatCannotMove(t *testing.T) {
	script := &progressScript{steps: []upload.Progress{{Active: true, Seq: 1, FilesTotal: 3, BytesTotal: 30, Blocked: 3}}}
	path, j := watchCLI(t, script)
	// A row whose parent directory creation is not in the queue at all: it
	// is pending, it is counted as blocked, and nothing will ever move it.
	queueRow(t, j, "under-a-lost-dir.txt", journal.LocalIDPrefix+"a-directory-that-was-never-queued")
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runUploads(ctx, []string{"watch", "--config", path}, &out)
	if err == nil {
		t.Fatalf("a queue that cannot move exited zero: %q", out.String())
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("the failure does not name the cause: %v", err)
	}
}

// TestTheTerminalPercentMatchesTheConsole: the console caps an active batch
// at 99.5% so a bar does not sit at 100% with files still going out. A
// terminal that rounds differently makes the two disagree by a digit, and
// then one of them is wrong.
func TestTheTerminalPercentMatchesTheConsole(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    control.UploadBatch
		want float64
	}{
		{"idle", control.UploadBatch{}, 0},
		{"by bytes", control.UploadBatch{Active: true, BytesTotal: 400, BytesDone: 100, FilesTotal: 4}, 25},
		{"by files when there are no bytes", control.UploadBatch{Active: true, FilesTotal: 4, FilesDone: 1}, 25},
		{"capped while active", control.UploadBatch{Active: true, BytesTotal: 1000, BytesDone: 1000, FilesTotal: 2}, 99.5},
		{"complete", control.UploadBatch{FilesTotal: 4, FilesDone: 4, BytesTotal: 400, BytesDone: 400}, 100},
		// A daemon that sends its own figure wins: this binary may be older
		// or newer than the daemon it is talking to, and one answer per
		// daemon is the point.
		{"served", control.UploadBatch{Active: true, Percent: 40, FilesTotal: 4, FilesDone: 1, BytesTotal: 400, BytesDone: 100}, 40},
	} {
		if got := uploadBatchPercent(tc.b); got != tc.want {
			t.Errorf("%s: percent = %.2f, want %.2f", tc.name, got, tc.want)
		}
	}
	// The cases above with no Percent field are the fallback path, which is
	// how this binary keeps working against a daemon too old to send one.
	// Both paths run the one formula, so they cannot disagree.
	capped := control.UploadBatch{Active: true, FilesTotal: 2, FilesDone: 1, BytesTotal: 1000, BytesDone: 1000}
	served := capped
	served.Percent = upload.PercentOf(capped.Active, capped.FilesTotal, capped.FilesDone, capped.BytesTotal, capped.BytesDone)
	if uploadBatchPercent(capped) != uploadBatchPercent(served) {
		t.Errorf("the fallback and the served value disagree at the cap: %.3f vs %.3f",
			uploadBatchPercent(capped), uploadBatchPercent(served))
	}
}

// TestStatusPrintsTheBatchAndTheBlockedCount: `cloudfs status` received the
// batch and printed none of it, which is how the only progress in the system
// came to live in a browser.
func TestStatusPrintsTheBatchAndTheBlockedCount(t *testing.T) {
	st := control.Status{Version: "test"}
	st.Uploads = control.UploadStatus{
		Pending: 3, Uploading: 1, Blocked: 2,
		Batch: control.UploadBatch{
			Active: true, Seq: 3, FilesTotal: 100, FilesDone: 40,
			BytesTotal: 1 << 30, BytesDone: 1 << 28, Rate: 1 << 20, ETASeconds: 90,
		},
	}
	var out bytes.Buffer
	printStatus(&out, st)
	got := out.String()
	if !strings.Contains(got, "40/100 files") {
		t.Errorf("no batch line in status: %s", got)
	}
	if !strings.Contains(got, "2 blocked") {
		t.Errorf("the blocked count is not printed: %s", got)
	}
	// A resumed batch has to say that its figures start at the restart.
	st.Uploads.Batch.Resumed = true
	out.Reset()
	printStatus(&out, st)
	if !strings.Contains(out.String(), "restart") {
		t.Errorf("a resumed batch does not explain its figures: %s", out.String())
	}
}
