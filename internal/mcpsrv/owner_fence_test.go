package mcpsrv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
)

// TestNonOwnerRefusesEveryMutatingToolBeforeTouchingTheFS walks tools/list
// on a NonOwner server, the way cmdMCP builds one when `cloudfs mount`
// already owns the cache. Every mutating tool must come back with the
// owner refusal and leave the provider, meta, the journal, the export
// queue and the index rules exactly as they were: T-43 found that a write
// which reaches commitWrite in a non-owner leaves a node in shared meta
// and a journal row nobody publishes. Read tools keep working. A tool
// added later has to be classified here or the test names it.
func TestNonOwnerRefusesEveryMutatingToolBeforeTouchingTheFS(t *testing.T) {
	root := t.TempDir()
	exports := newFakeExportJobs()
	e, st, x := newIndexAgentEnv(t,
		Options{Export: exports, ExportRoots: []string{root}, NonOwner: true, Workspace: "/work/.agent"},
		agent.Scope{Read: []string{"/"}}, config.Index{Enabled: true})
	e.fake.Seed("work/a.txt", []byte("hello"))
	e.fake.Seed("work/b.txt", []byte("bye"))
	// Warm the tree so the read tools below need no provider round trip
	// and the counters isolate what the mutating tools did.
	e.listDirs(t, "/", "/work")
	ctx := context.Background()
	calls := e.fake.TotalCalls()
	metaBefore, err := e.fs.Meta().Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rulesBefore, err := x.Rules(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Every tool that changes something: files, pins, queued jobs, index
	// rules or sessions. The arguments are valid for an owner, so the only
	// reason to refuse is the fence.
	mutating := map[string]map[string]any{
		"write_file":        {"path": "/work/new.txt", "content": "x"},
		"edit_file":         {"path": "/work/a.txt", "edits": []map[string]any{{"old_text": "hello", "new_text": "bye"}}},
		"create_directory":  {"path": "/work/sub"},
		"copy":              {"from": "/work/a.txt", "to": "/work/c.txt"},
		"move":              {"from": "/work/a.txt", "to": "/work/moved.txt"},
		"delete":            {"path": "/work/a.txt", "confirm": true, "recursive": true},
		"pin":               {"path": "/work/a.txt"},
		"unpin":             {"path": "/work/a.txt"},
		"export":            {"paths": []string{"/work/a.txt"}, "dest": root},
		"cancel_export_job": {"id": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"},
		"retry_upload":      {"id": "x"},
		"cancel_upload":     {"id": "x"},
		"resume_upload":     {"id": "x", "confirm": true},
		"discard_upload":    {"id": "x", "confirm": true},
		"flush_uploads":     {},
		"retry_copy_job":    {"id": "x"},
		"cancel_copy_job":   {"id": "x"},
		"forget_copy_job":   {"id": "x", "confirm": true},
		"index":             {"path": "/work"},
		"unindex":           {"path": "/work"},
		"begin_session":     {},
		"finish_session":    {},
		"list_sessions":     {},
	}
	// Read tools that must succeed outright on the warmed tree.
	readOK := map[string]map[string]any{
		"list_directory":   {"path": "/work"},
		"stat":             {"path": "/work/a.txt"},
		"stat_many":        {"paths": []string{"/work/a.txt", "/work/b.txt"}},
		"read_text":        {"path": "/work/a.txt"},
		"read_range":       {"path": "/work/a.txt", "offset": 0, "length": 2},
		"search":           {"query": "a", "path": "/work"},
		"cache_status":     {"path": "/work/a.txt"},
		"list_roots":       {},
		"get_download_url": {"path": "/work/a.txt"},
		"list_uploads":     {},
		"list_copy_jobs":   {},
		"list_export_jobs": {},
		"semantic_search":  {"query": "hello", "path": "/work"},
		"index_status":     {},
	}
	// Read tools whose arguments name something that does not exist: they
	// may say so, but they must not be fenced.
	readUnfenced := map[string]map[string]any{
		"get_upload":          {"id": "x"},
		"get_copy_job":        {"id": "x"},
		"get_export_job":      {"id": "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"},
		"read_extracted_text": {"path": "/work/a.txt"},
	}

	tools, err := e.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("tools/list returned nothing")
	}
	listed := map[string]bool{}
	for _, tool := range tools.Tools {
		listed[tool.Name] = true
		args, isMutating := mutating[tool.Name]
		if !isMutating {
			if _, ok := readOK[tool.Name]; ok {
				continue
			}
			if _, ok := readUnfenced[tool.Name]; ok {
				continue
			}
			t.Errorf("tool %q is in no table: classify it as mutating or read-only", tool.Name)
			continue
		}
		// list_sessions reads, but session state belongs to the owner's
		// process and the session tools refused it before the fence.
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint && tool.Name != "list_sessions" {
			t.Errorf("%s is fenced as mutating but advertises ReadOnlyHint", tool.Name)
		}
		res := e.call(t, tool.Name, args, nil)
		if !res.IsError {
			t.Errorf("%s ran on a non-owner server: %q", tool.Name, errText(res))
			continue
		}
		body := errText(res)
		if !strings.Contains(body, "requires the storage owner") || !strings.Contains(body, "cloudfs mcp install --transport http") {
			t.Errorf("%s refused for another reason: %q", tool.Name, body)
		}
	}
	for _, table := range []map[string]map[string]any{mutating, readOK, readUnfenced} {
		for name := range table {
			if !listed[name] {
				t.Errorf("table names unknown tool %q", name)
			}
		}
	}

	// Nothing behind the tools moved.
	if n := e.fake.TotalCalls(); n != calls {
		t.Errorf("refused calls reached the provider: %d calls", n-calls)
	}
	rows, err := e.j.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("refused calls left journal rows: %+v", rows)
	}
	metaAfter, err := e.fs.Meta().Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if metaAfter.Nodes != metaBefore.Nodes || metaAfter.Pins != 0 {
		t.Errorf("refused calls changed meta: before %+v after %+v", metaBefore, metaAfter)
	}
	for _, ghost := range []string{"/work/new.txt", "/work/sub", "/work/c.txt", "/work/moved.txt"} {
		if _, err := e.fs.Meta().Resolve(ctx, ghost); !errors.Is(err, meta.ErrNotFound) {
			t.Errorf("%s exists in meta after a refused call: %v", ghost, err)
		}
	}
	if _, err := e.fs.Meta().Resolve(ctx, "/work/a.txt"); err != nil {
		t.Errorf("/work/a.txt is gone after refused move/delete: %v", err)
	}
	if got, _ := e.fake.Content("work/a.txt"); string(got) != "hello" {
		t.Errorf("backend content of work/a.txt = %q", got)
	}
	if len(exports.created) != 0 {
		t.Errorf("a refused export was created: %+v", exports.created)
	}
	rulesAfter, err := x.Rules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rulesAfter) != len(rulesBefore) {
		t.Errorf("index rules changed: before %d after %d", len(rulesBefore), len(rulesAfter))
	}

	// The refusals are audited as denied, not as tool errors.
	denied, _, err := st.Audit(ctx, agent.AuditQuery{Tool: "write_file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 1 || denied[0].Result != "denied" {
		t.Errorf("audit row of the refused write_file: %+v", denied)
	}

	// The read side still works, including a dry-run edit, which changes
	// nothing.
	for name, args := range readOK {
		if res := e.call(t, name, args, nil); res.IsError {
			t.Errorf("%s failed on a non-owner server: %s", name, errText(res))
		}
	}
	for name, args := range readUnfenced {
		if res := e.call(t, name, args, nil); strings.Contains(errText(res), "requires the storage owner") {
			t.Errorf("%s was fenced: %s", name, errText(res))
		}
	}
	res := e.call(t, "edit_file", map[string]any{
		"path": "/work/a.txt", "dry_run": true,
		"edits": []map[string]any{{"old_text": "hello", "new_text": "bye"}},
	}, nil)
	if res.IsError {
		t.Errorf("dry-run edit_file on a non-owner server: %s", errText(res))
	}
	if got, err := e.fs.ReadFileRange(ctx, "/work/a.txt", 0, 0); err != nil || string(got) != "hello" {
		t.Errorf("/work/a.txt after the dry run reads %q, %v", got, err)
	}
}
