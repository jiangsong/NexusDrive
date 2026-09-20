package control

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// batchContractKeys are the names the console reads off the status document.
// They are a published interface between Go and JavaScript, and renaming one
// breaks the transfers page silently: the bar simply stops moving, because
// `b.files_done || 0` is a perfectly good zero.
var batchContractKeys = []string{
	"active", "started_at", "finished_at",
	"files_total", "files_done", "bytes_total", "bytes_done",
}

// TestTheBatchJSONKeysAreAContract pins those seven names. It passed before
// the progress model moved down into internal/upload and must keep passing
// after: the shape crossing the wire is the part that is not ours to change.
func TestTheBatchJSONKeysAreAContract(t *testing.T) {
	// Every field set, so the omitempty strings are on the wire too.
	raw, err := json.Marshal(UploadBatch{
		Active: true, StartedAt: "2026-09-20T10:00:00Z", FinishedAt: "2026-09-20T10:01:00Z",
		FilesTotal: 5, FilesDone: 2, BytesTotal: 500, BytesDone: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range batchContractKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("the batch no longer carries %q; internal/control/web/transfer_progress.js reads it", key)
		}
	}
}

// TestTheConsoleStillReadsTheContractKeys is the other half: a key kept in Go
// that the page stopped reading is just as much a broken contract, and the
// page is the reason the names are frozen at all.
func TestTheConsoleStillReadsTheContractKeys(t *testing.T) {
	src, err := os.ReadFile("web/transfer_progress.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range batchContractKeys {
		if key == "started_at" || key == "finished_at" {
			// The page renders times from elsewhere; only the numbers and the
			// flag drive the bar.
			continue
		}
		if !strings.Contains(string(src), key) {
			t.Errorf("transfer_progress.js no longer mentions %q", key)
		}
	}
	// The page prefers the percentage the daemon sends, and keeps its own
	// formula for a daemon that sends none. Dropping that fallback would
	// break a new console against an old daemon, silently and only in the
	// one number this whole surface exists to show.
	if !strings.Contains(string(src), "b.percent") {
		t.Error("transfer_progress.js no longer prefers the daemon's own percentage")
	}
	if !strings.Contains(string(src), "99.5") {
		t.Error("transfer_progress.js lost its fallback cap; an old daemon would drive the bar past 99.5%")
	}
}
