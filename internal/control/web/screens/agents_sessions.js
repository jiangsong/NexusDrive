import { api } from '/ui/api.js';
import { el, fill, moreRow } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { scopeParts } from '/ui/scope_view.js';
import { openSessionPanel } from '/ui/session_panel.js';
import { onAgentEvent } from '/ui/app.js';

// The sessions tab: three headline counts and the list of MCP sessions,
// newest first. Every row comes from GET /sessions and is held by this
// module alone — the shared store never sees a session, because nothing
// else on the page wants one and the list can be long.
const COLUMNS = 6;

// scopeSummary is the one-line reading of a scope, translated part by part.
export function scopeSummary(scope) {
  return scopeParts(scope).map((p) => t(p.key, ...p.args)).join(' · ');
}

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

export function renderSessionsTab(host, params) {
  const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:14px;padding:0 20px' });
  const rows = el('tbody');
  // A reader who paged deeper keeps that view until they refresh; a session
  // event only reloads the first page when the first page is all there is.
  let paged = false;
  let disposed = false;
  // Two first-page loads can overlap — a finish reloads, and the session
  // event it raises reloads again — and each would append its rows to a
  // table the other has already filled. Only the newest load lands.
  let generation = 0;
  // The sandbox filter is a view choice, kept for the life of the tab:
  // every reload, including the ones session events trigger, honours it.
  let sandboxOnly = false;

  function card(label, value) {
    return el('div', { class: 'panel pad' },
      el('div', { class: 'muted', style: 'font-size:13px;margin-bottom:9px' }, label),
      el('div', { class: 'tnums', style: 'font-size:28px;font-weight:720;letter-spacing:-.03em' }, value));
  }

  function refreshCards(summary) {
    const s = summary || {};
    fill(cards,
      card(t('agents.card.active'), String(s.active || 0)),
      card(t('agents.card.writes'), String(s.writes_today || 0)),
      card(t('agents.card.denied'), String(s.denied_today || 0)));
  }

  function stateCell(s) {
    const dot = s.state === 'active' ? 'ok' : s.state === 'expired' ? 'warn' : '';
    return el('span', { style: 'display:inline-flex;align-items:center;gap:7px' },
      el('span', { class: 'dot ' + dot }), t('session.state.' + s.state));
  }

  function sessionRow(s) {
    return el('tr', { 'data-session': s.id, style: 'cursor:pointer', onclick: () => open(s.id) },
      el('td', {},
        el('div', {}, s.client || s.id),
        el('div', { class: 'dim', style: 'font-size:12px' }, (s.transport || '') + (s.client_version ? ' · ' + s.client_version : ''))),
      el('td', { class: 'detail' }, scopeSummary(s.scope)),
      el('td', {}, stateCell(s)),
      el('td', { class: 'detail tnums' }, when(s.started_at)),
      el('td', { class: 'num' }, String(s.writes || 0)),
      el('td', { class: 'num' }, String(s.artifacts || 0)));
  }

  function appendResponse(r) {
    rows.append(...(r.sessions || []).map(sessionRow));
    if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('session.empty'))));
    if (r.next_cursor) rows.append(moreRow(COLUMNS, () => load(r.next_cursor)));
    if (r.summary) refreshCards(r.summary);
  }

  async function load(cursor) {
    // A cursor is a string the daemon issued; a click event is not one.
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    const mine = ++generation;
    try {
      const r = await api.get('/sessions?limit=50' + (sandboxOnly ? '&sandbox=1' : '')
        + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      if (disposed || mine !== generation) return;
      appendResponse(r);
    } catch (e) {
      if (disposed || mine !== generation) return;
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  function open(id) {
    openSessionPanel(id, () => load());
  }

  fill(host,
    cards,
    el('div', { class: 'pad', style: 'display:flex;align-items:center;gap:14px;padding-bottom:12px' },
      el('label', { style: 'display:inline-flex;align-items:center;gap:7px;font-size:13px' },
        el('input', { type: 'checkbox', onchange: (ev) => { sandboxOnly = ev.target.checked; load(); } }),
        t('session.filter.sandbox')),
      el('div', { class: 'grow' }),
      el('button', { onclick: () => load() }, t('action.refresh'))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('session.col.client')), el('th', {}, t('session.col.scope')),
        el('th', {}, t('session.col.state')), el('th', {}, t('session.col.started')),
        el('th', { class: 'num' }, t('session.col.writes')),
        el('th', { class: 'num' }, t('session.col.artifacts')))), rows))));

  refreshCards(null);
  load();
  // A deep link (#/agents?session=<id>) opens that session straight away;
  // the browser smoke test lands on one this way.
  const wanted = params && params.get('session');
  if (wanted) open(wanted);

  const off = onAgentEvent((ev) => {
    if (ev.kind !== 'session' || paged || document.hidden) return;
    load();
  });
  return () => { disposed = true; off(); };
}
