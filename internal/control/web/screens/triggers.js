import { api } from '/ui/api.js';
import { el, fill, toast, confirmDelete, openForm, moreRow } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onTriggerEvent } from '/ui/app.js';
import { selfTriggerRisk, argvCells, ruleSummary, deliveryState, EXEC_EXAMPLE, WEBHOOK_EXAMPLE, VERIFY_SNIPPET } from '/ui/trigger_view.js';
import { openDeliveryPanel } from '/ui/delivery_panel.js';

// The triggers screen: the rules the configuration file holds, read-only,
// and the deliveries the engine made from them.
//
// Rules are shown, never edited. The configuration file is the command
// whitelist (docs/agent-roadmap.md §5.4), so there is no form that spells a
// rule and no route that would take one; every card ends with "edit in the
// configuration file". An exec argv is rendered one <code> per element and
// never joined, so "{path}; rm -rf /" reads as the single argument it is
// rather than as a shell line. A webhook card shows the URL and whether a
// signing secret is configured; the daemon never sends the secret and the
// page never asks for it.
//
// Deliveries are a paged table with a rule and a state filter. A dead row
// has the one action a person can take on a delivery, retry. A row click
// opens the delivery panel; a live trigger event reloads the first page
// while the table is on it. "Test delivery" queues a rule on a path for
// real — the command runs, the webhook is called — so it goes through the
// typed confirmation sheet the way a deletion does.
//
// Everything in a row was written by the engine from a change the kernel,
// an agent or the remote made: paths, rule names, error text. All of it is
// inserted as text.
const COLUMNS = 8;
const PAGE = 50;

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || String(iso).startsWith('0001') ? '' : d.toLocaleString(locale());
}

// hashParams reads the query part of the hash, where a deep link keeps the
// delivery to open (#/triggers?delivery=<id>).
function hashParams() {
  return new URLSearchParams((location.hash.split('?')[1]) || '');
}

function chip(text) {
  return el('span', { class: 'chip', style: 'padding:2px 8px' }, text);
}

// chips renders a list as separate chips, or the "all" word for the empty
// list the daemon sends when a rule names no events or origins.
function chips(list, keyPrefix) {
  if (!list.length) return [chip(t('triggers.all'))];
  return list.map((v) => chip(t(keyPrefix + v)));
}

function field(label, ...value) {
  return el('div', { class: 'row', style: 'gap:10px;align-items:baseline;flex-wrap:wrap;font-size:13px' },
    el('span', { class: 'muted', style: 'min-width:64px' }, label), ...value);
}

// execAction is the argv, one <code> per element. The cells are separate
// inline blocks with a gap between them, not text with spaces in it, so
// nothing on the card can be copied as a command line.
function execAction(exec) {
  const e = exec || {};
  return [
    field(t('triggers.command'),
      el('span', { class: 'argv', style: 'display:inline-flex;flex-wrap:wrap;gap:6px' },
        ...argvCells(e.command).map((arg) => el('code', { class: 'argv-cell', style: 'padding:2px 6px;border:1px solid var(--border);border-radius:4px;background:#121a25;white-space:pre-wrap;overflow-wrap:anywhere' }, arg)))),
    e.cwd ? field(t('triggers.cwd'), el('code', {}, e.cwd)) : null,
    field(t('triggers.timeout'), e.timeout || ''),
  ];
}

function webhookAction(hook) {
  const webhook = hook || {};
  return [
    field(t('triggers.url'), el('code', { style: 'overflow-wrap:anywhere' }, webhook.url || '')),
    field(t('triggers.signing'), webhook.secret_configured
      ? el('span', {}, el('span', { class: 'dot ok', style: 'display:inline-block;margin-right:6px' }), t('triggers.secret_configured'))
      : el('span', { class: 'warn-text' }, t('triggers.secret_missing'))),
    field(t('triggers.timeout'), webhook.timeout || ''),
    webhook.proxy ? field(t('triggers.proxy'), webhook.proxy) : null,
    webhook.include_download_url ? field('', el('span', { class: 'warn-text' }, t('triggers.download_url'))) : null,
    webhook.insecure ? field('', el('span', { class: 'warn-text' }, t('triggers.insecure'))) : null,
  ];
}

// ruleCard is one rule as the file spelt it. The self-trigger mark is
// yellow and worded: an exec rule that still listens to api writes can be
// fired by the agent it runs.
function ruleCard(rule) {
  const s = ruleSummary(rule);
  const action = rule.action || {};
  return el('div', { class: 'card', 'data-rule': s.name, style: 'display:flex;flex-direction:column;gap:8px' },
    el('div', { class: 'row', style: 'align-items:center;gap:10px;flex-wrap:wrap' },
      el('strong', { style: 'font-size:15px' }, s.name),
      chip(t('triggers.action.' + (s.action || 'exec'))),
      selfTriggerRisk(rule)
        ? el('span', { class: 'warn-text', 'data-risk': 'self-trigger', style: 'font-size:12px' }, t('triggers.self_trigger'))
        : null),
    field(t('triggers.paths'), ...s.paths.map((p) => el('code', { style: 'overflow-wrap:anywhere' }, p))),
    field(t('triggers.events'), ...chips(s.events, 'triggers.event.')),
    field(t('triggers.origins'), ...chips(s.origins, 'triggers.origin.')),
    field(t('triggers.debounce'), s.debounce || '0s', s.onRescan ? el('span', { class: 'dim' }, t('triggers.on_rescan', s.onRescan)) : null),
    ...(s.action === 'webhook' ? webhookAction(action.webhook) : execAction(action.exec)),
    el('div', { class: 'dim', style: 'font-size:12px;margin-top:4px' }, t('triggers.edit_in_config')));
}

// renderEmpty is the whole screen when no rule is configured, or when this
// process does not run the engine: what to put in the file, and how a
// webhook receiver checks the signature. The three blocks are code, so they
// are text nodes in <pre>; every label around them is translated.
function renderEmpty(host) {
  const block = (label, code) => el('div', {},
    el('div', { class: 'eyebrow', style: 'margin:18px 0 8px' }, label),
    el('pre', { class: 'snippet', style: 'white-space:pre' }, code));
  fill(host,
    el('div', { class: 'pad', style: 'padding-bottom:12px' },
      el('div', { class: 'eyebrow' }, t('triggers.eyebrow')),
      el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('triggers.title'))),
    el('div', { style: 'padding:0 20px 20px;max-width:720px' },
      el('p', { class: 'detail' }, t('triggers.empty.body')),
      block(t('triggers.example.exec'), EXEC_EXAMPLE),
      block(t('triggers.example.webhook'), WEBHOOK_EXAMPLE),
      el('p', { class: 'detail', style: 'margin-top:18px' }, t('triggers.example.verify.body')),
      block(t('triggers.example.verify'), VERIFY_SNIPPET)));
}

export function renderTriggers(host) {
  const rows = el('tbody');
  const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(auto-fill,minmax(360px,1fr));gap:14px' });
  let rules = [];
  let paged = false;
  let disposed = false;
  let generation = 0;
  let reloadTimer = null;

  // actionOf tells the delivery panel which action a rule has, which a row
  // does not carry.
  function actionOf(name) {
    const r = rules.find((x) => x.name === name);
    return r && r.action ? r.action.type : '';
  }

  const ruleFilter = el('select', { 'aria-label': t('triggers.filter.rule'), style: 'width:auto' },
    el('option', { value: '' }, t('triggers.filter.rule')));
  const stateFilter = el('select', { 'aria-label': t('triggers.filter.state'), style: 'width:auto' },
    el('option', { value: '' }, t('triggers.filter.state')),
    ...['pending', 'running', 'done', 'dead'].map((v) => el('option', { value: v }, t('triggers.state.' + v))));

  function filters() {
    const q = new URLSearchParams();
    if (ruleFilter.value) q.set('rule', ruleFilter.value);
    if (stateFilter.value) q.set('state', stateFilter.value);
    return q;
  }

  function stateCell(d) {
    const st = deliveryState(d);
    return el('span', { style: 'display:inline-flex;align-items:center;gap:7px' },
      el('span', { class: 'dot ' + st.dot }), t(st.key));
  }

  function rowEl(d) {
    const dead = d.state === 'dead';
    const retryBtn = dead
      ? el('button', { style: 'padding:4px 10px', onclick: (e) => { e.stopPropagation(); retry(d, e.currentTarget); } }, t('triggers.retry'))
      : null;
    return el('tr', { 'data-delivery': String(d.id), 'data-state': d.state, style: 'cursor:pointer', onclick: () => openDeliveryPanel(d.id, actionOf) },
      el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(d.first_seen)),
      el('td', {}, d.rule),
      el('td', { class: 'detail', style: 'word-break:break-all' }, d.path || el('span', { class: 'dim' }, t('triggers.path.rescan'))),
      el('td', {}, d.kind ? t('triggers.event.' + d.kind) : ''),
      el('td', {}, d.origin ? t('triggers.origin.' + d.origin) : ''),
      el('td', { class: 'num tnums' }, String(d.attempts || 0)),
      el('td', {}, stateCell(d)),
      el('td', { style: 'white-space:nowrap' }, retryBtn));
  }

  // retry is the one state change a row allows, and only a dead row offers
  // it: the daemon answers 409 for any other state.
  async function retry(d, button) {
    button.disabled = true;
    try {
      await api.post('/triggers/retry', { id: d.id });
      toast(t('triggers.retried', String(d.id)));
      load();
    } catch (e) {
      button.disabled = false;
      toast(e.message, 'bad');
    }
  }

  function emptyRow() {
    return el('tr', { 'data-empty': 'true' }, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('triggers.deliveries.empty')));
  }

  function appendResponse(r) {
    rows.append(...(r.deliveries || []).map(rowEl));
    if (!rows.children.length) fill(rows, emptyRow());
    if (r.next_cursor) rows.append(moreRow(COLUMNS, () => load(r.next_cursor)));
  }

  async function load(cursor) {
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    const mine = ++generation;
    const q = filters();
    q.set('limit', String(PAGE));
    if (cursor) q.set('cursor', cursor);
    try {
      const r = await api.get('/triggers/deliveries?' + q.toString());
      if (disposed || mine !== generation) return;
      appendResponse(r);
    } catch (e) {
      if (disposed || mine !== generation) return;
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  // testDelivery: pick a rule and a path, type the rule's name, and only
  // then ask the daemon to queue it. The daemon demands confirm: true for
  // the same reason the sheet exists — the action runs for real.
  async function testDelivery() {
    const select = el('select', { style: 'width:100%' },
      ...rules.map((r) => el('option', { value: r.name }, r.name)));
    const path = el('input', { type: 'text', placeholder: '/work/inbox/a.txt', autocomplete: 'off', spellcheck: 'false' });
    const ok = await openForm({
      title: t('triggers.test.title'),
      rows: [[t('triggers.test.rule'), select], [t('triggers.test.path'), path]],
      note: el('p', { class: 'muted', style: 'font-size:12px;margin:0' }, t('triggers.test.note')),
      confirmLabel: t('triggers.test.next'),
      validate: () => (path.value.trim().startsWith('/') ? '' : t('triggers.test.needpath')),
    });
    if (!ok) return;
    const rule = select.value;
    const target = path.value.trim();
    const sure = await confirmDelete({
      title: t('triggers.test.confirm.title'),
      body: t('triggers.test.confirm.body', rule, target),
      confirmToken: rule,
      confirmLabel: t('triggers.test.run'),
    });
    if (!sure) return;
    try {
      const r = await api.post('/triggers/test', { name: rule, path: target, confirm: true });
      toast(t('triggers.test.queued', String(r.id)));
      load();
      openDeliveryPanel(r.id, actionOf);
    } catch (e) {
      toast(e.message, 'bad');
    }
  }

  function renderRules() {
    fill(cards, ...rules.map(ruleCard));
    fill(ruleFilter, el('option', { value: '' }, t('triggers.filter.rule')),
      ...rules.map((r) => el('option', { value: r.name }, r.name)));
  }

  function renderLoaded() {
    fill(host,
      el('div', { class: 'pad', style: 'padding-bottom:12px' },
        el('div', { class: 'row', style: 'justify-content:space-between;align-items:flex-end' },
          el('div', {},
            el('div', { class: 'eyebrow' }, t('triggers.eyebrow')),
            el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('triggers.title'))),
          el('button', { class: 'primary', onclick: testDelivery }, t('triggers.test.button')))),
      el('div', { style: 'padding:0 20px 20px' },
        el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('triggers.rules')),
        cards),
      el('div', { class: 'row', style: 'padding:0 20px 12px;flex-wrap:wrap;align-items:center' },
        el('div', { class: 'eyebrow' }, t('triggers.deliveries')),
        el('div', { class: 'grow' }),
        ruleFilter, stateFilter,
        el('button', { onclick: () => load() }, t('action.refresh'))),
      el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
        el('table', {}, el('thead', {}, el('tr', {},
          el('th', {}, t('triggers.col.time')), el('th', {}, t('triggers.col.rule')),
          el('th', {}, t('triggers.col.path')), el('th', {}, t('triggers.col.event')),
          el('th', {}, t('triggers.col.origin')), el('th', { class: 'num' }, t('triggers.col.attempts')),
          el('th', {}, t('triggers.col.state')), el('th', {}, t('triggers.col.action')))), rows))),
      el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('triggers.note')));
    renderRules();
    ruleFilter.addEventListener('change', () => load());
    stateFilter.addEventListener('change', () => load());
  }

  async function start() {
    fill(host, el('div', { class: 'pad dim' }, t('triggers.loading')));
    let r;
    try {
      r = await api.get('/triggers');
    } catch (e) {
      if (!disposed) fill(host, el('div', { class: 'pad', role: 'alert', style: 'color:var(--danger-text)' }, e.message));
      return;
    }
    if (disposed) return;
    rules = r.rules || [];
    if (!r.enabled || !rules.length) {
      renderEmpty(host);
      return;
    }
    renderLoaded();
    load();
    // A deep link (#/triggers?delivery=<id>) opens that delivery straight
    // away; the send-to-agent panel points here after a run.
    const wanted = hashParams().get('delivery');
    if (wanted) openDeliveryPanel(wanted, actionOf);
  }

  start();
  // A live event reloads the first page while the table is on it; several
  // arrive for one delivery (pending, running, done), so the reload is
  // coalesced rather than issued per frame.
  const off = onTriggerEvent(() => {
    if (paged || document.hidden || !rules.length) return;
    if (reloadTimer) return;
    reloadTimer = setTimeout(() => { reloadTimer = null; if (!disposed) load(); }, 300);
  });
  return () => { disposed = true; off(); if (reloadTimer) clearTimeout(reloadTimer); };
}
