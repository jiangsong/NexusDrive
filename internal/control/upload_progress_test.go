package control

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/i18n"
	"cloudfs/internal/upload"
)

// progressCollector is a collector wired to nothing but an upload progress
// source. That is the point of keeping the injected-function shape: a CLI or
// a metrics test can stand one up without a filesystem, a journal or a cache.
func progressCollector(p upload.Progress) *Collector {
	return &Collector{Version: "test", UploadProgress: func() upload.Progress { return p }}
}

// TestTheStatusBatchCarriesWhatTheUploaderMeasured: the collector no longer
// derives progress from two counters and a queue reading; it passes on what
// internal/upload sampled, including the fields that had no home before —
// the batch number, the resumed mark, the rate and the estimate.
func TestTheStatusBatchCarriesWhatTheUploaderMeasured(t *testing.T) {
	started := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := progressCollector(upload.Progress{
		Active: true, Seq: 4, Resumed: true, StartedAt: started,
		FilesTotal: 3434, FilesDone: 812, BytesTotal: 9_000_000, BytesDone: 2_000_000,
		Rate: 1_500_000, ETA: 90 * time.Second, Blocked: 2, InFlightBytes: 65536,
		FilesDoneTotal: 10812, BytesDoneTotal: 42_000_000,
	})
	st := c.Collect(context.Background(), i18n.EN)
	b := st.Uploads.Batch
	if !b.Active || b.Seq != 4 || !b.Resumed {
		t.Fatalf("batch identity: %+v", b)
	}
	if b.FilesTotal != 3434 || b.FilesDone != 812 || b.BytesTotal != 9_000_000 || b.BytesDone != 2_000_000 {
		t.Fatalf("batch figures: %+v", b)
	}
	if b.StartedAt != "2026-09-20T10:00:00Z" {
		t.Fatalf("started_at = %q", b.StartedAt)
	}
	if b.Rate != 1_500_000 || b.ETASeconds != 90 {
		t.Fatalf("rate and estimate: %+v", b)
	}
	if b.FilesDoneTotal != 10812 || b.BytesDoneTotal != 42_000_000 {
		t.Fatalf("lifetime totals: %+v", b)
	}
	// Without a journal to read them from, the queue's own figures come from
	// the same snapshot rather than being left at zero.
	if st.Uploads.Blocked != 2 || st.Uploads.InFlightBytes != 65536 {
		t.Fatalf("blocked and in-flight: %+v", st.Uploads)
	}
}

// TestTheNewBatchFieldsAreOnTheWire: the console reads the batch off /status,
// and so now do the CLI and the MCP tool. The new names have to serialize.
func TestTheNewBatchFieldsAreOnTheWire(t *testing.T) {
	c := progressCollector(upload.Progress{Active: true, Seq: 2, Resumed: true, Rate: 10, ETA: 5 * time.Second})
	raw, err := json.Marshal(c.Collect(context.Background(), i18n.EN).Uploads)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"blocked", "in_flight_bytes"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the upload status does not carry %q: %s", key, raw)
		}
	}
	var batch map[string]json.RawMessage
	if err := json.Unmarshal(got["batch"], &batch); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"seq", "resumed", "rate", "eta_seconds", "percent"} {
		if _, ok := batch[key]; !ok {
			t.Errorf("the batch does not carry %q: %s", key, got["batch"])
		}
	}
}

// TestThePercentOnTheWireIsTheOneFormula: the console used to work the
// percentage out in JavaScript from the same four numbers. Two hand-written
// copies of one formula drift, and the symptom — a browser and a terminal
// disagreeing by a digit about the same batch — is the exact class of silent
// divergence this surface exists to remove. The daemon now sends the number
// it computed; the page prefers it.
func TestThePercentOnTheWireIsTheOneFormula(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    upload.Progress
		want float64
	}{
		{"idle", upload.Progress{}, 0},
		{"by bytes", upload.Progress{Active: true, FilesTotal: 4, BytesTotal: 400, BytesDone: 100}, 25},
		{"by files without bytes", upload.Progress{Active: true, FilesTotal: 4, FilesDone: 1}, 25},
		// The cap is the case worth pinning: an active batch whose bytes have
		// all gone out still has files landing, and a full bar sitting there
		// is what makes a person stop trusting it.
		{"capped while active", upload.Progress{Active: true, FilesTotal: 2, FilesDone: 1, BytesTotal: 1000, BytesDone: 1000}, 99.5},
		{"complete", upload.Progress{FilesTotal: 4, FilesDone: 4, BytesTotal: 400, BytesDone: 400}, 100},
	} {
		got := progressCollector(tc.p).Collect(context.Background(), i18n.EN).Uploads.Batch.Percent
		if got != tc.want {
			t.Errorf("%s: percent on the wire = %.3f, want %.3f", tc.name, got, tc.want)
		}
		if got != tc.p.Percent() {
			t.Errorf("%s: the wire says %.3f and upload.PercentOf says %.3f", tc.name, got, tc.p.Percent())
		}
	}
}

// TestABlockedQueueWarnsAndAnActiveBatchDoesNot draws the line between a
// problem and a state.
//
// A blocked queue is a problem: those rows will never move on their own and
// the warning has to say what unblocks them. A batch in flight is not — a
// copy into the mount returns long before its bytes do, so it is the most
// ordinary condition this filesystem has. Warning about it would put an alarm
// on every healthy copy, and test/e2e's TestStatusAndMetricsReflectRealWork
// asserts exactly that a healthy run raises none.
//
// The batch is still discoverable, just not here: `cloudfs status` prints it
// as its own line, `cloudfs uploads watch` follows it, #/transfers draws it,
// and the agent turn-start hook says it in a sentence.
func TestABlockedQueueWarnsAndAnActiveBatchDoesNot(t *testing.T) {
	c := progressCollector(upload.Progress{
		Active: true, Seq: 1, FilesTotal: 100, FilesDone: 40,
		BytesTotal: 1 << 30, BytesDone: 1 << 28, Blocked: 3,
	})
	st := c.Collect(context.Background(), i18n.EN)
	joined := strings.Join(st.Warnings, "\n")
	if !strings.Contains(joined, "uploads retry") {
		t.Errorf("a blocked queue does not say what unblocks it: %q", joined)
	}
	if strings.Contains(joined, "uploads watch") {
		t.Errorf("an upload in flight was raised as a warning; it is a state, not a problem: %q", joined)
	}
	// Two copies at once are one batch — the queue has no column saying
	// which row came from where — so nothing may call it "your copy".
	if strings.Contains(strings.ToLower(joined), "your copy") {
		t.Errorf("a warning claims to know whose copy this is: %q", joined)
	}
	// A healthy run, batch and all, raises nothing at all.
	healthy := progressCollector(upload.Progress{
		Active: true, Seq: 1, FilesTotal: 100, FilesDone: 40,
		BytesTotal: 1 << 30, BytesDone: 1 << 28,
	}).Collect(context.Background(), i18n.EN)
	if len(healthy.Warnings) != 0 {
		t.Errorf("a healthy run with an upload in flight produced warnings: %v", healthy.Warnings)
	}
}

// TestBothLanguagesSayTheNewThings: a catalog key copied into the other
// table without translating renders the same sentence twice, which is how a
// Chinese console ends up with an English line in it.
func TestBothLanguagesSayTheNewThings(t *testing.T) {
	for _, key := range []string{"status.upload_blocked"} {
		en, zh := i18n.T(i18n.EN, key, 1, 2, "3 B", "4 B"), i18n.T(i18n.ZH, key, 1, 2, "3 B", "4 B")
		if en == key || zh == key {
			t.Errorf("%s is missing from a catalog: en=%q zh=%q", key, en, zh)
		}
		if en == zh {
			t.Errorf("%s reads identically in both languages", key)
		}
	}
}

// TestCompletionCountersAreFedFromTheLifetimeTotals: a Prometheus counter
// may not decrease. Fed from the batch delta it would fall to zero at every
// batch boundary, and the rate over that minute would be nonsense.
func TestCompletionCountersAreFedFromTheLifetimeTotals(t *testing.T) {
	render := func(p upload.Progress) string {
		var buf bytes.Buffer
		writeMetrics(&buf, progressCollector(p).Collect(context.Background(), i18n.EN))
		return buf.String()
	}
	// End of the first batch: 10 files, 1000 bytes, all of it done.
	first := render(upload.Progress{
		Seq: 1, FilesTotal: 10, FilesDone: 10, BytesTotal: 1000, BytesDone: 1000,
		FilesDoneTotal: 10, BytesDoneTotal: 1000,
	})
	if !strings.Contains(first, "cloudfs_uploads_completed_files_total 10") {
		t.Fatalf("completion counter missing: %s", first)
	}
	// The start of the second batch: the bar is back at zero, the counter
	// must not be.
	second := render(upload.Progress{
		Active: true, Seq: 2, FilesTotal: 4, FilesDone: 0, BytesTotal: 40, BytesDone: 0,
		FilesDoneTotal: 10, BytesDoneTotal: 1000,
	})
	if !strings.Contains(second, "cloudfs_uploads_completed_files_total 10") ||
		!strings.Contains(second, "cloudfs_uploads_completed_bytes_total 1000") {
		t.Fatalf("the completion counters went backwards at the batch boundary: %s", second)
	}
	for _, want := range []string{
		"cloudfs_uploads_blocked",
		"cloudfs_upload_batch_active",
		"cloudfs_upload_batch_files_total",
		"cloudfs_upload_batch_bytes_done",
	} {
		if !strings.Contains(second, want) {
			t.Errorf("%s is not exported: %s", want, second)
		}
	}
}
