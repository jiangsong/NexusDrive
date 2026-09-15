package control

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/trigger"
)

// resolvedHookSecret is what the fixture's secret resolver answers for the
// webhook's keyring reference; neither it nor the reference may ever reach
// a response body.
const resolvedHookSecret = "topsecret-hmac-key-value"

func triggerRules() []config.Trigger {
	all := func(s []string) []string { return append([]string(nil), s...) }
	return []config.Trigger{
		{
			Name: "inbox", Paths: []string{"/work/inbox/**"}, Events: []string{"write", "create"}, Origins: []string{"kernel", "remote"},
			Debounce: 2 * time.Second, OnRescan: config.OnRescanDeliver,
			Action: config.TriggerAction{Exec: &config.ExecAction{Command: []string{"/bin/cat", "{path}", "; rm -rf /"}, Cwd: "/tmp", Timeout: time.Minute}},
		},
		{
			Name: "hook", Paths: []string{"/**"}, Events: all(config.TriggerEvents), Origins: all(config.TriggerOrigins),
			Debounce: time.Second, OnRescan: config.OnRescanIgnore,
			Action: config.TriggerAction{Webhook: &config.WebhookAction{
				URL: "https://hooks.example/cloudfs", Secret: "keyring:hook-secret", Timeout: 15 * time.Second,
				IncludeDownloadURL: true, Proxy: "direct", Insecure: false,
			}},
		},
	}
}

// triggerFixture wires an engine over a fresh agent.db into the collector
// the way the daemon does. The engine is not run: the routes are what is
// under test, and a worker racing to claim the rows they create would make
// every state assertion a coin toss.
func triggerFixture(t *testing.T) (*fixture, *agent.Store, *trigger.Engine) {
	t.Helper()
	f := newFixture(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.coll.Agent = NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "", RollbackDeps{})
	eng := trigger.New(trigger.Options{
		Store: st, Rules: triggerRules(),
		Agents:  []config.Agent{{Name: "summarize", Exec: config.ExecAction{Command: []string{"/usr/local/bin/agent-runner", "{prompt}"}}}},
		Secrets: func(string) (string, error) { return resolvedHookSecret, nil },
	})
	t.Cleanup(eng.Close)
	f.coll.Trigger = eng
	return f, st, eng
}

// deadDelivery makes one row that has spent its retries.
func deadDelivery(t *testing.T, st *agent.Store, rule, path string) int64 {
	t.Helper()
	ctx := context.Background()
	q := st.Deliveries()
	id, _, err := q.Enqueue(ctx, rule, path, "write", "kernel", time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.Claim(ctx, rule, time.Now()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := q.Dead(ctx, id, "exit status 1", "boom"); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestTriggersViewNeverContainsTheSecret: the rules view is what the
// console renders, and a webhook's signing key is the one thing in the
// configuration that must not be there — neither the keyring reference nor
// what it resolves to. The view says only that a secret is configured.
func TestTriggersViewNeverContainsTheSecret(t *testing.T) {
	f, _, _ := triggerFixture(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/triggers", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, leak := range []string{"keyring:hook-secret", "hook-secret", resolvedHookSecret, `"secret":`} {
		if strings.Contains(body, leak) {
			t.Fatalf("the rules view leaks %q:\n%s", leak, body)
		}
	}
	var res TriggersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || len(res.Rules) != 2 || len(res.Agents) != 1 || res.Agents[0].Name != "summarize" {
		t.Fatalf("view: %+v", res)
	}
	exec, hook := res.Rules[0], res.Rules[1]
	if exec.Name != "inbox" || exec.Action.Type != "exec" || exec.Action.Exec == nil || exec.Action.Webhook != nil {
		t.Fatalf("exec rule: %+v", exec)
	}
	// argv is handed over element by element so the page can render it
	// that way; "; rm -rf /" stays one literal argument.
	if got := exec.Action.Exec.Command; len(got) != 3 || got[0] != "/bin/cat" || got[1] != "{path}" || got[2] != "; rm -rf /" {
		t.Fatalf("argv: %q", got)
	}
	if exec.Action.Exec.Cwd != "/tmp" || exec.Action.Exec.Timeout != "1m0s" || exec.Debounce != "2s" || exec.OnRescan != "deliver" {
		t.Fatalf("exec rule fields: %+v", exec)
	}
	if len(exec.Events) != 2 || len(exec.Origins) != 2 || len(exec.Paths) != 1 {
		t.Fatalf("exec rule filters: %+v", exec)
	}
	if hook.Action.Type != "webhook" || hook.Action.Webhook == nil || hook.Action.Exec != nil {
		t.Fatalf("webhook rule: %+v", hook)
	}
	wh := hook.Action.Webhook
	if wh.URL != "https://hooks.example/cloudfs" || !wh.SecretConfigured || wh.Timeout != "15s" || !wh.IncludeDownloadURL || wh.Proxy != "direct" || wh.Insecure {
		t.Fatalf("webhook fields: %+v", wh)
	}
	// An agent is only a name here: its command is defined in the config
	// file and the console never needs it to offer a run.
	if strings.Contains(body, "agent-runner") {
		t.Fatalf("the agents list carries the command:\n%s", body)
	}
}

// TestTriggerTestNeedsConfirm: a test delivery runs the rule's action for
// real, so it is refused without confirm=true, naming the consequence, and
// no row is queued by the refusal. Confirmed, the row is queued through the
// engine with kind "test".
func TestTriggerTestNeedsConfirm(t *testing.T) {
	f, st, _ := triggerFixture(t)
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "POST", "/triggers/test", `{"name":"inbox","path":"/work/inbox/a.txt"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "confirm=true") || !strings.Contains(w.Body.String(), "inbox") {
		t.Fatalf("unconfirmed test: %d %s", w.Code, w.Body)
	}
	if rows, _, err := st.Deliveries().List(context.Background(), agent.DeliveryQuery{}); err != nil || len(rows) != 0 {
		t.Fatalf("a refused test queued a delivery: %v %v", rows, err)
	}
	w = uiCallControl(t, h, "POST", "/triggers/test?lang=zh", `{"name":"inbox","path":"/work/inbox/a.txt"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), i18n.T(i18n.ZH, "confirm.trigger.test", "inbox", "/work/inbox/a.txt")) {
		t.Fatalf("unconfirmed test in Chinese: %d %s", w.Code, w.Body)
	}

	w = uiCallControl(t, h, "POST", "/triggers/test", `{"name":"inbox","path":"/work/inbox/a.txt","confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("confirmed test: %d %s", w.Code, w.Body)
	}
	var res TriggerActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("test response: %s %v", w.Body, err)
	}
	d, err := st.Deliveries().Get(context.Background(), res.ID)
	if err != nil || d.Rule != "inbox" || d.Path != "/work/inbox/a.txt" || d.Kind != trigger.KindTest || d.Origin != trigger.OriginConsole || d.State != agent.DeliveryPending {
		t.Fatalf("queued row: %+v %v", d, err)
	}

	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"name":"nope","path":"/x","confirm":true}`, http.StatusNotFound},
		{`{"name":"inbox","path":"relative","confirm":true}`, http.StatusBadRequest},
		{`{"name":"","path":"/x","confirm":true}`, http.StatusNotFound},
	} {
		if w := uiCallControl(t, h, "POST", "/triggers/test", tc.body); w.Code != tc.code {
			t.Fatalf("%s: %d %s, want %d", tc.body, w.Code, w.Body, tc.code)
		}
	}
	if w := uiCallControl(t, h, "GET", "/triggers/test", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET test: %d", w.Code)
	}
}

// TestRetryOnlyDeadDeliveries: a pending or done row is already on its way
// or finished, so retrying it is a conflict; a dead row reopens as pending
// through the engine, which is the only writer allowed to change a pending
// row (its debounce cache must learn about it).
func TestRetryOnlyDeadDeliveries(t *testing.T) {
	f, st, _ := triggerFixture(t)
	h := NewServer(f.coll).Handler()
	ctx := context.Background()
	q := st.Deliveries()
	pending, _, err := q.Enqueue(ctx, "hook", "/a", "write", "kernel", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := func(id int64) string { return `{"id":` + jsonInt(id) + `}` }
	if w := uiCallControl(t, h, "POST", "/triggers/retry", body(pending)); w.Code != http.StatusConflict {
		t.Fatalf("retry of a pending row: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/triggers/retry", `{"id":99999}`); w.Code != http.StatusNotFound {
		t.Fatalf("retry of a missing row: %d %s", w.Code, w.Body)
	}
	dead := deadDelivery(t, st, "inbox", "/work/inbox/b.txt")
	w := uiCallControl(t, h, "POST", "/triggers/retry", body(dead))
	if w.Code != 200 {
		t.Fatalf("retry of a dead row: %d %s", w.Code, w.Body)
	}
	var res TriggerActionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.ID != dead {
		t.Fatalf("retry response: %s %v", w.Body, err)
	}
	d, err := q.Get(ctx, dead)
	if err != nil || d.State != agent.DeliveryPending || d.Attempts != 1 {
		t.Fatalf("after retry: %+v %v", d, err)
	}
	// Once reopened it is pending, so a second retry is the conflict again.
	if w := uiCallControl(t, h, "POST", "/triggers/retry", body(dead)); w.Code != http.StatusConflict {
		t.Fatalf("second retry: %d %s", w.Code, w.Body)
	}
}

func jsonInt(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// TestDeliveryDetailCarriesOutputAndTruncation: the list is what the table
// shows and stays light; the detail carries the action's output and says
// whether the runner cut it. Paging and filters follow the sessions route.
func TestDeliveryDetailCarriesOutputAndTruncation(t *testing.T) {
	f, st, _ := triggerFixture(t)
	h := NewServer(f.coll).Handler()
	ctx := context.Background()
	q := st.Deliveries()
	first, _, err := q.Enqueue(ctx, "inbox", "/work/inbox/a.txt", "write", "kernel", time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.Claim(ctx, "inbox", time.Now()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := q.Done(ctx, first, "hello from cat"+agent.OutputTruncatedMarker); err != nil {
		t.Fatal(err)
	}
	second, _, err := q.Enqueue(ctx, "hook", "/b", "remove", "api", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	w := uiCallControl(t, h, "GET", "/triggers/deliveries/"+jsonInt(first), "")
	if w.Code != 200 {
		t.Fatalf("detail: %d %s", w.Code, w.Body)
	}
	var d agent.Delivery
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != first || d.State != agent.DeliveryDone || !strings.HasPrefix(d.Output, "hello from cat") || !d.Truncated || d.Attempts != 1 || d.Kind != "write" || d.Origin != "kernel" {
		t.Fatalf("detail: %+v", d)
	}
	if !strings.Contains(w.Body.String(), `"truncated":true`) {
		t.Fatalf("detail lacks the truncation flag: %s", w.Body)
	}

	w = uiCallControl(t, h, "GET", "/triggers/deliveries", "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var list DeliveriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deliveries) != 2 || list.NextCursor != "" {
		t.Fatalf("list: %s %v", w.Body, err)
	}
	if list.Deliveries[0].ID != second || list.Deliveries[1].ID != first {
		t.Fatalf("list is not newest first: %+v", list.Deliveries)
	}
	if strings.Contains(w.Body.String(), "hello from cat") {
		t.Fatalf("the list carries the output; only the detail should:\n%s", w.Body)
	}
	if !list.Deliveries[1].Truncated {
		t.Fatalf("the list row lost its truncation flag: %+v", list.Deliveries[1])
	}

	w = uiCallControl(t, h, "GET", "/triggers/deliveries?limit=1", "")
	list = DeliveriesResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deliveries) != 1 || list.Deliveries[0].ID != second || list.NextCursor == "" {
		t.Fatalf("page 1: %d %s %v", w.Code, w.Body, err)
	}
	w = uiCallControl(t, h, "GET", "/triggers/deliveries?limit=1&cursor="+list.NextCursor, "")
	list = DeliveriesResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deliveries) != 1 || list.Deliveries[0].ID != first || list.NextCursor != "" {
		t.Fatalf("page 2: %d %s %v", w.Code, w.Body, err)
	}
	w = uiCallControl(t, h, "GET", "/triggers/deliveries?rule=hook&state=pending", "")
	list = DeliveriesResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deliveries) != 1 || list.Deliveries[0].ID != second {
		t.Fatalf("filtered: %d %s %v", w.Code, w.Body, err)
	}
	w = uiCallControl(t, h, "GET", "/triggers/deliveries?rule=hook&state=done", "")
	list = DeliveriesResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deliveries) != 0 || list.Deliveries == nil {
		t.Fatalf("empty filter must be an empty array: %d %s %v", w.Code, w.Body, err)
	}
	for _, bad := range []string{"/triggers/deliveries?state=bogus", "/triggers/deliveries?cursor=x", "/triggers/deliveries?limit=0"} {
		if w := uiCallControl(t, h, "GET", bad, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	for _, missing := range []string{"/triggers/deliveries/99999", "/triggers/deliveries/abc", "/triggers/deliveries/1/x"} {
		if w := uiCallControl(t, h, "GET", missing, ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", missing, w.Code, w.Body)
		}
	}
}

// TestTriggerEventsReachSSE: every delivery state change the store publishes
// reaches the page as a "trigger" event with what the table row needs.
func TestTriggerEventsReachSSE(t *testing.T) {
	f, st, _ := triggerFixture(t)
	srv := httptest.NewServer(NewServer(f.coll).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- event + " " + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	if first := <-events; !strings.HasPrefix(first, "status ") {
		t.Fatalf("first event should be the status document: %s", first)
	}
	id, _, err := st.Deliveries().Enqueue(context.Background(), "inbox", "/work/inbox/a.txt", "write", "kernel", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if !strings.HasPrefix(ev, "trigger ") {
				continue
			}
			var got TriggerEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "trigger ")), &got); err != nil {
				t.Fatalf("trigger event: %s %v", ev, err)
			}
			if got.ID != id || got.Rule != "inbox" || got.State != agent.DeliveryPending || got.Attempts != 0 || got.Path != "/work/inbox/a.txt" || got.Kind != "write" {
				t.Fatalf("trigger event: %s", ev)
			}
			return
		case <-deadline:
			t.Fatal("no trigger event arrived")
		}
	}
}

// TestDoctorSurfacesConfigWarnings: what Validate accepted but warned about
// — an exec rule the agent's own writes can fire — is a doctor warning, so
// it is seen after the mount's log line has scrolled away.
func TestDoctorSurfacesConfigWarnings(t *testing.T) {
	cfg := &config.Config{Triggers: triggerRules(), Warnings: []string{
		"trigger inbox: exec may be fired by the agent's own writes; set origins: [kernel, remote]",
	}}
	checks := (&Doctor{Config: func() *config.Config { return cfg }}).Run(context.Background())
	c, ok := checkByName(checks, "triggers_config")
	if !ok || c.Level != LevelWarn || !strings.Contains(c.Detail, "trigger inbox: exec may be fired") || c.Fix == "" {
		t.Fatalf("triggers_config: %+v", c)
	}
	if zh := c.Localize(i18n.ZH); !strings.Contains(zh.Detail, "trigger inbox") || zh.Detail == c.Detail {
		t.Fatalf("triggers_config did not localize: %+v", zh)
	}

	clean := &config.Config{Triggers: triggerRules()}
	checks = (&Doctor{Config: func() *config.Config { return clean }}).Run(context.Background())
	if c, ok := checkByName(checks, "triggers_config"); !ok || c.Level != LevelOK || !strings.Contains(c.Detail, "2 ") {
		t.Fatalf("clean triggers_config: %+v %v", c, ok)
	}
	// A configuration without rules has nothing to report on.
	none := &config.Config{}
	if c, ok := checkByName((&Doctor{Config: func() *config.Config { return none }}).Run(context.Background()), "triggers_config"); ok {
		t.Fatalf("triggers_config reported without rules: %+v", c)
	}
	if _, ok := checkByName((&Doctor{}).Run(context.Background()), "triggers_config"); ok {
		t.Fatal("triggers_config reported without a configuration")
	}
}

// TestStatusCountsDeliveries: the status document and the metrics carry
// the two numbers a person acts on — what is waiting and what is dead.
func TestStatusCountsDeliveries(t *testing.T) {
	f, st, _ := triggerFixture(t)
	h := NewServer(f.coll).Handler()
	ctx := context.Background()
	for _, p := range []string{"/a", "/b"} {
		if _, _, err := st.Deliveries().Enqueue(ctx, "hook", p, "write", "kernel", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	deadDelivery(t, st, "inbox", "/work/inbox/c.txt")
	w := uiCallControl(t, h, "GET", "/status", "")
	var s Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if s.Triggers == nil || !s.Triggers.Enabled || s.Triggers.Pending != 2 || s.Triggers.Dead != 1 {
		t.Fatalf("triggers status: %+v", s.Triggers)
	}
	body := uiCallControl(t, h, "GET", "/metrics", "").Body.String()
	for _, want := range []string{"cloudfs_trigger_deliveries_pending 2\n", "cloudfs_trigger_deliveries_dead 1\n", "# TYPE cloudfs_trigger_deliveries_dead gauge"} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
}

// TestTriggersRoutesWhenNoEngine: a daemon without rules runs no engine.
// The rules view says so, so the console can show the configuration
// examples, and everything that needs the queue is a 404 rather than a
// pretence. Status and metrics simply lack the trigger line.
func TestTriggersRoutesWhenNoEngine(t *testing.T) {
	f := newFixture(t)
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/triggers", "")
	var res TriggersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 200 || res.Enabled || res.Rules == nil || res.Agents == nil {
		t.Fatalf("rules view: %d %s %v", w.Code, w.Body, err)
	}
	for _, target := range []string{"/triggers/deliveries", "/triggers/deliveries/1"} {
		if w := uiCallControl(t, h, "GET", target, ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
	}
	for _, target := range []string{"/triggers/test", "/triggers/retry"} {
		if w := uiCallControl(t, h, "POST", target, `{"id":1,"name":"x","path":"/x","confirm":true}`); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
	}
	if w := uiCallControl(t, h, "GET", "/status", ""); w.Code != 200 || strings.Contains(w.Body.String(), `"triggers"`) {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/metrics", ""); strings.Contains(w.Body.String(), "cloudfs_trigger_deliveries") {
		t.Fatal("metrics report a queue that does not exist")
	}
	want := map[string]bool{"/triggers": false, "/triggers/deliveries": false, "/triggers/deliveries/": false, "/triggers/test": false, "/triggers/retry": false}
	for _, r := range NewServer(f.coll).routes() {
		if _, ok := want[r.pattern]; ok {
			want[r.pattern] = true
			if r.open {
				t.Errorf("%s is marked open; every trigger route names local state", r.pattern)
			}
			if strings.HasSuffix(r.pattern, "/") && r.probe == "" {
				t.Errorf("%s has no probe path for the guard test", r.pattern)
			}
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("%s is not registered in routes()", pattern)
		}
	}
}
