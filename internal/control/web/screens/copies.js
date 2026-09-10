import { api, ApiError } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, openForm, moreRow } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';

// Server-side copy: the remote duplicates the object, the bytes never come
// through this machine. A copy is a durable journal row, so one that stalls
// stays visible until someone retries, cancels or forgets it — which is the
// whole reason this screen exists rather than a fire-and-forget button.
//
// Forgetting drops the record without undoing anything the remote already
// did, so it takes a typed confirmation and sends confirm: true, which the
// daemon requires for that action and refuses for the others.
// One table for the six journal states, built once rather than per row.
const STATE_COLOR = {
  preparing: 'var(--dim)', ready: 'var(--warn-text)', submitted: 'var(--ok)',
  failed: 'var(--bad)', cancelled: 'var(--warn)', purging: 'var(--dim)',
};

export function renderCopies(host) {
  const rows = el('tbody');

  async function load(cursor) {
    // Every refresh path calls this, and one of them is an event handler.
    // A cursor is a string the daemon issued; anything else means "the
    // first page", never "encode this object into &cursor=".
    cursor = pageCursor(cursor);
    if (!cursor) fill(rows);
    try {
      const r = await api.get('/copies?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      rows.append(...(r.copies || []).map(copyRow));
      if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('copies.empty'))));
      if (r.next_cursor) rows.append(moreRow(5, () => load(r.next_cursor)));
    } catch (e) {
      // A failed continuation must not take the pages already on screen with
      // it: the rows being read cost nothing to keep.
      const failed = el('tr', {}, el('td', { colspan: '5' }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  function copyRow(c) {
    const color = STATE_COLOR[c.state] || 'var(--dim)';
    const done = c.size > 0 ? Math.min(1, (c.checkpoint || 0) / c.size) : 0;
    const actions = el('div', { class: 'row' });
    if (c.state === 'failed' || c.state === 'ready') actions.append(btn(t('action.retry'), () => act('retry', c)));
    if (c.state === 'preparing' || c.state === 'ready' || c.state === 'submitted') actions.append(btn(t('action.stop'), () => act('cancel', c)));
    if (c.state === 'failed' || c.state === 'cancelled') actions.append(btn(t('copies.forget'), () => forget(c), true));
    return el('tr', {},
      el('td', {}, el('div', {},
        el('div', {}, c.source),
        el('div', { class: 'dim', style: 'font-size:12px' }, '→ ' + c.target),
        c.warning ? el('div', { style: 'font-size:12px;color:var(--warn-text)' }, c.warning) : null)),
      el('td', { class: 'num detail' }, bytes(c.size)),
      el('td', { style: 'padding-left:20px;min-width:120px' },
        el('div', { class: 'progress' }, el('span', { style: `width:${Math.round(done * 100)}%` })),
        el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:4px' }, bytes(c.checkpoint || 0))),
      el('td', { style: 'padding-left:20px' },
        el('span', { style: 'display:flex;align-items:center;gap:7px;color:' + color },
          el('span', { class: 'dot', style: 'background:' + color }), t('copy.state.' + c.state))),
      el('td', { style: 'padding-left:20px' }, actions));
  }
  const btn = (label, fn, danger) => el('button', { class: danger ? 'danger' : '', onclick: fn, style: 'padding:6px 12px' }, label);

  async function act(action, c) {
    try {
      await api.post('/copies/' + action, { id: c.id });
      toast(action === 'retry' ? t('toast.retried') : t('toast.stopped'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  async function forget(c) {
    const ok = await confirmDelete({
      title: t('copies.forget.title'), body: t('copies.forget.body'),
      confirmToken: c.id, confirmLabel: t('copies.forget'),
    });
    if (!ok) return;
    try {
      await api.post('/copies/forget', { id: c.id, confirm: true });
      toast(t('copies.forgotten'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  async function startCopy() {
    const from = el('input', { type: 'text', placeholder: '/mnt/prefix/file', autocomplete: 'off', spellcheck: 'false' });
    const to = el('input', { type: 'text', placeholder: '/mnt/prefix/copy', autocomplete: 'off', spellcheck: 'false' });
    const ok = await openForm({
      title: t('copies.new'),
      rows: [[t('col.source'), from], [t('copies.target'), to]],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('copies.new.note')),
      confirmLabel: t('copies.start'),
    });
    if (!ok) return;
    const q = { from: from.value.trim(), to: to.value.trim() };
    if (!q.from || !q.to) { toast(t('copies.new.needpaths'), 'bad'); return; }
    try {
      await api.post('/copy', q);
      toast(t('copies.started'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between;gap:12px' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('copies.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('copies.title'))),
      el('div', { class: 'row', style: 'gap:9px' },
        el('button', { onclick: () => load() }, t('action.refresh')),
        el('button', { class: 'primary', onclick: startCopy }, t('copies.new')))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('col.path')), el('th', { class: 'num' }, t('col.size')),
        el('th', { style: 'padding-left:20px' }, t('copies.progress')),
        el('th', { style: 'padding-left:20px' }, t('col.status')),
        el('th', { style: 'padding-left:20px' }, t('col.actions')))), rows))),
    el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('copies.note')));

  load();
  return () => {};
}
