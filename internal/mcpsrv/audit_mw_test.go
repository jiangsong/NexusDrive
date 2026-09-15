package mcpsrv

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func auditRows(t *testing.T, st *agent.Store) []agent.AuditRow {
	t.Helper()
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// waitAuditRow polls until a row for tool appears, because handshake and
// subscription rows are written by the server side of the transport after the
// client has already seen its reply.
func waitAuditRow(t *testing.T, st *agent.Store, tool string) agent.AuditRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, r := range auditRows(t, st) {
			if r.Tool == tool {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no audit row for %q: %+v", tool, auditRows(t, st))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeniedDeleteIsAuditedWithoutContent(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("gd/x.txt", []byte("secret"))
	e.call(t, "delete", map[string]any{"path": "/gd/x.txt", "confirm": true}, nil)
	e.call(t, "write_file", map[string]any{"path": "/gd/y.txt", "content": "see https://signed.example/a?sig=1"}, nil)
	rows := auditRows(t, st)
	var del, wr *agent.AuditRow
	for i := range rows {
		switch rows[i].Tool {
		case "delete":
			del = &rows[i]
		case "write_file":
			wr = &rows[i]
		}
	}
	if del == nil || del.Result != "denied" || len(del.Paths) != 1 || del.Paths[0] != "/gd/x.txt" {
		t.Fatalf("delete row %+v", del)
	}
	if wr == nil || wr.Result != "denied" || bytes.Contains(wr.Args, []byte("signed.example")) || bytes.Contains(wr.Args, []byte("http")) {
		t.Fatalf("write row %+v", wr)
	}
	if !bytes.Contains(wr.Args, []byte(`"content":{"bytes":34}`)) || !bytes.Contains(wr.Args, []byte(`"/gd/y.txt"`)) {
		t.Fatalf("write args must keep the path and the content size: %s", wr.Args)
	}
	if del.SessionID == "" {
		t.Fatal("the audit row has no session: middleware order is wrong")
	}
	if del.PrincipalID == "" || del.Transport != "stdio" || del.Error == "" || del.TS.IsZero() {
		t.Fatalf("row is missing who/where/why: %+v", del)
	}
}

func TestLargeWriteAuditArgsStayBounded(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	if res := e.call(t, "write_file", map[string]any{"path": "/a.txt", "content": strings.Repeat("z", 204800)}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	for _, r := range auditRows(t, st) {
		if r.Tool == "write_file" {
			if len(r.Args) > agent.MaxAuditArgs || r.BytesIn != 204800 || r.Result != "ok" {
				t.Fatalf("%d %d %s", len(r.Args), r.BytesIn, r.Result)
			}
			if len(r.Paths) != 1 || r.Paths[0] != "/a.txt" || r.BytesOut == 0 || r.Error != "" {
				t.Fatalf("row %+v", r)
			}
			return
		}
	}
	t.Fatal("no write_file audit row")
}

func TestMoveAuditsBothEndsAndAnErrorIsRecorded(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.call(t, "move", map[string]any{"from": "/missing.txt", "to": "/b.txt"}, nil)
	row := waitAuditRow(t, st, "move")
	if row.Result != "error" || row.Error == "" || len(row.Paths) != 2 || row.Paths[0] != "/missing.txt" || row.Paths[1] != "/b.txt" {
		t.Fatalf("row %+v", row)
	}
}

func TestSearchFilteringLeavesNoDenialInTheAudit(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/report.md", []byte("in scope"))
	e.fake.Seed("gd/report.md", []byte("hidden"))
	for _, p := range []string{"/work", "/gd"} {
		if _, err := e.fs.ReadDirPath(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if res := e.call(t, "search", map[string]any{"query": "report"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	row := waitAuditRow(t, st, "search")
	if row.Result != "ok" || len(row.Paths) != 0 {
		t.Fatalf("hiding a result outside the scope is not a denial: %+v", row)
	}
}

type failingAudit struct{ n atomic.Int64 }

func (f *failingAudit) AppendAudit(context.Context, agent.AuditRow) (int64, error) {
	f.n.Add(1)
	return 0, errors.New("agent.db is read-only")
}

func TestAuditFailureDoesNotFailTheTool(t *testing.T) {
	fa := &failingAudit{}
	e, _ := newAgentEnv(t, Options{Audit: fa}, agent.Scope{})
	if res := e.call(t, "write_file", map[string]any{"path": "/a.txt", "content": "x"}, nil); res.IsError {
		t.Fatalf("tool failed because audit failed: %s", errText(res))
	}
	if fa.n.Load() == 0 {
		t.Fatal("audit was never attempted")
	}
}

func TestStoreAuditFailureIsCountedAndTheToolStillSucceeds(t *testing.T) {
	// A real store whose database has been closed fails every append the way
	// a full or read-only disk would, while the session store stays healthy.
	broken, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	e, st := newAgentEnv(t, Options{Audit: broken}, agent.Scope{})
	e.fake.Seed("a.txt", []byte("hello"))
	if res := e.call(t, "stat", map[string]any{"path": "/a.txt"}, nil); res.IsError {
		t.Fatalf("tool failed because the audit trail is broken: %s", errText(res))
	}
	if broken.AuditWriteFailures() == 0 {
		t.Fatal("the failed append was not counted")
	}
	if rows := auditRows(t, st); len(rows) != 0 {
		t.Fatalf("Options.Audit must take precedence over the session store: %+v", rows)
	}
}

func TestNoAuditWithoutASessionStore(t *testing.T) {
	fa := &failingAudit{}
	e := newEnv(t, Options{Allow: []string{"/work"}, Audit: fa})
	e.fake.Seed("work/a.txt", []byte("hello"))
	if res := e.call(t, "stat", map[string]any{"path": "/work/a.txt"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	// The handshake and the tool call: an explicit writer works without a
	// session store, the rows then carrying no session.
	if fa.n.Load() != 2 {
		t.Fatalf("an explicit audit writer works without sessions: %d attempts", fa.n.Load())
	}
	plain := newEnv(t, Options{Allow: []string{"/work"}})
	if plain.server.audit != nil {
		t.Fatal("with neither Sessions nor Audit there must be no audit writer")
	}
}

// TestInitializeAndSubscribeAreAudited covers both protocol generations: a
// legacy client's initialize and resources/subscribe over stateful HTTP (the
// request shape of TestResourceSubscriptionsLegacyHTTP), and a modern client's
// server/discover and subscriptions/listen over the in-memory transport.
func TestInitializeAndSubscribeAreAudited(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/file", []byte("old"))
	e.fake.Seed("gd/file", []byte("hidden"))
	for _, p := range []string{"/work/file", "/gd/file"} {
		if _, err := e.fs.StatPath(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}

	// The in-memory client of newAgentEnv has already discovered the server.
	disc := waitAuditRow(t, st, "server/discover")
	if disc.Result != "ok" || disc.SessionID == "" || disc.Transport != "stdio" || !bytes.Contains(disc.Args, []byte(`"client":"test"`)) {
		t.Fatalf("discover row %+v", disc)
	}

	h := subscriptionHTTPServer(t, e.server)
	resp, data := subscriptionRPC(t, h.URL, "2025-11-25", "", "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "legacy-audit-test", "version": "1"},
	})
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sessionID == "" {
		t.Fatalf("legacy initialize: status=%d body=%s", resp.StatusCode, data)
	}
	init := waitAuditRow(t, st, "initialize")
	if init.Result != "ok" || init.SessionID == "" || init.Transport != "http-legacy" || !bytes.Contains(init.Args, []byte(`"client":"legacy-audit-test"`)) {
		t.Fatalf("initialize row %+v", init)
	}
	if bytes.Contains(init.Args, []byte("capabilities")) {
		t.Fatalf("initialize args must be the client identity, not the whole handshake: %s", init.Args)
	}

	resp, data = subscriptionRPC(t, h.URL, "2025-11-25", sessionID, "resources/subscribe", map[string]string{"uri": "cloudfs://ali/gd/file"})
	if resp.StatusCode != 200 || !bytes.Contains(data, []byte(`"error"`)) {
		t.Fatalf("subscribe outside the scope must fail: %d %s", resp.StatusCode, data)
	}
	denied := waitAuditRow(t, st, "resources/subscribe")
	if denied.Result != "denied" || len(denied.Paths) != 1 || denied.Paths[0] != "/gd/file" || denied.SessionID != init.SessionID {
		t.Fatalf("denied subscribe row %+v", denied)
	}
	resp, data = subscriptionRPC(t, h.URL, "2025-11-25", sessionID, "resources/subscribe", map[string]string{"uri": "cloudfs://ali/work/file"})
	if resp.StatusCode != 200 || bytes.Contains(data, []byte(`"error"`)) {
		t.Fatalf("legacy subscribe: %d %s", resp.StatusCode, data)
	}
	waitSubscriptionCount(t, e.server, 1)
	var ok *agent.AuditRow
	for _, r := range auditRows(t, st) {
		if r.Tool == "resources/subscribe" && r.Result == "ok" {
			ok = &r
			break
		}
	}
	if ok == nil || len(ok.Paths) != 1 || ok.Paths[0] != "/work/file" || !bytes.Contains(ok.Args, []byte(`"cloudfs://ali/work/file"`)) {
		t.Fatalf("accepted subscribe row %+v", ok)
	}

	// A modern client's listen stream is one long call: its row lands when
	// the stream ends, a refused one at once. The SDK client sends the listen
	// without waiting for an answer, so the refusal is only visible here.
	client := newSubscriptionClient(t, e.server)
	_ = client.session.Subscribe(context.Background(), &mcp.SubscribeParams{URI: "cloudfs://ali/gd/file"})
	listenDenied := waitAuditRow(t, st, "subscriptions/listen")
	if listenDenied.Result != "denied" || len(listenDenied.Paths) != 1 || listenDenied.Paths[0] != "/gd/file" {
		t.Fatalf("denied listen row %+v", listenDenied)
	}
	client.subscribe(t, "cloudfs://ali/work/file")
	waitSubscriptionCount(t, e.server, 2)
	client.session.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var found *agent.AuditRow
		for _, r := range auditRows(t, st) {
			if r.Tool == "subscriptions/listen" && r.Result == "ok" {
				found = &r
				break
			}
		}
		if found != nil {
			if len(found.Paths) != 1 || found.Paths[0] != "/work/file" || found.SessionID == "" {
				t.Fatalf("listen row %+v", found)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ok listen row after the stream ended: %+v", auditRows(t, st))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, r := range auditRows(t, st) {
		if r.Tool == "subscriptions/listen" && len(r.Paths) == 0 {
			t.Fatalf("a catalog-only listen must not be audited: %+v", r)
		}
	}
}
