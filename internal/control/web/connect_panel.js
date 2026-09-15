import { api } from '/ui/api.js';
import { el, fill, copyBtn } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

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
  if (c.stdio_non_owner) {
    parts.push(el('div', { class: 'banner warn', role: 'alert', style: 'margin-top:12px' },
      t('connect.stdio.banner'), ' ',
      el('a', { href: '#/diagnostics' }, t('connect.stdio.link'))));
  }
  if (!c.http_listening) {
    parts.push(el('p', { class: 'detail', style: 'margin:12px 0 6px' }, t('connect.http.hint')));
    parts.push(el('pre', { class: 'detail snippet' }, CONFIG_HINT));
    parts.push(el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(CONFIG_HINT)));
    return parts;
  }
  const add = (c.add_commands || {}).claude;
  if (add) {
    parts.push(el('div', { class: 'eyebrow', style: 'margin:14px 0 6px' }, t('connect.add.claude')));
    parts.push(el('pre', { class: 'detail snippet' }, add));
    parts.push(el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(add)));
  }
  parts.push(el('p', { class: 'detail', style: 'margin:12px 0 0' }, t('connect.token.hint')));
  return parts;
}

// renderConnectPanel fills host and loads once; it returns a dispose that
// stops a late reply from landing on a screen that has moved on.
export function renderConnectPanel(host, { open = false } = {}) {
  let disposed = false;
  const inner = el('div', { class: 'dim' }, t('connect.loading'));
  const state = el('span', {});
  const details = el('details', { class: 'connect', open },
    el('summary', {},
      el('span', { class: 'connect-title' }, t('connect.title')), state),
    el('div', { class: 'connect-body' }, inner));
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
  }
  load();
  return () => { disposed = true; };
}
