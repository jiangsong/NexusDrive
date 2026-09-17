import { api } from '/ui/api.js';
import { el, fill, moreRow } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onFsChange } from '/ui/app.js';
import { originCell, actorText, when } from '/ui/provenance.js';
import { openSessionPanel } from '/ui/session_panel.js';

// The changes tab (ui-plan G5-3): the change record of the whole mount,
// newest first — every write that reached the VFS, from the terminal, an
// agent, the console or WebDAV — with a prefix filter. The audit tab is
// what agents asked for; this is what happened to the files, whoever did
// it. Rows come from GET /changes?path=&cursor= and are held here only.
//
// Paths, origins and actors are daemon data and go in as text. A row with
// reliable false follows a feed overflow and carries the "may be
// incomplete" tag; a session actor opens the session's detail.
const COLUMNS = 5;

export function renderChangesTab(host) {
  const rows = el('tbody');
  let paged = false;
  let disposed = false;
  let generation = 0;
  let timer = 0;

  const prefix = el('input', { type: 'text', placeholder: t('changes.filter.prefix'), 'aria-label': t('changes.filter.prefix'), autocomplete: 'off', spellcheck: 'false', style: 'width:280px' });

  function filtered() { return !!prefix.value.trim(); }

  function rowEl(c) {
    const actor = actorText(c);
    return el('tr', { 'data-change': String(c.id), 'data-reliable': c.reliable === false ? '0' : '1' },
      el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(c.ts)),
      el('td', { style: 'white-space:nowrap' }, t('changes.kind.' + c.kind),
        c.reliable === false ? el('span', { class: 'dim' }, ' · ', t('changes.unreliable')) : null),
      el('td', { class: 'detail', style: 'word-break:break-all' }, c.from ? [c.from, ' → ', c.path] : c.path),
      el('td', {}, originCell(c.origin)),
      el('td', {}, c.session_id
        ? el('button', { style: 'font-size:12px', 'data-session': c.session_id, onclick: () => openSessionPanel(c.session_id) }, actor)
        : el('span', { class: 'dim' }, actor)));
  }

  function emptyRow(enabled) {
    return el('tr', { 'data-empty': 'true' }, el('td', { colspan: String(COLUMNS), class: 'dim' }, enabled ? t('changes.empty') : t('changes.disabled')));
  }

  async function load(cursor) {
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    const mine = ++generation;
    const q = new URLSearchParams();
    if (filtered()) q.set('path', prefix.value.trim());
    q.set('limit', '100');
    if (cursor) q.set('cursor', cursor);
    try {
      const r = await api.get('/changes?' + q.toString());
      if (disposed || mine !== generation) return;
      rows.append(...(r.changes || []).map(rowEl));
      if (!rows.children.length) fill(rows, emptyRow(r.enabled));
      if (r.next_cursor) rows.append(moreRow(COLUMNS, () => load(r.next_cursor)));
    } catch (e) {
      if (disposed || mine !== generation) return;
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  prefix.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); load(); } });
  prefix.addEventListener('change', () => load());

  fill(host,
    el('div', { class: 'row', style: 'padding:0 20px 12px;flex-wrap:wrap;align-items:center' },
      prefix,
      el('div', { class: 'grow' }),
      el('button', { onclick: () => load() }, t('action.refresh'))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('audit.col.time')), el('th', {}, t('changes.col.kind')),
        el('th', {}, t('audit.col.path')), el('th', {}, t('changes.col.origin')),
        el('th', {}, t('changes.col.actor')))), rows))),
    el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('changes.note')));

  load();
  // The SSE change event says only that paths changed; the row itself (its
  // origin, its actor) is in the record, so the first page is re-read —
  // debounced, and only while the table is the unfiltered first page.
  const off = onFsChange(() => {
    if (paged || filtered() || document.hidden) return;
    clearTimeout(timer);
    timer = setTimeout(() => { if (!disposed) load(); }, 800);
  });
  return () => { disposed = true; off(); clearTimeout(timer); };
}
