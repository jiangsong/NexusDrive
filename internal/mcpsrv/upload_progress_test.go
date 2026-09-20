package mcpsrv

import (
	"context"
	"strings"
	"testing"
)

// TestUploadProgressIsHonestAboutAQueueNotYetSampled: an agent writes a file,
// gets its call back, and asks how the upload is going. The batch is sampled
// once a second by the daemon, so the answer at that instant comes from the
// queue itself — a tool that said "idle" with a row still queued would be
// worse than no tool at all.
func TestUploadProgressIsHonestAboutAQueueNotYetSampled(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()

	var idle uploadProgressOutput
	first := e.call(t, "upload_progress", struct{}{}, &idle)
	if first.IsError {
		t.Fatal(errText(first))
	}
	if idle.Active || idle.Queued != 0 || idle.Batch != 0 {
		t.Fatalf("a daemon that has uploaded nothing reports %+v", idle)
	}
	if !strings.Contains(errText(first), "empty") {
		t.Errorf("an idle queue does not say so in words: %q", errText(first))
	}

	// A write queues a row. Nothing has drained it and no sample has been
	// taken, so the batch is still empty — and the tool must still say that
	// an upload is outstanding.
	if res := e.call(t, "write_file", writeInput{Path: "/pending.txt", Content: "hello"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var busy uploadProgressOutput
	if res := e.call(t, "upload_progress", struct{}{}, &busy); res.IsError {
		t.Fatal(errText(res))
	}
	if busy.Queued+busy.InFlight == 0 {
		t.Fatalf("a queued write is invisible to upload_progress: %+v", busy)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	var drained uploadProgressOutput
	if res := e.call(t, "upload_progress", struct{}{}, &drained); res.IsError {
		t.Fatal(errText(res))
	}
	if drained.Queued != 0 {
		t.Fatalf("the queue drained but the tool still reports %+v", drained)
	}
}

// TestUploadProgressSentenceTellsAnAgentWhatToDo: the structured half is for
// a program, the text half for the model reading it. Each state has to end
// in an action — wait and ask again, or go and look at what failed.
func TestUploadProgressSentenceTellsAnAgentWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  uploadProgressOutput
		want string
	}{
		{"active", uploadProgressOutput{Active: true, Batch: 2, Percent: 40, FilesTotal: 10, FilesDone: 4}, "Call again"},
		{"resumed", uploadProgressOutput{Active: true, Batch: 1, Resumed: true}, "since the daemon restarted"},
		{"blocked", uploadProgressOutput{Blocked: 3}, "waiting on a directory creation"},
		{"queued but unsampled", uploadProgressOutput{Queued: 2}, "Call again in a second"},
		{"failed", uploadProgressOutput{Failed: 1, Batch: 1}, "list_uploads"},
		{"finished", uploadProgressOutput{Batch: 3, FilesDone: 9}, "reached the provider"},
		{"never ran", uploadProgressOutput{}, "nothing has been uploaded"},
	} {
		if got := uploadProgressSentence(tc.out); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not contain %q", tc.name, got, tc.want)
		}
	}
	// Two copies at once are one batch: nothing may claim to know whose.
	active := uploadProgressSentence(uploadProgressOutput{Active: true, Batch: 1})
	if strings.Contains(strings.ToLower(active), "your ") {
		t.Errorf("the sentence claims the batch belongs to the caller: %q", active)
	}
}

// TestCacheStatusCarriesTheQueuesProgress: cache_status is the tool an agent
// already reaches for, and it reported a queued count with no sense of scale.
func TestCacheStatusCarriesTheQueuesProgress(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("a.txt", []byte("hello"))
	var cs cacheStatusOutput
	if res := e.call(t, "cache_status", statInput{Path: "/a.txt"}, &cs); res.IsError {
		t.Fatal(errText(res))
	}
	// An idle daemon has no batch, and the fields are zero rather than absent:
	// an agent reading them must not have to tell "missing" from "none".
	if cs.UploadPercent != 0 || cs.UploadFilesTotal != 0 {
		t.Fatalf("an idle queue reports %+v", cs)
	}
}
