import { api } from '/ui/api.js';
import { el, fill, showPanel } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { deliveryState, splitOutput, looksLikeWebhookOutput } from '/ui/trigger_view.js';

// The delivery panel: one trigger delivery in detail — which rule fired on
// which path, how many attempts it took, and what the action produced. GET
// /triggers/deliveries/<id> answers all of that in one round trip; the
// table rows carry no output, so this is where it is read.
//
// Everything in the output was written by the command or the remote
// endpoint: stdout, stderr, the webhook's response body. All of it is
// inserted as text nodes inside <pre>, never as markup. The same goes for
// the path and the error text.

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || String(iso).startsWith('0001') ? '' : d.toLocaleString(locale());
}

function fact(label, value) {
  if (value == null || value === '') return null;
  return el('div', { class: 'row', style: 'gap:12px;font-size:13px' },
    el('span', { class: 'muted', style: 'min-width:96px' }, label),
    el('span', { class: 'detail', style: 'word-break:break-all' }, value));
}

// stream is one pane of output. An empty stream is still shown, with its
// heading, so a reader can tell "nothing on stderr" from "not loaded".
function stream(label, text) {
  return el('div', {},
    el('div', { class: 'muted', style: 'font-size:12px;margin:10px 0 4px' }, label),
    el('pre', { class: 'snippet', style: 'max-height:32vh;overflow:auto' }, text || ''));
}

// body renders the loaded delivery. A row does not say which action its
// rule has, so the screen passes what it knows from the rules it loaded. A
// webhook's output is the "HTTP <status>" line the runner wrote followed by
// the response, shown as one pane; an exec's output is split at the
// runner's marker into stdout and stderr. Without a hint — a deep link to a
// rule that has since left the configuration — the status line decides.
function body(d, action) {
  const st = deliveryState(d);
  const out = splitOutput(d.output);
  const isWebhook = action === 'webhook' || (action !== 'exec' && looksLikeWebhookOutput(d.output));
  return el('div', {},
    el('div', { class: 'stack', style: 'gap:6px;margin-bottom:8px' },
      fact(t('triggers.col.rule'), d.rule),
      fact(t('triggers.col.path'), d.path || t('triggers.path.rescan')),
      fact(t('triggers.col.event'), d.kind ? t('triggers.event.' + d.kind) : ''),
      fact(t('triggers.col.origin'), d.origin ? t('triggers.origin.' + d.origin) : ''),
      fact(t('triggers.col.state'), el('span', { style: 'display:inline-flex;align-items:center;gap:7px' },
        el('span', { class: 'dot ' + st.dot }), t(st.key))),
      fact(t('triggers.col.attempts'), String(d.attempts || 0)),
      fact(t('triggers.first_seen'), when(d.first_seen)),
      fact(t('triggers.done_at'), when(d.done_at))),
    d.last_error ? el('div', { role: 'alert', style: 'color:var(--danger-text);margin:8px 0;word-break:break-all;font-size:13px' }, d.last_error) : null,
    d.truncated ? el('div', { class: 'banner warn', style: 'margin:8px 0' }, t('triggers.output_truncated')) : null,
    isWebhook
      ? stream(t('triggers.output.response'), d.output)
      : [stream(t('triggers.output.stdout'), out.stdout), stream(t('triggers.output.stderr'), out.stderr)]);
}

// openDeliveryPanel opens the panel for one delivery id. actionOf, when
// given, maps a rule name to its action type ('exec' | 'webhook'). The
// panel appears at once with a loading note and fills in when the daemon
// answers, so a slow read still shows something to close; a failed read
// shows the daemon's own message.
export function openDeliveryPanel(id, actionOf) {
  const content = el('div', {}, el('div', { class: 'dim' }, t('triggers.loading')));
  const done = showPanel({ title: t('triggers.delivery.title', String(id)), content, width: 720 });
  api.get('/triggers/deliveries/' + encodeURIComponent(String(id)))
    .then((d) => fill(content, body(d, actionOf ? actionOf(d.rule) : '')))
    .catch((e) => fill(content, el('div', { role: 'alert', style: 'color:var(--danger-text)' }, e.message)));
  return done;
}
