import { api } from '/ui/api.js';
import { el, fill, copyBtn, confirmDelete, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { stdioWarning, bridgeBanner } from '/ui/connect_view.js';
import { clientRow } from '/ui/hooks_view.js';

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
function stateLine(c) {
  const dot = c.http_listening ? 'ok' : 'warn';
  return el('span', { style: 'display:inline-flex;align-items:center;gap:8px' },
    el('span', { class: 'dot ' + dot }),
    c.http_listening ? t('connect.http.on', c.http_addr || '') : t('connect.http.off'));
}

async function enableHTTP(button) {
  const ok = await confirmDelete({
    title: t('integration.http.title'), body: t('integration.http.body'),
    confirmToken: 'restart', confirmLabel: t('integration.http.enable'), danger: false,
  });
  if (!ok) return;
  button.disabled = true;
  try {
    await api.post('/agent/integration/enable-http', {});
    await api.post('/daemon/restart?confirm=true', {});
    toast(t('integration.http.restarting'));
    setTimeout(() => location.reload(), 2500);
  } catch (e) {
    toast(e.message, 'bad');
    button.disabled = false;
  }
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
  // The bridge line answers the warning: green when that stdio server's
  // writes reach the owner, yellow with the reason when they do not.
  const bridge = bridgeBanner(c);
  if (bridge) {
    parts.push(el('div', { class: 'banner ' + bridge.cls, role: 'status', 'data-bridge-banner': bridge.cls, style: 'margin-top:8px' },
      bridge.reasonKey ? t(bridge.key) + ' ' + t(bridge.reasonKey) : t(bridge.key, bridge.arg)));
  }
  if (!c.http_listening) {
    parts.push(el('p', { class: 'detail', style: 'margin:12px 0 6px' }, t('connect.http.hint')));
    const enable = el('button', { class: 'primary' }, t('integration.http.enable'));
    enable.onclick = () => enableHTTP(enable);
    parts.push(el('div', { class: 'row', style: 'margin-top:6px' }, enable));
    return parts;
  }
  parts.push(el('p', { class: 'detail', style: 'margin:12px 0 0' }, t('connect.integration.hint')));
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

async function changeIntegration(action, selected, button, reload) {
  const clients = selected().filter((c) => c.checked).map((c) => c.value);
  if (!clients.length) { toast(t('integration.select'), 'bad'); return; }
  if (action === 'uninstall') {
    const ok = await confirmDelete({
      title: t('integration.uninstall.title'), body: t('integration.uninstall.body', clients.join(', ')),
      confirmToken: 'uninstall', confirmLabel: t('integration.uninstall'), danger: true,
    });
    if (!ok) return;
  }
  button.disabled = true;
  try {
    await api.post('/agent/integration/' + action, { clients, confirm: action === 'uninstall' });
    toast(t('integration.' + action + '.done', clients.join(', ')));
    await reload();
  } catch (e) {
    toast(e.message, 'bad');
    button.disabled = false;
  }
}

// hooksCard owns the full local integration workflow: selection, install,
// status refresh and confirmed uninstall all happen without leaving the UI.
function hooksCard(h, reload) {
  const rows = (h.clients || []).map(clientRow);
  const checks = [];
  const integrationRows = (h.integration || []).map((c) => {
    const check = el('input', { type: 'checkbox', value: c.client });
    check.checked = c.present || c.skill_installed || c.mcp_configured || c.hooks_installed;
    checks.push(check);
    const parts = [
      t(c.skill_installed ? 'integration.skill.ready' : 'integration.skill.missing'),
      t(c.mcp_configured ? 'integration.mcp.ready' : 'integration.mcp.missing'),
      t(c.hooks_installed ? 'integration.hooks.ready' : 'integration.hooks.missing'),
      t('integration.connection.short', c.mcp_connection || 'unavailable'),
      t('integration.auto.short', c.auto_injection || 'unverified'),
    ];
    return el('tr', { 'data-integration-client': c.client },
      el('td', {}, check), el('td', {}, c.client),
      el('td', { class: 'dim' }, c.client_version || t(c.present ? 'integration.detected' : 'integration.notfound')),
      el('td', {}, parts.join(' · ')),
      el('td', { class: c.problem ? 'detail' : 'dim' }, c.problem || c.memory || ''));
  });
  const install = el('button', { class: 'primary' }, t('integration.install'));
  const uninstall = el('button', { class: 'danger' }, t('integration.uninstall'));
  const refresh = el('button', {}, t('integration.refresh'));
  install.onclick = () => changeIntegration('install', () => checks, install, reload);
  uninstall.onclick = () => changeIntegration('uninstall', () => checks, uninstall, reload);
  refresh.onclick = async () => { refresh.disabled = true; await reload(); };
  return el('div', { class: 'guidance', 'data-hooks': '', style: 'margin-top:14px' },
    el('div', { class: 'eyebrow', style: 'margin:0 0 6px' }, t('hooks.title', t('hooks.context.' + (h.context || 'minimal')))),
    el('div', { 'data-agent-integration': '' },
      el('p', {}, t('integration.title')),
      el('table', { style: 'font-size:13px' },
        el('thead', {}, el('tr', {}, el('th', {}), el('th', {}, t('hooks.col.client')), el('th', {}, t('integration.version')), el('th', {}, t('integration.state')), el('th', {}, t('integration.detail')))),
        el('tbody', {}, integrationRows)),
      el('div', { class: 'row', style: 'margin-top:8px;gap:8px' }, install, refresh, uninstall),
      el('p', { class: 'dim' }, t('integration.connection', h.mcp_connection || 'unverified')),
      el('p', { class: 'dim' }, t('integration.directory', h.directory || 'unavailable')),
      el('p', { class: 'dim' }, t('integration.memory', h.memory || 'unavailable'))),

    el('table', { style: 'font-size:13px' },
      el('thead', {}, el('tr', {}, el('th', {}, t('hooks.col.client')), el('th', {}, t('hooks.col.state')), el('th', {}, t('hooks.col.verified')), el('th', {}, t('hooks.col.path')))),
      el('tbody', {}, rows.map((r) => el('tr', { 'data-hook-client': r.client, 'data-hook-state': r.state },
        el('td', {}, r.client),
        el('td', {}, el('span', { style: 'display:inline-flex;align-items:center;gap:7px;white-space:nowrap' },
          el('span', { class: 'dot ' + (r.state === 'installed' ? 'ok' : r.state === 'absent' ? 'warn' : '') }), t(r.stateKey))),
        el('td', { class: r.verified ? '' : 'dim' }, t(r.verifiedKey)),
        el('td', { class: 'dim', style: 'word-break:break-all' }, r.path, r.note ? el('div', { style: 'font-size:11px' }, r.note) : null))))),
    el('div', { class: 'dim', style: 'margin:8px 0 6px;font-size:12px' }, t('hooks.registry', h.mounts_registry || '', h.mounts || 0)),
    el('p', { class: 'dim', style: 'margin:8px 0 0;font-size:12px' }, t('hooks.note')));
}

// renderConnectPanel fills host and loads once; it returns a dispose that
// stops a late reply from landing on a screen that has moved on.
export function renderConnectPanel(host, { open = false } = {}) {
  let disposed = false;
  const inner = el('div', { class: 'dim' }, t('connect.loading'));
  const guidance = el('div', {});
  const hooks = el('div', {});
  const state = el('span', {});
  const details = el('details', { class: 'connect', open },
    el('summary', {},
      el('span', { class: 'connect-title' }, t('connect.title')), state),
    el('div', { class: 'connect-body' }, inner, guidance, hooks));
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
    try {
      const h = await api.get('/agent/hooks');
      if (disposed) return;
      fill(hooks, hooksCard(h, load));
    } catch (e) {
      if (disposed) return;
      fill(hooks);
    }
  }
  load();
  return () => { disposed = true; };
}
