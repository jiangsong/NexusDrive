// What the triggers screen decides about a rule or a delivery before it
// draws anything. No DOM and no imports, so node runs these decisions
// directly (_tests/trigger_view.test.mjs); the screen only renders what
// comes back.

// STDERR_MARKER is what the exec runner puts between the two streams when
// it stores them in one output field (internal/trigger/exec.go).
const STDERR_MARKER = '\n--- stderr ---\n';

// selfTriggerRisk says whether an agent's own write could fire this rule
// back at the agent: an exec action whose origins still include api. An
// empty or missing origins list is "all of them", which includes api. A
// webhook is a notification and cannot loop the same way.
export function selfTriggerRisk(rule) {
  if (!rule || !rule.action || rule.action.type !== 'exec') return false;
  const origins = Array.isArray(rule.origins) ? rule.origins : [];
  return origins.length === 0 || origins.includes('api');
}

// argvCells is the argv as the card renders it: one string per element, in
// order, never joined. An element like "{path}; rm -rf /" is one argument
// to the command and must be read as one — the screen puts each cell in its
// own <code> for that reason. Anything but an array has no cells.
export function argvCells(command) {
  if (!Array.isArray(command)) return [];
  return command.map((arg) => (arg == null ? '' : String(arg)));
}

// ruleSummary normalises a rule from GET /triggers: the daemon sends every
// list, but the screen still reads them defensively. Empty events or
// origins mean "all", which the screen spells out.
export function ruleSummary(rule) {
  const r = rule || {};
  const action = r.action || {};
  const list = (v) => (Array.isArray(v) ? v.map(String) : []);
  return {
    name: r.name == null ? '' : String(r.name),
    paths: list(r.paths),
    events: list(r.events),
    origins: list(r.origins),
    action: action.type === 'exec' || action.type === 'webhook' ? action.type : '',
    debounce: r.debounce == null ? '' : String(r.debounce),
    onRescan: r.on_rescan == null ? '' : String(r.on_rescan),
  };
}

const STATE_DOTS = { pending: '', running: 'warn', done: 'ok', dead: 'bad' };

// deliveryState is the dot colour and the catalog key for a delivery row.
// The word is always shown beside the dot: colour alone is not a label.
export function deliveryState(d) {
  const state = d && typeof d.state === 'string' ? d.state : '';
  if (!Object.prototype.hasOwnProperty.call(STATE_DOTS, state)) {
    return { dot: '', key: 'triggers.state.unknown' };
  }
  return { dot: STATE_DOTS[state], key: 'triggers.state.' + state };
}

// looksLikeWebhookOutput says whether an output is a webhook's: the runner
// writes "HTTP <status>" as its first line, after an optional note. An exec
// output has no such line unless the command printed one itself, and the
// screen normally knows the rule's action anyway; this only decides for a
// delivery whose rule has left the configuration.
export function looksLikeWebhookOutput(output) {
  return output != null && /(^|\n)HTTP \d{3}\b/.test(String(output));
}

// splitOutput divides a delivery's output at the first stderr marker. The
// runner writes the marker only when stderr had something, so an output
// without one is all stdout. Only the first marker counts: a command that
// prints the marker itself does not get to move the rest of its stdout into
// the stderr pane.
export function splitOutput(output) {
  const text = output == null ? '' : String(output);
  const at = text.indexOf(STDERR_MARKER);
  if (at < 0) return { stdout: text, stderr: '' };
  return { stdout: text.slice(0, at), stderr: text.slice(at + STDERR_MARKER.length) };
}

// The empty state's three code blocks. They are configuration and Go, not
// prose, so they are not translated; every label around them is.
export const EXEC_EXAMPLE = `triggers:
  - name: inbox-to-agent
    paths: ["/work/inbox/**"]
    events: [create, write, rename]
    origins: [kernel, remote]
    debounce: 2s
    action:
      exec:
        command: ["/usr/local/bin/summarize", "{path}"]
        cwd: ~/work
        timeout: 10m
`;

export const WEBHOOK_EXAMPLE = `triggers:
  - name: notify
    paths: ["/work/reports/**"]
    events: [write]
    action:
      webhook:
        url: https://hooks.example/cloudfs
        secret: keyring:cloudfs/hook
        timeout: 15s
`;

export const VERIFY_SNIPPET = `func verify(secret []byte, r *http.Request, body []byte, now time.Time) bool {
	ts := r.Header.Get("X-CloudFS-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false // reject replays
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-CloudFS-Signature")))
}
`;
