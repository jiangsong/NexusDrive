package control

// writeOps are the provider calls one file's upload is made of. A session
// upload spends begin_upload, one upload_part per part and complete_upload; a
// driver that offers SinglePutMax spends a single put_file instead.
var writeOps = []string{"begin_upload", "upload_part", "complete_upload", "put_file"}

// finishOps are the calls that mean one more file landed. Every upload ends in
// exactly one of them, so counting both never counts a file twice.
var finishOps = []string{"complete_upload", "put_file"}

// writeCostMS reports what one file's write costs on a member, in
// milliseconds, and how many finished files that average rests on.
//
// The inputs are the per-operation counters the provider layer already keeps
// (provider.Stats.Report). Time spent on reads and tree operations is left
// out: those run on the same limiter but are far cheaper per call, and folding
// them in would flatter a slow member — the opposite of what the figure is
// for.
//
// Zero samples means "no answer yet", not "instant": a member with uploads in
// flight and none finished has spent time without producing a file. The caller
// must render that as a dash rather than as a zero, which would rank a member
// that has never finished anything as the fastest in the pool.
func writeCostMS(calls, nanos map[string]int64) (float64, int) {
	var files int64
	for _, op := range finishOps {
		files += calls[op]
	}
	if files <= 0 {
		return 0, 0
	}
	var total int64
	for _, op := range writeOps {
		total += nanos[op]
	}
	return float64(total) / float64(files) / 1e6, int(files)
}
