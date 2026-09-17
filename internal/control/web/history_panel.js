import { api } from '/ui/api.js';
import { el, fill, openPanel, moreRow, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { originCell, actorText, when } from '/ui/provenance.js';
import { reversibility } from '/ui/rollback_plan.js';

// The history overlay (ui-plan G5-2): every recorded change to a path,
// newest first, paged by GET /changes?path=&cursor=. The inspector has no
// tab bar, so history opens as a panel from its "History" button. Each row
// is time, kind, origin, actor (a session opens its detail), and whether
// the change could be undone — which a change row cannot know on its own,
// so the column says "via session" for a change a session made and
// nothing otherwise. A row with reliable false follows a feed overflow and
// carries the "may be incomplete" tag.

const COLUMNS = 5;

function historyRow(c, openSession) {
  const actor = actorText(c);
  return el('tr', { 'data-change': String(c.id), 'data-reliable': c.reliable === false ? '0' : '1' },
    el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(c.ts)),
    el('td', { style: 'white-space:nowrap' }, t('changes.kind.' + c.kind), c.from ? el('span', { class: 'dim' }, ' ', c.from, ' →') : null,
      c.reliable === false ? el('span', { class: 'dim' }, ' · ', t('changes.unreliable')) : null),
    el('td', {}, originCell(c.origin)),
    el('td', {}, c.session_id
      ? el('button', { style: 'font-size:12px', onclick: () => { openSession(c.session_id); } }, actor)
      : el('span', { class: 'dim' }, actor)),
    el('td', { class: 'dim' }, c.session_id ? t('changes.via_session') : ''));
}

// openHistoryPanel opens the overlay for path.
export function openHistoryPanel(path, { openSession }) {
  const rows = el('tbody', {});
  const table = el('div', { class: 'panel', style: 'overflow:auto;max-height:60vh' },
    el('table', {},
      el('thead', {}, el('tr', {},
        el('th', {}, t('audit.col.time')), el('th', {}, t('changes.col.kind')), el('th', {}, t('changes.col.origin')),
        el('th', {}, t('changes.col.actor')), el('th', {}, t('changes.col.reversible')))),
      rows));
  let generation = 0;
  const close = openPanel({ title: t('history.title', path), width: 760,
    content: el('div', {}, el('p', { class: 'dim', style: 'margin:0 0 10px;font-size:12px' }, t('history.note')), table) });
  const openAndClose = (id) => { close(); openSession(id); };
  async function load(cursor) {
    cursor = pageCursor(cursor);
    if (!cursor) fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('history.loading'))));
    const mine = ++generation;
    let r;
    try {
      r = await api.get('/changes?path=' + encodeURIComponent(path) + '&limit=50' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
    } catch (err) {
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, err.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed); else fill(rows, failed);
      toast(err.message, 'bad');
      return;
    }
    if (mine !== generation) return;
    if (!cursor) fill(rows);
    const list = r.changes || [];
    for (const c of list) rows.append(historyRow(c, openAndClose));
    if (!rows.children.length) rows.append(el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, r.enabled ? t('history.empty') : t('changes.disabled'))));
    if (r.next_cursor) rows.append(moreRow(COLUMNS, () => load(r.next_cursor)));
  }
  load('');
  return close;
}

// The change kinds a row can carry, so the Go test can hold the catalog
// to them: write, create, mkdir, remove, rename, remote, rescan.
export const KINDS = ['write', 'create', 'mkdir', 'remove', 'rename', 'remote', 'rescan'];
