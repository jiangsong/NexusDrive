package mcpsrv

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

// TestWriteWithoutPreimagesReportsNotRecorded: a server without a
// preimage store (or without sessions) still writes, and every write
// tool says so — reversible false, preimage_reason not_recorded — instead
// of leaving the agent to assume rollback_session has it.
func TestWriteWithoutPreimagesReportsNotRecorded(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/a.txt", []byte("hello"))
	e.fake.Seed("work/b.txt", []byte("bye"))
	var w writeOutput
	if res := e.call(t, "write_file", writeInput{Path: "/work/a.txt", Content: "x"}, &w); res.IsError {
		t.Fatal(errText(res))
	}
	if w.Reversible || w.PreimageReason != preimageNotRecorded {
		t.Fatalf("write_file: %+v", w)
	}
	var ed editOutput
	if res := e.call(t, "edit_file", editInput{Path: "/work/b.txt", Edits: []editSpec{{OldText: "bye", NewText: "ciao"}}}, &ed); res.IsError {
		t.Fatal(errText(res))
	}
	if ed.Reversible || ed.PreimageReason != preimageNotRecorded {
		t.Fatalf("edit_file: %+v", ed)
	}
	var ok okOutput
	for _, c := range []struct {
		tool string
		args any
	}{
		{"create_directory", mkdirInput{Path: "/work/d"}},
		{"move", moveInput{From: "/work/b.txt", To: "/work/d/b.txt"}},
		{"copy", moveInput{From: "/work/a.txt", To: "/work/c.txt"}},
		{"delete", deleteInput{Path: "/work/c.txt", Confirm: true}},
	} {
		ok = okOutput{}
		if res := e.call(t, c.tool, c.args, &ok); res.IsError {
			t.Fatalf("%s: %s", c.tool, errText(res))
		}
		if !ok.OK || ok.Reversible || ok.PreimageReason != preimageNotRecorded {
			t.Fatalf("%s: %+v", c.tool, ok)
		}
	}
	ok = okOutput{}
	if res := e.call(t, "pin", pinInput{Path: "/work/a.txt"}, &ok); res.IsError || ok.PreimageReason != preimageNotApplicable {
		t.Fatalf("pin: %+v %s", ok, errText(res))
	}
	// With a store and a session the same writes are reversible.
	e2, _, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	e2.fake.Seed("work/a.txt", []byte("hello"))
	w = writeOutput{}
	if res := e2.call(t, "write_file", writeInput{Path: "/work/a.txt", Content: "x"}, &w); res.IsError {
		t.Fatal(errText(res))
	}
	if !w.Reversible || w.PreimageReason != preimageOK || !strings.Contains(errText(e2.call(t, "write_file", writeInput{Path: "/work/a.txt", Content: "y"}, nil)), "reversible") {
		t.Fatalf("with preimages: %+v", w)
	}
	ed = editOutput{}
	if res := e2.call(t, "edit_file", editInput{Path: "/work/a.txt", DryRun: true, Edits: []editSpec{{OldText: "y", NewText: "z"}}}, &ed); res.IsError {
		t.Fatal(errText(res))
	}
	if ed.Reversible || ed.PreimageReason != preimageNotRecorded {
		t.Fatalf("dry run must say nothing was recorded: %+v", ed)
	}
}

// TestWriteOverLimitReportsTooLarge: a file past max_preimage_bytes is
// still overwritten, and the response says the old content was not kept.
func TestWriteOverLimitReportsTooLarge(t *testing.T) {
	e, _, _ := newPreimageEnvWithMax(t, Options{}, agent.Scope{}, 16)
	e.fake.Seed("work/big.txt", []byte(strings.Repeat("x", 64)))
	var w writeOutput
	if res := e.call(t, "write_file", writeInput{Path: "/work/big.txt", Content: "small"}, &w); res.IsError {
		t.Fatal(errText(res))
	}
	if w.Reversible || w.PreimageReason != "too_large" || w.Size != 5 {
		t.Fatalf("%+v", w)
	}
}

// TestDeletePlanWithoutConfirmDoesNotDelete: recursive=true with
// confirm=false answers the plan from meta — counts, bytes, a sample,
// which files are cached — and touches neither the tree nor the journal
// nor the provider.
func TestDeletePlanWithoutConfirmDoesNotDelete(t *testing.T) {
	e, _, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/dir/a.txt", []byte("aaaa"))
	e.fake.Seed("work/dir/sub/b.txt", []byte("bb"))
	e.fake.Seed("work/dir/sub/c.txt", []byte("c"))
	e.fake.Seed("work/other/x.txt", []byte("x"))
	e.fake.Seed("work/other/deep/y.txt", []byte("y"))
	e.listDirs(t, "/work", "/work/dir", "/work/dir/sub")
	if res := e.call(t, "read_text", readTextInput{Path: "/work/dir/a.txt"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	before := e.fake.TotalCalls()
	var out okOutput
	res := e.call(t, "delete", deleteInput{Path: "/work/dir", Recursive: true}, &out)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if out.OK || out.Plan == nil {
		t.Fatalf("%+v", out)
	}
	p := out.Plan
	if p.Files != 3 || p.Dirs != 1 || p.Bytes != 7 || p.Cached != 1 || p.Unlisted != 0 || len(p.Sample) != 4 {
		t.Fatalf("plan: %+v", p)
	}
	if !strings.Contains(errText(res), "plan only") || !strings.Contains(errText(res), "confirm=true") {
		t.Fatalf("summary: %s", errText(res))
	}
	if e.fake.TotalCalls() != before {
		t.Fatalf("the plan reached the provider: %d→%d", before, e.fake.TotalCalls())
	}
	if _, err := e.fs.StatPath(context.Background(), "/work/dir/sub/b.txt"); err != nil {
		t.Fatalf("the plan deleted something: %v", err)
	}
	if st, err := e.j.Stats(context.Background()); err != nil || st.Pending+st.Uploading != 0 {
		t.Fatalf("the plan queued journal work: %+v %v", st, err)
	}
	// A directory meta never listed makes the plan say it is incomplete.
	e.listDirs(t, "/work/other")
	out = okOutput{}
	if res := e.call(t, "delete", deleteInput{Path: "/work/other", Recursive: true}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Plan.Unlisted != 1 || out.Plan.Files != 1 {
		t.Fatalf("unlisted: %+v", out.Plan)
	}
}

// TestRecursiveDeleteCapturesCachedFilesOnly: deleting a directory with
// three cached and two uncached files keeps the three (no download for
// the two), and a rollback brings back the directory, its subdirectory
// and the three files, skipping the two as not_cached.
func TestRecursiveDeleteCapturesCachedFilesOnly(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	for _, f := range []string{"a", "b", "sub/c"} {
		e.fake.Seed("work/dir/"+f+".txt", []byte("cached "+f))
	}
	for _, f := range []string{"u1", "sub/u2"} {
		e.fake.Seed("work/dir/"+f+".txt", []byte("never read "+f))
	}
	e.listDirs(t, "/work", "/work/dir", "/work/dir/sub")
	for _, f := range []string{"a", "b", "sub/c"} {
		if res := e.call(t, "read_text", readTextInput{Path: "/work/dir/" + f + ".txt"}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	gets := e.fake.Calls("ReadRange")
	var out okOutput
	if res := e.call(t, "delete", deleteInput{Path: "/work/dir", Recursive: true, Confirm: true}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if e.fake.Calls("ReadRange") != gets {
		t.Fatalf("the delete downloaded uncached files: ReadRange %d→%d", gets, e.fake.Calls("ReadRange"))
	}
	if !out.OK || out.Reversible || out.PreimageReason != preimagePartial || out.Plan == nil {
		t.Fatalf("%+v", out)
	}
	if out.Plan.Kept != 3 || out.Plan.Unkept != 2 || out.Plan.Files != 5 || out.Plan.Dirs != 1 {
		t.Fatalf("plan: %+v", out.Plan)
	}
	if _, err := e.fs.StatPath(context.Background(), "/work/dir"); err == nil {
		t.Fatal("the directory is still there")
	}
	sess := currentSession(t, st)
	ops, err := st.OpsOf(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// files, then the subdirectory, then the directory itself.
	if n := len(ops); n != 7 || ops[6].Path != "/work/dir" || ops[6].PreState != "dir" || ops[6].PreReason != "" || ops[5].Path != "/work/dir/sub" || ops[5].PreState != "dir" {
		t.Fatalf("rows: %+v", ops)
	}
	var plan agent.Plan
	if res := e.call(t, "rollback_session", rollbackSessionInput{SessionID: sess.ID, Confirm: true}, &plan); res.IsError {
		t.Fatal(errText(res))
	}
	if len(plan.Restored) != 5 || len(plan.Skipped) != 2 || len(plan.Conflict) != 0 {
		t.Fatalf("rollback: restored %d skipped %d conflict %d: %+v", len(plan.Restored), len(plan.Skipped), len(plan.Conflict), plan)
	}
	for _, sk := range plan.Skipped {
		if sk.Reason != "not_cached" {
			t.Fatalf("skipped %+v", sk)
		}
	}
	for _, f := range []string{"a", "b", "sub/c"} {
		data, err := e.fs.ReadFileRange(context.Background(), "/work/dir/"+f+".txt", 0, 0)
		if err != nil || string(data) != "cached "+f {
			t.Fatalf("%s after rollback: %q %v", f, data, err)
		}
	}
	if _, err := e.fs.StatPath(context.Background(), "/work/dir/u1.txt"); err == nil {
		t.Fatal("an uncached file came back from nowhere")
	}
}

// TestRecursiveDeleteHonoursPreimageFileBudget: past preimage_files the
// remaining cached files are recorded as too_many, not captured.
func TestRecursiveDeleteHonoursPreimageFileBudget(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{PreimageFiles: 2}, agent.Scope{})
	for _, f := range []string{"a", "b", "c", "d"} {
		e.fake.Seed("work/dir/"+f+".txt", []byte("cached "+f))
	}
	e.listDirs(t, "/work", "/work/dir")
	for _, f := range []string{"a", "b", "c", "d"} {
		if res := e.call(t, "read_text", readTextInput{Path: "/work/dir/" + f + ".txt"}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	var out okOutput
	if res := e.call(t, "delete", deleteInput{Path: "/work/dir", Recursive: true, Confirm: true}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Plan.Kept != 2 || out.Plan.Unkept != 2 || out.PreimageReason != preimagePartial {
		t.Fatalf("%+v", out.Plan)
	}
	ops, err := st.OpsOf(context.Background(), currentSession(t, st).ID)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := 0
	for _, o := range ops {
		if o.PreReason == preimageTooMany {
			tooMany++
		}
	}
	if tooMany != 2 {
		t.Fatalf("too_many rows = %d: %+v", tooMany, ops)
	}
	var plan agent.Plan
	if res := e.call(t, "rollback_session", rollbackSessionInput{SessionID: currentSession(t, st).ID, DryRun: true}, &plan); res.IsError {
		t.Fatal(errText(res))
	}
	if len(plan.Restored) != 3 || len(plan.Skipped) != 2 || plan.Skipped[0].Reason != preimageTooMany {
		t.Fatalf("%+v", plan)
	}
}
