import { api } from '/ui/api.js';
import { el, fill, copyBtn } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { stdioWarning } from '/ui/connect_view.js';

// The connect panel sits at the top of the agents screen and answers the
// first question a person has there: how does an agent reach this mount? It
// reads GET /mcp/connect — whether the HTTP transport is listening and
// where, whether this process owns the storage, and the registration
// command with a <token> placeholder — and never a token. It is a
// <details> so it folds away once the answer is known; the setup finish
// page links here with ?connect=1 to open it.
//
// Three states matter. HTTP on: show the address and the registration
// command with a copy button, and say where a token comes from. HTTP off:
// show the configuration lines that turn it on. And, whenever the daemon
// reports a stdio server running beside the mount, a warning banner that
// links to the diagnostics item, because that server is a separate view of
// the files and nothing done through it shows up here.
const CONFIG_HINT = 'mcp:\n  http: 127.0.0.1:8765';

function stateLine(c) {
  const dot = c.http_listening ? 'ok' : 'warn';
  return el('span', { style: 'display:inline-flex;align-items:center;gap:8px' },
    el('span', { class: 'dot ' + dot }),
    c.http_listening ? t('connect.http.on', c.http_addr || '') : t('connect.http.off'));
}

function body(c) {
  const parts = [];
  parts.push(el('div', { class: 'detail', style: 'margin-top:8px' },
    el('span', { style: 'display:inline-flex;align-items:center;gap:8px' },
      el('span', { class: 'dot ' + (c.owner ? 'ok' : 'warn') }),
      t(c.owner ? 'connect.owner.yes' : 'connect.owner.no'))));
  const warning = stdioWarning(c);
  if (warning) {
    parts.push(el('div', { class: 'banner warn', role: 'alert', 'data-stdio-warning': '', style: 'margin-top:12px' },
      t(warning.key), ' ',
      el('a', { href: warning.href }, t(warning.linkKey))));
  }
  if (!c.http_listening) {
    parts.push(el('p', { class: 'detail', style: 'margin:12px 0 6px' }, t('connect.http.hint')));
    parts.push(el('pre', { class: 'detail snippet' }, CONFIG_HINT));
    parts.push(el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(CONFIG_HINT)));
    return parts;
  }
  // What `cloudfs mcp install` will pick with no --transport: http while
  // the listener is up, stdio otherwise (which beside a running mount
  // cannot write without the bridge).
  parts.push(el('div', { class: 'detail', style: 'margin-top:8px' }, t('connect.install.' + (c.install_transport === 'http' ? 'http' : 'stdio'))));
  const add = (c.add_commands || {}).claude;
  if (add) {
    parts.push(el('div', { class: 'eyebrow', style: 'margin:14px 0 6px' }, t('connect.add.claude')));
    parts.push(el('pre', { class: 'detail snippet' }, add));
    parts.push(el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(add)));
  }
  parts.push(el('p', { class: 'detail', style: 'margin:12px 0 0' }, t('connect.token.hint')));
  return parts;
}

// guidanceCard shows what the MCP server tells every agent at initialize
// (GET /agent/prompt?kind=instructions): the text, its token estimate, and
// the prompts an MCP client can list. It is the one place a person sees
// what their agent was told without being the agent.
function guidanceCard(g) {
  const names = (g.prompts || []).filter((n) => n !== 'instructions');
  return el('div', { class: 'guidance', 'data-guidance': '', style: 'margin-top:14px' },
    el('div', { class: 'eyebrow', style: 'margin:0 0 6px' }, t('connect.guidance.title', g.tokens || 0)),
    el('pre', { class: 'detail snippet', style: 'white-space:pre-wrap' }, g.prompt || ''),
    el('div', { class: 'row', style: 'margin-top:6px;gap:8px;flex-wrap:wrap' },
      copyBtn(g.prompt || ''),
      el('span', { class: 'dim' }, t('connect.guidance.prompts', names.join(', ')))));
}

// renderConnectPanel fills host and loads once; it returns a dispose that
// stops a late reply from landing on a screen that has moved on.
export function renderConnectPanel(host, { open = false } = {}) {
  let disposed = false;
  const inner = el('div', { class: 'dim' }, t('connect.loading'));
  const guidance = el('div', {});
  const state = el('span', {});
  const details = el('details', { class: 'connect', open },
    el('summary', {},
      el('span', { class: 'connect-title' }, t('connect.title')), state),
    el('div', { class: 'connect-body' }, inner, guidance));
  fill(host, details);

  async function load() {
    try {
      const c = await api.get('/mcp/connect');
      if (disposed) return;
      fill(state, stateLine(c));
      fill(inner, ...body(c));
      inner.className = '';
    } catch (e) {
      if (disposed) return;
      fill(state);
      fill(inner, el('span', { class: 'detail' }, e.message));
    }
    try {
      const g = await api.get('/agent/prompt?kind=instructions');
      if (disposed) return;
      fill(guidance, guidanceCard(g));
    } catch (e) {
      // A daemon without a filesystem has no guidance to show; the
      // connect state above still stands on its own.
      if (disposed) return;
      fill(guidance);
    }
  }
  load();
  return () => { disposed = true; };
}
