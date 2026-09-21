package control

import "testing"

const ms = int64(1e6)

// The pool page ranks members by free space, so the member with the emptiest
// drive becomes every file's primary even when it is an order of magnitude
// slower than its sibling. A person cannot see that from the page: the only
// evidence lives in /metrics, as two counters that have to be divided. This
// turns those counters into the number the decision actually needs — what one
// file costs on this member.
func TestWriteCostAveragesTheWholeWritePathOverTheFilesThatFinished(t *testing.T) {
	calls := map[string]int64{"begin_upload": 2, "upload_part": 2, "complete_upload": 2}
	nanos := map[string]int64{"begin_upload": 4000 * ms, "upload_part": 6000 * ms, "complete_upload": 10000 * ms}

	cost, samples := writeCostMS(calls, nanos)

	if samples != 2 {
		t.Fatalf("samples = %d, want the 2 files that reached complete_upload", samples)
	}
	if cost != 10000 {
		t.Fatalf("cost = %g ms, want 20s of write path split over 2 files", cost)
	}
}

// A member whose driver offers SinglePutMax never opens a session: one
// put_file is the whole upload. Counting only the session ops would report no
// samples for the fastest member in the pool, which is the one case the page
// exists to show.
func TestWriteCostCountsAOneRequestPutAsAFinishedFile(t *testing.T) {
	calls := map[string]int64{"put_file": 4}
	nanos := map[string]int64{"put_file": 8000 * ms}

	cost, samples := writeCostMS(calls, nanos)

	if samples != 4 {
		t.Fatalf("samples = %d, want one sample per put_file", samples)
	}
	if cost != 2000 {
		t.Fatalf("cost = %g ms, want 8s split over 4 files", cost)
	}
}

// Reads and tree operations run on the same limiter as the write path and are
// far cheaper per call. Folding them in would flatter a slow member, which is
// the opposite of what the column is for.
func TestWriteCostIgnoresReadsAndTreeOperations(t *testing.T) {
	calls := map[string]int64{"put_file": 1, "read_range": 50, "list": 20, "mkdir": 20, "stat": 20}
	nanos := map[string]int64{"put_file": 1000 * ms, "read_range": 90000 * ms, "list": 90000 * ms, "mkdir": 90000 * ms, "stat": 90000 * ms}

	cost, _ := writeCostMS(calls, nanos)

	if cost != 1000 {
		t.Fatalf("cost = %g ms, want only the put_file time", cost)
	}
}

// An upload in flight has spent time without producing a file yet. Dividing by
// zero there would report +Inf, and reporting 0 would rank a member that has
// never finished anything as the fastest in the pool. Neither is an answer, so
// the page must be told there is no answer and print a dash.
func TestWriteCostReportsNoSamplesUntilAFileFinishes(t *testing.T) {
	calls := map[string]int64{"begin_upload": 3, "upload_part": 3}
	nanos := map[string]int64{"begin_upload": 9000 * ms, "upload_part": 9000 * ms}

	cost, samples := writeCostMS(calls, nanos)

	if samples != 0 {
		t.Fatalf("samples = %d, want 0 while no upload has completed", samples)
	}
	if cost != 0 {
		t.Fatalf("cost = %g, want 0 alongside 0 samples", cost)
	}
}

// A member the daemon has not touched this run has no counters at all. The
// nil map is the normal state right after a restart, not an error.
func TestWriteCostOfAMemberWithNoCountersIsNoSamples(t *testing.T) {
	cost, samples := writeCostMS(nil, nil)

	if samples != 0 || cost != 0 {
		t.Fatalf("cost, samples = %g, %d; want a member with no history to report nothing", cost, samples)
	}
}
