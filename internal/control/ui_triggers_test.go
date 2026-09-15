package control

import (
	"regexp"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/trigger"
)

// The triggers screen shows the rules the configuration file holds and the
// deliveries the engine made from them. These tests read the embedded
// sources, because what they guard has no runtime failure mode: a form that
// edits a rule would work fine and quietly contradict "the config file is
// the whitelist"; an argv joined with spaces renders and reads as a shell
// line it never was; a webhook secret in the DOM shows up nowhere but the
// DOM; a test run without the typed confirmation just runs.
var triggerScreenFiles = []string{"web/screens/triggers.js", "web/delivery_panel.js", "web/trigger_view.js"}

func triggerSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range triggerScreenFiles {
		out[name] = webSource(t, name)
	}
	return out
}

// The screen is read-only over rules: no route that creates, changes or
// removes one exists, and the page must not pretend otherwise. The only
// POSTs it makes are the two actions the daemon offers.
func TestTriggersScreenHasNoRuleEditor(t *testing.T) {
	srcs := triggerSources(t)
	posts := regexp.MustCompile(`api\.post\('([^']*)'`)
	for name, src := range srcs {
		for _, verb := range []string{"api.put(", "api.patch(", "api.del("} {
			if strings.Contains(src, verb) {
				t.Errorf("%s calls %s; rules live in the configuration file and the page has no write route", name, verb)
			}
		}
		for _, m := range posts.FindAllStringSubmatch(src, -1) {
			if m[1] != "/triggers/test" && m[1] != "/triggers/retry" {
				t.Errorf("%s posts to %s; only /triggers/test and /triggers/retry exist", name, m[1])
			}
		}
		if strings.Contains(src, "'/triggers/rules'") || strings.Contains(src, "'/config'") {
			t.Errorf("%s reaches a rule-editing route", name)
		}
	}
	screen := srcs["web/screens/triggers.js"]
	if !strings.Contains(screen, "api.get('/triggers')") {
		t.Error("the screen never loads the rules")
	}
	if !strings.Contains(screen, "t('triggers.edit_in_config')") {
		t.Error("the rule cards do not say that rules are edited in the configuration file")
	}
	// The one form on the page is the test-delivery form: a rule select and
	// a path, nothing that spells a rule.
	form := strings.Index(screen, "openForm(")
	if form < 0 {
		t.Fatal("the screen has no test-delivery form")
	}
	for _, forbidden := range []string{"placeholder: t('triggers.rule.name')", "'events'", "'origins'", "'debounce'"} {
		if strings.Contains(screen[form:], forbidden) {
			t.Errorf("the test form carries a rule field (%s); it must only pick a rule and a path", forbidden)
		}
	}
}

// An exec argv is a list, and the page keeps it one: each element in its
// own <code>, never joined into a line that reads as a shell command.
func TestArgvRendersPerElement(t *testing.T) {
	srcs := triggerSources(t)
	for name, src := range srcs {
		for _, join := range []string{".join(' ')", `.join(" ")`, ".join('')", ".join(\"\")"} {
			if strings.Contains(src, join) {
				t.Errorf("%s joins argv elements (%s); each one must stay separate", name, join)
			}
		}
	}
	view := srcs["web/trigger_view.js"]
	if !strings.Contains(view, "export function argvCells(") {
		t.Fatal("trigger_view.js does not export argvCells")
	}
	if strings.Contains(view, "import ") {
		t.Error("trigger_view.js must have no imports so node can test it directly")
	}
	screen := srcs["web/screens/triggers.js"]
	if !strings.Contains(screen, "argvCells(") {
		t.Error("the screen does not render argv through argvCells")
	}
	if !regexp.MustCompile(`argvCells\([^)]*\)\.map\(\([a-z]+\) => el\('code'`).MatchString(screen) {
		t.Error("the screen must map each argv cell to its own <code> element")
	}
}

// The daemon strips the secret before answering; the page must not even
// look for it. secret_configured is the one field it reads.
func TestWebhookSecretNeverInDOM(t *testing.T) {
	srcs := triggerSources(t)
	reads := regexp.MustCompile(`\.secret([^_a-zA-Z0-9]|$)`)
	literal := regexp.MustCompile(`['"]secret['"]`)
	for name, src := range srcs {
		if m := reads.FindString(src); m != "" {
			t.Errorf("%s reads a secret field (%q); the daemon never sends one and the page must not ask", name, m)
		}
		if literal.MatchString(src) {
			t.Errorf("%s names a secret property", name)
		}
	}
	screen := srcs["web/screens/triggers.js"]
	if !strings.Contains(screen, "secret_configured") || !strings.Contains(screen, "t('triggers.secret_configured')") {
		t.Error("a webhook card must say whether a signing secret is configured, and only that")
	}
	if !strings.Contains(screen, "webhook.url") {
		t.Error("a webhook card must show its URL")
	}
}

// A test delivery runs the rule's action for real, so it is confirmed the
// way a deletion is: the form picks the rule and path, the typed sheet takes
// the rule's name, and only then does the POST go out with confirm: true.
func TestTestDeliveryConfirms(t *testing.T) {
	screen := webSource(t, "web/screens/triggers.js")
	post := strings.Index(screen, "api.post('/triggers/test'")
	if post < 0 {
		t.Fatal("the screen never posts a test delivery")
	}
	body := screen[post:]
	if end := strings.Index(body, ")"); end > 0 {
		body = body[:end]
	}
	for _, want := range []string{"name:", "path:", "confirm: true"} {
		if !strings.Contains(body, want) {
			t.Errorf("the test post lacks %s: %s", want, body)
		}
	}
	confirm := strings.LastIndex(screen[:post], "confirmDelete(")
	form := strings.LastIndex(screen[:post], "openForm(")
	if form < 0 || confirm < 0 || form > confirm {
		t.Fatalf("a test run must go openForm (%d) → confirmDelete (%d) → post (%d)", form, confirm, post)
	}
	sheet := screen[confirm:post]
	if !strings.Contains(sheet, "confirmToken: ") {
		t.Error("the confirmation sheet has no token to type")
	}
	if !strings.Contains(sheet, "if (!") || !strings.Contains(sheet, "return") {
		t.Error("the post is not guarded by the sheet's answer")
	}
	// The post is followed by the delivery panel opening on the new id.
	after := screen[post:]
	if !strings.Contains(after, "openDeliveryPanel(") {
		t.Error("a finished test run must open the delivery it created")
	}
}

// A dead delivery has a retry control, and only a dead one: retry is the
// sole state change a person can make to a row.
func TestDeadRowRetries(t *testing.T) {
	screen := webSource(t, "web/screens/triggers.js")
	if !strings.Contains(screen, "api.post('/triggers/retry', { id: ") {
		t.Fatal("the screen never posts a retry with the delivery id")
	}
	retry := strings.Index(screen, "api.post('/triggers/retry'")
	guard := strings.LastIndex(screen[:retry], "=== 'dead'")
	if guard < 0 {
		t.Error("the retry control is not limited to dead rows")
	}
	if !strings.Contains(screen, "t('triggers.retry')") {
		t.Error("the retry control has no translated label")
	}
}

// What a delivery produced was written by the command or the remote
// endpoint. It goes in as text nodes inside <pre>, never as markup, and the
// two streams are shown apart with a note when either was cut.
func TestDeliveryOutputIsText(t *testing.T) {
	srcs := triggerSources(t)
	panel := srcs["web/delivery_panel.js"]
	view := srcs["web/trigger_view.js"]
	for _, name := range []string{"web/delivery_panel.js", "web/screens/triggers.js"} {
		if strings.Contains(srcs[name], "html:") {
			t.Errorf("%s uses the html attribute; delivery output is untrusted", name)
		}
	}
	for _, want := range []string{
		"api.get('/triggers/deliveries/' + encodeURIComponent(",
		"showPanel(",
		"el('pre'",
		"splitOutput(",
		"t('triggers.output_truncated')",
		"d.truncated",
		"export function openDeliveryPanel(",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("delivery_panel.js lacks %s", want)
		}
	}
	if !strings.Contains(view, "'\\n--- stderr ---\\n'") || !strings.Contains(view, "export function splitOutput(") {
		t.Error("trigger_view.js must split the output on the runner's stderr marker")
	}
	if !strings.Contains(panel, "last_error") {
		t.Error("the panel never shows the delivery's last error")
	}
	// A deep link opens the panel on load, so a toast elsewhere can point at
	// a delivery.
	screen := srcs["web/screens/triggers.js"]
	if !strings.Contains(screen, "get('delivery')") || !strings.Contains(screen, "openDeliveryPanel(") {
		t.Error("the screen does not open #/triggers?delivery=<id> on load")
	}
}

// The nav item sits after the index item, carries the bolt icon and a
// badge that counts dead deliveries — read from the status tick's triggers
// line, which a daemon without an engine omits.
func TestTriggersNavAndBadge(t *testing.T) {
	router := webSource(t, "web/router.js")
	for _, want := range []string{"'#/triggers': 'triggers-view'", "hash: '#/triggers', icon: 'bolt', key: 'nav.triggers', badge: 'triggers'"} {
		if !strings.Contains(router, want) {
			t.Errorf("router.js lacks %s", want)
		}
	}
	index := strings.Index(router, "hash: '#/index'")
	triggers := strings.Index(router, "hash: '#/triggers'")
	storage := strings.Index(router, "hash: '#/storage'")
	if index < 0 || triggers < 0 || storage < 0 || index > triggers || triggers > storage {
		t.Errorf("the triggers nav item must come after index and before storage (index=%d triggers=%d storage=%d)", index, triggers, storage)
	}
	app := webSource(t, "web/app.js")
	for _, want := range []string{
		"'triggers-view': renderTriggers",
		"item.badge === 'triggers'",
		"status.triggers",
		".dead",
		"export function onTriggerEvent(",
		"onTrigger:",
		"refreshNavBadges()",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	// A trigger event may change the dead count between two status ticks,
	// so the handler refreshes the badge itself.
	handler := ""
	if at := strings.Index(app, "onTrigger:"); at >= 0 {
		handler = app[at:]
		if end := strings.Index(handler, "\n"); end > 0 {
			handler = handler[:end]
		}
	}
	if !strings.Contains(handler, "refreshNavBadges()") {
		t.Error("the onTrigger handler does not refresh the nav badge")
	}
	screen := webSource(t, "web/screens/triggers.js")
	if !strings.Contains(screen, "onTriggerEvent(") {
		t.Error("the screen never subscribes to trigger events")
	}
	if !strings.Contains(screen, "paged") {
		t.Error("the screen has no paged guard for live refreshes")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"nav.triggers", "nav.triggers.dead", "triggers.secret_configured", "triggers.self_trigger", "triggers.edit_in_config", "triggers.output_truncated", "triggers.retry", "triggers.all", "triggers.state.unknown"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

// Delivery states, event kinds and origins arrive from the daemon and are
// translated by assembled keys, which the literal scan cannot see.
func TestWebCatalogCoversTriggerEnums(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, s := range agent.DeliveryStates {
		if !zh["triggers.state."+s] {
			t.Errorf("missing triggers.state.%s", s)
		}
	}
	for _, e := range config.TriggerEvents {
		if !zh["triggers.event."+e] {
			t.Errorf("missing triggers.event.%s", e)
		}
	}
	// A test run and an agent run are queued by the console with their own
	// kind and origin (trigger.KindTest, KindInvoke, OriginConsole).
	for _, o := range append(append([]string{}, config.TriggerOrigins...), trigger.OriginConsole) {
		if !zh["triggers.origin."+o] {
			t.Errorf("missing triggers.origin.%s", o)
		}
	}
	for _, e := range []string{trigger.KindTest, trigger.KindInvoke} {
		if !zh["triggers.event."+e] {
			t.Errorf("missing triggers.event.%s", e)
		}
	}
	for _, a := range []string{"exec", "webhook"} {
		if !zh["triggers.action."+a] {
			t.Errorf("missing triggers.action.%s", a)
		}
	}
}

func TestTriggersModulesStayShort(t *testing.T) {
	for _, name := range triggerScreenFiles {
		if n := strings.Count(webSource(t, name), "\n"); n >= 800 {
			t.Errorf("%s is %d lines; split it", name, n)
		}
	}
}
