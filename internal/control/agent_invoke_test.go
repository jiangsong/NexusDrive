package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/trigger"
)

// /agent/invoke hands a file and a prompt to a locally configured agent. The
// engine runs the command; these tests are about what the route lets
// through to it: nothing at all when no agent is configured, nothing
// without the typed confirmation, paths only as separate argv elements, and
// an audit row for every attempt that names the agent but never the prompt.

// fakeTrigger is a TriggerControl that records what Invoke was asked and
// runs nothing. A process that started here would be a test failure that
// no assertion could see, so the route is first proven against this.
type fakeTrigger struct {
	agents  []config.Agent
	invoked []fakeInvoke
	err     error
}

type fakeInvoke struct {
	agent  string
	paths  []string
	prompt string
}

func (f *fakeTrigger) Rules() []config.Trigger  { return nil }
func (f *fakeTrigger) Agents() []config.Agent   { return f.agents }
func (f *fakeTrigger) Deliveries(context.Context, agent.DeliveryQuery) ([]agent.Delivery, string, error) {
	return nil, "", nil
}
func (f *fakeTrigger) Delivery(context.Context, int64) (agent.Delivery, error) {
	return agent.Delivery{}, agent.ErrDeliveryNotFound
}
func (f *fakeTrigger) Test(context.Context, string, string) (int64, error) { return 0, nil }
func (f *fakeTrigger) Retry(context.Context, int64) error                  { return nil }
func (f *fakeTrigger) Counts(context.Context) (int, int, error)            { return 0, 0, nil }
func (f *fakeTrigger) Invoke(_ context.Context, name string, paths []string, prompt string) (int64, error) {
	f.invoked = append(f.invoked, fakeInvoke{agent: name, paths: append([]string(nil), paths...), prompt: prompt})
	if f.err != nil {
		return 0, f.err
	}
	return int64(len(f.invoked)), nil
}

// invokeFixture is a filesystem with /docs/b and /docs/sub/c, an agent
// store the audit lands in, and a real engine over the given agents. The
// engine is only started when run is set: a stopped engine keeps every
// invocation pending, which is what the merge and audit tests need.
func invokeFixture(t *testing.T, agents []config.Agent, run bool) (*fixture, *agent.Store, *trigger.Engine) {
	t.Helper()
	f, _ := fsControl(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.coll.Agent = NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "", RollbackDeps{})
	eng := trigger.New(trigger.Options{Store: st, Agents: agents, Tick: 20 * time.Millisecond})
	t.Cleanup(eng.Close)
	if run {
		if err := eng.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	f.coll.Trigger = eng
	return f, st, eng
}

func invokeBody(agentName string, paths []string, prompt string, confirm bool) string {
	b, _ := json.Marshal(AgentInvokeRequest{Agent: agentName, Paths: paths, Prompt: prompt, Confirm: confirm})
	return string(b)
}

// TestInvokeWithoutAgentsIs404AndSpawnsNothing: no engine, an engine with
// no agents, and an engine that has other agents all answer 404 and the
// engine's Invoke is never reached, so no process can have started.
func TestInvokeWithoutAgentsIs404AndSpawnsNothing(t *testing.T) {
	f, _ := fsControl(t)
	body := invokeBody("claude", []string{"/docs/b"}, "summarize", true)

	// No engine at all: the daemon is not the owner or has no agents[].
	s := NewServer(f.coll)
	if w := call(t, s, "POST", "/agent/invoke", body); w.Code != 404 {
		t.Fatalf("no engine: %d %s", w.Code, w.Body.String())
	} else if strings.Contains(w.Body.String(), f.dir) {
		t.Fatalf("404 body leaks the cache directory: %s", w.Body.String())
	}
	if w := call(t, s, "GET", "/agent/endpoints", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"agents":[]}` {
		t.Fatalf("endpoints without an engine: %d %s", w.Code, w.Body.String())
	}

	for _, agents := range [][]config.Agent{nil, {{Name: "other", Exec: config.ExecAction{Command: []string{"/bin/true"}}}}} {
		ft := &fakeTrigger{agents: agents}
		f.coll.Trigger = ft
		s := NewServer(f.coll)
		w := call(t, s, "POST", "/agent/invoke", body)
		if w.Code != 404 {
			t.Fatalf("agents %v: %d %s", agents, w.Code, w.Body.String())
		}
		if len(ft.invoked) != 0 {
			t.Fatalf("agents %v: the engine was asked to run %+v", agents, ft.invoked)
		}
	}
	// The name is checked before the confirmation: a typo is a 404, not a
	// prompt to confirm something that does not exist.
	ft := &fakeTrigger{}
	f.coll.Trigger = ft
	if w := call(t, NewServer(f.coll), "POST", "/agent/invoke", invokeBody("claude", []string{"/docs/b"}, "x", false)); w.Code != 404 {
		t.Fatalf("unconfirmed and unconfigured: %d", w.Code)
	}
}

// TestInvokeNeedsConfirm: the run executes a local command, so it is
// confirmed like a deletion; the 400 names the agent so the person knows
// what they are about to run. Bad paths are refused before the engine
// sees anything.
func TestInvokeNeedsConfirm(t *testing.T) {
	f, _ := fsControl(t)
	ft := &fakeTrigger{agents: []config.Agent{{Name: "claude", Exec: config.ExecAction{Command: []string{"/bin/true"}}}}}
	f.coll.Trigger = ft
	s := NewServer(f.coll)

	w := call(t, s, "POST", "/agent/invoke", invokeBody("claude", []string{"/docs/b"}, "summarize", false))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "claude") || !strings.Contains(w.Body.String(), "confirm=true") {
		t.Fatalf("unconfirmed: %d %s", w.Code, w.Body.String())
	}
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"no paths":        {invokeBody("claude", nil, "x", true), 400},
		"relative path":   {invokeBody("claude", []string{"docs/b"}, "x", true), 400},
		"missing path":    {invokeBody("claude", []string{"/docs/b", "/nope"}, "x", true), 404},
		"unknown field":   {`{"agent":"claude","paths":["/docs/b"],"confirm":true,"argv":["x"]}`, 400},
		"prompt too long": {invokeBody("claude", []string{"/docs/b"}, strings.Repeat("x", 70<<10), true), 400},
	} {
		if w := call(t, s, "POST", "/agent/invoke", tc.body); w.Code != tc.code {
			t.Errorf("%s: %d %s", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	if w := call(t, s, "GET", "/agent/invoke", ""); w.Code != 405 {
		t.Errorf("GET: %d", w.Code)
	}
	if len(ft.invoked) != 0 {
		t.Fatalf("a refused request reached the engine: %+v", ft.invoked)
	}
	// A missing path answers like /agent/prompt does: 404 without internals.
	w = call(t, s, "POST", "/agent/invoke", invokeBody("claude", []string{"/nope"}, "x", true))
	if strings.Contains(w.Body.String(), f.dir) || strings.Contains(w.Body.String(), "ali") {
		t.Fatalf("404 body leaks: %s", w.Body.String())
	}

	// Confirmed and valid: the engine gets exactly what was sent, cleaned.
	w = call(t, s, "POST", "/agent/invoke", invokeBody("claude", []string{"/docs//b", "/docs/sub/../sub/c"}, "summarize", true))
	if w.Code != 200 {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body.String())
	}
	var r TriggerActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.ID != 1 {
		t.Fatalf("response: %v %s", err, w.Body.String())
	}
	if len(ft.invoked) != 1 || ft.invoked[0].agent != "claude" || ft.invoked[0].prompt != "summarize" ||
		strings.Join(ft.invoked[0].paths, ",") != "/docs/b,/docs/sub/c" {
		t.Fatalf("engine got %+v", ft.invoked)
	}
	// The engine's "already queued" is a conflict the page can name.
	ft.err = trigger.ErrAlreadyQueued
	if w := call(t, s, "POST", "/agent/invoke", invokeBody("claude", []string{"/docs/b"}, "again", true)); w.Code != 409 {
		t.Fatalf("already queued: %d %s", w.Code, w.Body.String())
	}
}

// TestInvokePassesPathsAsSeparateArgv runs the real engine end to end: the
// configured argv is executed element for element, {prompt} is one element
// however many spaces it has, and every path follows as its own element.
// printf '%s\n' prints one argument per line, which makes that visible.
func TestInvokePassesPathsAsSeparateArgv(t *testing.T) {
	printf := "/usr/bin/printf"
	if _, err := os.Stat(printf); err != nil {
		t.Skip("no /usr/bin/printf")
	}
	ag := config.Agent{Name: "claude", Exec: config.ExecAction{Command: []string{printf, `%s\n`, "-p", "{prompt}", "--"}, Timeout: 10 * time.Second}}
	f, _, eng := invokeFixture(t, []config.Agent{ag}, true)
	s := NewServer(f.coll)

	w := call(t, s, "POST", "/agent/invoke", invokeBody("claude", []string{"/docs/b", "/docs/sub/c"}, "Summarize these; echo pwned", true))
	if w.Code != 200 {
		t.Fatalf("invoke: %d %s", w.Code, w.Body.String())
	}
	var r TriggerActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.ID <= 0 {
		t.Fatalf("response: %v %s", err, w.Body.String())
	}
	var d agent.Delivery
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		d, err = eng.Delivery(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.State == agent.DeliveryDone || d.State == agent.DeliveryDead || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := "-p\nSummarize these; echo pwned\n--\n/docs/b\n/docs/sub/c\n"
	if d.State != agent.DeliveryDone || d.Output != want {
		t.Fatalf("delivery = %+v\noutput = %q\nwant %q", d, d.Output, want)
	}
	if d.Rule != "agent:claude" || d.Path != "/docs/b" || d.Kind != trigger.KindInvoke || d.Origin != trigger.OriginConsole {
		t.Fatalf("row = %+v", d)
	}
	// The row is reachable where the "view delivery" link points.
	if w := call(t, s, "GET", "/triggers/deliveries/"+strconv.FormatInt(r.ID, 10), ""); w.Code != 200 {
		t.Fatalf("delivery detail: %d %s", w.Code, w.Body.String())
	}
}

// TestInvokeIsAudited: every attempt that reaches the engine leaves an
// audit row for the console principal naming the agent and the paths, with
// the prompt's length but never its text; a merged (409) attempt is an
// error row.
func TestInvokeIsAudited(t *testing.T) {
	ag := config.Agent{Name: "summarize", Exec: config.ExecAction{Command: []string{"/usr/local/bin/agent-runner", "{prompt}"}}}
	f, st, _ := invokeFixture(t, []config.Agent{ag}, false)
	s := NewServer(f.coll)
	const prompt = "Summarize the secret plan"

	if w := call(t, s, "POST", "/agent/invoke", invokeBody("summarize", []string{"/docs/b", "/docs/sub/c"}, prompt, true)); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	// The engine has not run, so the row is still pending and a second run
	// on the same first path would be merged into it: 409.
	if w := call(t, s, "POST", "/agent/invoke", invokeBody("summarize", []string{"/docs/b"}, prompt+" again", true)); w.Code != 409 {
		t.Fatalf("second: %d %s", w.Code, w.Body.String())
	}
	// Refused before the engine: no row.
	call(t, s, "POST", "/agent/invoke", invokeBody("nope", []string{"/docs/b"}, prompt, true))
	call(t, s, "POST", "/agent/invoke", invokeBody("summarize", []string{"/docs/b"}, prompt, false))
	call(t, s, "GET", "/agent/endpoints", "")

	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Tool: "agent.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v", rows)
	}
	// Newest first.
	merged, ok := rows[0], rows[1]
	if ok.PrincipalID != "console" || ok.SessionID != "" || ok.Transport != "console" || ok.Result != "ok" ||
		strings.Join(ok.Paths, ",") != "/docs/b,/docs/sub/c" {
		t.Fatalf("ok row = %+v", ok)
	}
	var args struct {
		Agent     string `json:"agent"`
		PromptLen int    `json:"prompt_len"`
	}
	if err := json.Unmarshal(ok.Args, &args); err != nil || args.Agent != "summarize" || args.PromptLen != len(prompt) {
		t.Fatalf("ok args = %s (%v)", ok.Args, err)
	}
	if merged.Result != "error" || merged.Error == "" || merged.PrincipalID != "console" {
		t.Fatalf("merged row = %+v", merged)
	}
	for _, row := range rows {
		if strings.Contains(string(row.Args), "secret plan") || strings.Contains(row.Error, "secret plan") {
			t.Fatalf("the prompt text is in the audit: %+v", row)
		}
	}
	all, _, err := st.Audit(context.Background(), agent.AuditQuery{})
	if err != nil || len(all) != 2 {
		t.Fatalf("other routes wrote audit rows: %+v %v", all, err)
	}
}

// TestEndpointsListNamesOnly: the page learns the names it can offer and
// nothing about what they run.
func TestEndpointsListNamesOnly(t *testing.T) {
	ag := config.Agent{Name: "summarize", Exec: config.ExecAction{Command: []string{"/usr/local/bin/agent-runner", "--token", "hunter2", "{prompt}"}, Cwd: "/srv/agent"}}
	f, _, _ := invokeFixture(t, []config.Agent{ag}, false)
	s := NewServer(f.coll)
	w := call(t, s, "GET", "/agent/endpoints", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"agents":[{"name":"summarize"}]}` {
		t.Fatalf("endpoints: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("endpoints are not no-store")
	}
	if w := call(t, s, "POST", "/agent/endpoints", "{}"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
}
