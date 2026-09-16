import { api } from '/ui/api.js';
import { el, fill, bytes, moreRow, showPanel } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onAgentEvent } from '/ui/app.js';

// The audit tab: every tool call an agent made, newest first, with a filter
// bar over session, tool, result and time. Rows are held by this module and
// nowhere else — they are large, untrusted and shared with nothing.
//
// Everything in a row was written by the agent: tool name, paths, the
// redacted argument object, error text. All of it is inserted as text; the
// arguments open as formatted JSON in a read-only <pre>.
const COLUMNS = 7;
const SINCE_MS = { '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 };

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// pathCell folds several paths into the first one plus a count, so a move
// or a copy with two ends stays on one line.
function pathCell(paths) {
  const list = paths || [];
  if (!list.length) return '';
  return list.length > 1 ? list[0] + ' ' + t('audit.paths.more', list.length - 1) : list[0];
}

export function renderAuditTab(host) {
  const rows = el('tbody');
  let paged = false;
  let disposed = false;
  // Two loads can overlap — a filter change while the previous page is
  // still in flight — and each would append its rows to a table the other
  // has already filled. Only the newest load lands.
  let generation = 0;

  const session = el('input', { type: 'text', placeholder: t('audit.filter.session'), 'aria-label': t('audit.filter.session'), autocomplete: 'off', spellcheck: 'false', style: 'width:220px' });
  const tool = el('input', { type: 'text', placeholder: t('audit.filter.tool'), 'aria-label': t('audit.filter.tool'), autocomplete: 'off', spellcheck: 'false', style: 'width:160px' });
  const result = el('select', { 'aria-label': t('audit.filter.result'), style: 'width:auto' },
    el('option', { value: '' }, t('audit.filter.result')),
    ...['ok', 'denied', 'error', 'forwarded', 'oversize'].map((v) => el('option', { value: v }, t('audit.result.' + v))));
  const since = el('select', { 'aria-label': t('audit.filter.since'), style: 'width:auto' },
    el('option', { value: '' }, t('audit.filter.since')),
    ...Object.keys(SINCE_MS).map((v) => el('option', { value: v }, t('audit.since.' + v))));

  function filters() {
    const q = new URLSearchParams();
    if (session.value.trim()) q.set('session', session.value.trim());
    if (tool.value.trim()) q.set('tool', tool.value.trim());
    if (result.value) q.set('result', result.value);
    if (since.value) q.set('since', new Date(Date.now() - SINCE_MS[since.value]).toISOString());
    return q;
  }

  // filtered says whether the table is a slice rather than the whole trail;
  // a live row only belongs at the top of the whole trail.
  function filtered() {
    return !!(session.value.trim() || tool.value.trim() || result.value || since.value);
  }

  function showArgs(row) {
    showPanel({
      title: t('audit.args'),
      content: el('div', {},
        el('div', { class: 'detail', style: 'margin-bottom:8px' }, row.tool + (row.session_id ? ' · ' + row.session_id : '')),
        row.error ? el('div', { style: 'color:var(--danger-text);margin-bottom:8px;word-break:break-all' }, row.error) : null,
        el('pre', { class: 'detail', style: 'margin:0;max-height:55vh;overflow:auto;white-space:pre-wrap;word-break:break-all;font-size:12px' },
          JSON.stringify(row.args, null, 2))),
      closeLabel: t('conn.close'),
      width: 720,
    });
  }

  function rowEl(row) {
    return el('tr', { class: row.result === 'denied' ? 'denied' : '', 'data-result': row.result, 'data-audit': String(row.id), style: 'cursor:pointer', onclick: () => showArgs(row) },
      el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(row.ts)),
      el('td', {}, row.client || '-'),
      el('td', {}, row.tool),
      el('td', { class: 'detail', style: 'word-break:break-all' }, pathCell(row.paths)),
      el('td', {}, t('audit.result.' + row.result)),
      el('td', { class: 'num' }, bytes((row.bytes_in || 0) + (row.bytes_out || 0))),
      el('td', { class: 'num dim' }, (row.duration_ms || 0) + ' ms'));
  }

  function emptyRow() {
    return el('tr', { 'data-empty': 'true' }, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('audit.empty')));
  }

  function appendResponse(r) {
    rows.append(...(r.rows || []).map(rowEl));
    if (!rows.children.length) fill(rows, emptyRow());
    if (r.next_cursor) rows.append(moreRow(COLUMNS, () => load(r.next_cursor)));
  }

  async function load(cursor) {
    // A cursor is a string the daemon issued; a click event is not one.
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    const mine = ++generation;
    const q = filters();
    q.set('limit', '100');
    if (cursor) q.set('cursor', cursor);
    try {
      const r = await api.get('/audit?' + q.toString());
      if (disposed || mine !== generation) return;
      appendResponse(r);
    } catch (e) {
      if (disposed || mine !== generation) return;
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  for (const input of [session, tool]) {
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); load(); } });
    input.addEventListener('change', () => load());
  }
  result.addEventListener('change', () => load());
  since.addEventListener('change', () => load());

  fill(host,
    el('div', { class: 'row', style: 'padding:0 20px 12px;flex-wrap:wrap;align-items:center' },
      session, tool, result, since,
      el('div', { class: 'grow' }),
      el('button', { onclick: () => load() }, t('action.refresh'))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('audit.col.time')), el('th', {}, t('session.col.client')),
        el('th', {}, t('audit.filter.tool')), el('th', {}, t('audit.col.path')),
        el('th', {}, t('audit.filter.result')), el('th', { class: 'num' }, t('audit.col.bytes')),
        el('th', { class: 'num' }, t('audit.col.duration')))), rows))),
    el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('audit.note')));

  load();
  // A live row goes to the top only while the table is the unfiltered first
  // page; otherwise it would land in a slice it does not belong to.
  const off = onAgentEvent((ev) => {
    if (ev.kind !== 'audit' || paged || filtered() || document.hidden) return;
    const empty = rows.querySelector('tr[data-empty]');
    if (empty) empty.remove();
    rows.prepend(rowEl(ev.data));
  });
  return () => { disposed = true; off(); };
}
