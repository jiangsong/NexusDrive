package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/memory"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioInitializeRecordsOneSession(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	_ = e
	m := agent.NewSessions(st, agent.SessionOptions{})
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, _, err := m.List(context.Background(), agent.ListQuery{State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].ClientName == "test" && list[0].Transport == "stdio" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions after initialize: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestToolCallsReuseTheSessionTheirConnectionOpened(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("a.txt", []byte("hello"))
	for i := 0; i < 3; i++ {
		if res := e.call(t, "stat", map[string]any{"path": "/a.txt"}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	m := agent.NewSessions(st, agent.SessionOptions{})
	all, _, err := m.List(context.Background(), agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].State != "active" {
		t.Fatalf("one connection must map to one session: %+v", all)
	}
}

func TestWriteScopeIsSeparateFromReadScope(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/"}, Write: []string{"/work"}})
	e.fake.Seed("other/a.txt", []byte("hello"))
	e.fake.Seed("work/.keep", []byte(""))
	if res := e.call(t, "read_text", map[string]any{"path": "/other/a.txt"}, nil); res.IsError {
		t.Fatalf("read outside write scope refused: %s", errText(res))
	}
	res := e.call(t, "write_file", map[string]any{"path": "/other/b.txt", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("write outside write scope: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "write_file", map[string]any{"path": "/work/b.txt", "content": "x"}, nil); res.IsError {
		t.Fatalf("write inside write scope refused: %s", errText(res))
	}
	// A move needs write on both ends: the source is deleted, the target is
	// created.
	res = e.call(t, "move", map[string]any{"from": "/work/b.txt", "to": "/other/b.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("move out of the write scope: %v %s", res.IsError, errText(res))
	}
	res = e.call(t, "copy", map[string]any{"from": "/other/a.txt", "to": "/work/c.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("copy from outside the write scope: %v %s", res.IsError, errText(res))
	}
}

func TestReadOnlyScopeKeepsTheOldMessage(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{ReadOnly: true})
	res := e.call(t, "write_file", map[string]any{"path": "/a.txt", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("read-only scope: %v %s", res.IsError, errText(res))
	}
	res = e.call(t, "retry_upload", map[string]any{"id": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("read-only scope on an id-only write tool: %v %s", res.IsError, errText(res))
	}
}

func TestExpiredScopeRefusesEverything(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{ExpiresAt: time.Unix(1, 0)})
	e.fake.Seed("a.txt", []byte("hello"))
	res := e.call(t, "read_text", map[string]any{"path": "/a.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "expired") {
		t.Fatalf("expired scope read: %v %s", res.IsError, errText(res))
	}
}

func TestOptionsWithoutSessionsKeepTheProcessWideScope(t *testing.T) {
	// Allow and ReadOnly still derive the one scope every call shares when
	// no session store is configured.
	e := newEnv(t, Options{Allow: []string{"/work"}, ReadOnly: true})
	e.fake.Seed("work/a.txt", []byte("hello"))
	if res := e.call(t, "read_text", map[string]any{"path": "/work/a.txt"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	res := e.call(t, "read_text", map[string]any{"path": "/gd/a.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("allowlist: %v %s", res.IsError, errText(res))
	}
	res = e.call(t, "write_file", map[string]any{"path": "/work/b.txt", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("read-only: %v %s", res.IsError, errText(res))
	}
	if !errors.Is(ErrDenied, agent.ErrDenied) {
		t.Fatal("mcpsrv.ErrDenied must be the agent scope error")
	}
}

// TestEveryToolChecksItsPaths walks tools/list so that a tool added later has
// to be classified here: either it takes a path and that path is checked, or it
// is listed as path-less with a reason.
func TestEveryToolChecksItsPaths(t *testing.T) {
	root := t.TempDir()
	// The env carries an index so that the index tools are listed too, and
	// a memory store rooted outside the scope so that the memory tools,
	// whose paths come from the configuration, are refused like the rest.
	fsShell := &lateFS{}
	mem := memory.New(memory.Options{FS: fsShell, Config: memoryConfig("/gd/.agent")})
	e, _, _ := newIndexAgentEnv(t, Options{Export: newFakeExportJobs(), ExportRoots: []string{root}, Memory: mem}, agent.Scope{Read: []string{"/work"}}, config.Index{Enabled: true})
	fsShell.fs = e.fs
	e.fake.Seed("gd/x.txt", []byte("secret"))
	e.fake.Seed("work/nope.txt", []byte("here"))
	outside := map[string]map[string]any{
		"list_directory":   {"path": "/gd"},
		"stat":             {"path": "/gd/x.txt"},
		"stat_many":        {"paths": []string{"/gd/x.txt"}},
		"read_text":        {"path": "/gd/x.txt"},
		"read_range":       {"path": "/gd/x.txt", "offset": 0, "length": 4},
		"search":           {"query": "x", "path": "/gd"},
		"cache_status":     {"path": "/gd/x.txt"},
		"get_download_url": {"path": "/gd/x.txt"},
		"write_file":       {"path": "/gd/y.txt", "content": "x"},
		"edit_file":        {"path": "/gd/x.txt", "edits": []map[string]any{{"old_text": "secret", "new_text": "s"}}},
		"create_directory": {"path": "/gd/new"},
		"copy":             {"from": "/gd/x.txt", "to": "/work/x.txt"},
		"move":             {"from": "/work/nope.txt", "to": "/gd/y.txt"},
		"delete":           {"path": "/gd/x.txt", "confirm": true},
		"pin":              {"path": "/gd/x.txt"},
		"unpin":            {"path": "/gd/x.txt"},
		"export":           {"paths": []string{"/gd/x.txt"}, "dest": root},
		// The index tools: a rule or a search below /gd, or the extracted
		// text of a file there.
		"semantic_search":     {"query": "x", "path": "/gd"},
		"index":               {"path": "/gd"},
		"unindex":             {"path": "/gd"},
		"read_extracted_text": {"path": "/gd/x.txt"},
		// The memory tools: every path is under memory.root, here
		// /gd/.agent, which the scope does not contain.
		"memory_list":   {},
		"memory_get":    {"name": "x"},
		"memory_put":    {"name": "x", "content": "x"},
		"memory_delete": {"name": "x", "confirm": true},
		"memory_search": {"query": "x"},
	}
	// Path-less tools: they take ids or nothing, and filter their results by
	// scope internally (list_roots, upload/copy/export job listings).
	pathless := map[string]string{
		"list_roots": "filters mounts by scope", "flush_uploads": "unrestricted scope only",
		"list_uploads": "filters by scope", "get_upload": "filters by scope", "retry_upload": "write gate only",
		"cancel_upload": "write gate only", "resume_upload": "write gate only", "discard_upload": "write gate only",
		"list_copy_jobs": "filters by scope", "get_copy_job": "filters by scope", "retry_copy_job": "write gate only",
		"cancel_copy_job": "write gate only", "forget_copy_job": "write gate only",
		"list_export_jobs": "job ids only", "get_export_job": "job ids only", "cancel_export_job": "write gate only",
		// The session tools derive their paths from the configuration and
		// check them internally; list_sessions filters by principal.
		"begin_session": "workspace checked internally", "finish_session": "session ids only", "list_sessions": "filters by principal",
		// rollback_session replays the paths its rows recorded, which the
		// session's own scope checked when they were written.
		"rollback_session": "session ids only; paths come from the session's own rows",
		// index_status checks the path it is given (TestIndexStatusChecksItsOptionalPath)
		// and filters the failure list by scope when given none.
		"index_status": "optional path checked; failures filtered by scope",
	}
	// perEntry tools report a denied path inside their structured result
	// instead of failing the whole call.
	perEntry := map[string]bool{"stat_many": true}
	tools, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("tools/list returned nothing")
	}
	for _, tool := range tools.Tools {
		args, ok := outside[tool.Name]
		if !ok {
			if _, exempt := pathless[tool.Name]; !exempt {
				t.Errorf("tool %q is in neither table: classify it", tool.Name)
			}
			continue
		}
		res := e.call(t, tool.Name, args, nil)
		body := errText(res)
		if perEntry[tool.Name] {
			raw, _ := json.Marshal(res.StructuredContent)
			body = string(raw)
		} else if !res.IsError {
			t.Errorf("%s reached outside /work: %q", tool.Name, body)
			continue
		}
		if !strings.Contains(body, "outside the allowed") {
			t.Errorf("%s reached outside /work: IsError=%v %q", tool.Name, res.IsError, body)
		}
	}
	// Both tables must name real tools, or a rename would silently drop a
	// check.
	listed := map[string]bool{}
	for _, tool := range tools.Tools {
		listed[tool.Name] = true
	}
	for name := range outside {
		if !listed[name] {
			t.Errorf("outside table names unknown tool %q", name)
		}
	}
	for name := range pathless {
		if !listed[name] {
			t.Errorf("pathless table names unknown tool %q", name)
		}
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatalf("a denied call reached the provider: %d calls", e.fake.TotalCalls())
	}
}

// TestSubscribeOutsideScopeIsDenied mirrors the subscription set-up of
// subscriptions_test.go with a session scope in front. The reservation is
// driven the way TestResourceSubscriptionsRejectInvalidBatchesAtomically
// drives it, with the session in the context: a URI outside the scope is
// refused, one inside it reaches the SDK. Then a real client subscribes
// inside the scope, which pins the middleware order: with a session store the
// reservation refuses every call that did not pass session resolution first.
func TestSubscribeOutsideScopeIsDenied(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/a.txt", []byte("inside"))
	e.fake.Seed("gd/x.txt", []byte("secret"))
	_, transport := mcp.NewInMemoryTransports()
	session, err := e.server.MCP().Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx := agent.WithSession(context.Background(), agent.Session{ID: "s", Scope: agent.Scope{Read: []string{"/work"}}})
	for _, tc := range []struct {
		uri    string
		denied bool
	}{
		{"cloudfs://ali/gd/x.txt", true},
		{"cloudfs://ali/work/../gd/x.txt", true},
		{"cloudfs://ali/work/a.txt", false},
	} {
		called := false
		handler := e.server.subscriptions.receive(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			called = true
			return nil, nil
		})
		_, err := handler(ctx, "subscriptions/listen", &mcp.SubscriptionsListenRequest{
			Session: session,
			Params:  &mcp.SubscriptionsListenParams{Notifications: &mcp.NotificationSubscriptions{ResourceSubscriptions: []string{tc.uri}}},
		})
		var rpcErr *jsonrpc.Error
		if tc.denied && (!errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams || called) {
			t.Fatalf("%s: outside the scope reached SDK or wrong error: called=%v err=%v", tc.uri, called, err)
		}
		if !tc.denied && (err != nil || !called) {
			t.Fatalf("%s: inside the scope was refused: called=%v err=%v", tc.uri, called, err)
		}
		waitSubscriptionCount(t, e.server, 0)
	}
	client := newSubscriptionClient(t, e.server)
	client.subscribe(t, "cloudfs://ali/work/a.txt")
	waitSubscriptionCount(t, e.server, 1)
	if e.fake.TotalCalls() != 0 {
		t.Fatal("subscribing touched the provider")
	}
	m := agent.NewSessions(st, agent.SessionOptions{})
	active, _, err := m.List(context.Background(), agent.ListQuery{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("the tool client and the subscription client are two connections: %d sessions", len(active))
	}
}
