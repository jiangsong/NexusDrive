import { api } from '/ui/api.js';
import { el, fill, iconEl, bytes, toast, confirmDelete, moreRow } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { subscribe, get } from '/ui/store.js';

// The transfer queue. A file is committed the moment its journal row lands;
// this is the trip to the remote. Closing the page does not stop it.
export function renderTransfers(host) {
  const rows = el('tbody');
  // True once the reader has asked for more than the first page.
  let paged = false;
  const summary = el('div', { style: 'display:flex;gap:26px;padding:20px' });

  // The queue is paged like every other listing: the journal can hold far
  // more than a hundred rows after a long offline stretch, and the ones past
  // the first page are exactly the ones a person is looking for.
  async function load(cursor) {
    // Every refresh path calls this, and one of them is an event handler.
    // A cursor is a string the daemon issued; anything else means "the
    // first page", never "encode this object into &cursor=".
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    try {
      const r = await api.get('/uploads?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      rows.append(...(r.uploads || []).map(uploadRow));
      if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('transfers.empty'))));
      if (r.next_cursor) rows.append(moreRow(5, () => load(r.next_cursor)));
    } catch (e) {
      // A failed continuation must not take the pages already on screen with
      // it: the rows being read cost nothing to keep.
      const failed = el('tr', {}, el('td', { colspan: '5' }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
    const s = get().status;
    if (s && s.uploads) {
      const u = s.uploads;
      fill(summary,
        stat(t('transfers.queued'), bytes(u.queued_bytes), t('transfers.tasks', (u.pending || 0) + (u.uploading || 0))),
        stat(t('transfers.retained'), bytes(u.retained_bytes), t('transfers.retained.note'), 'var(--danger-text)'),
        stat(t('transfers.completed'), String(u.done || 0), t('transfers.keep')));
    }
  }
  function stat(label, value, sub, color) {
    return el('div', {}, el('div', { class: 'muted', style: 'font-size:12.5px;margin-bottom:8px' }, label),
      el('div', { style: 'font-size:26px;font-weight:700;letter-spacing:-.03em' + (color ? ';color:' + color : '') }, value),
      el('div', { class: 'dim', style: 'font-size:12px;margin-top:5px' }, sub));
  }

  function uploadRow(u) {
    const stateColor = { pending: 'var(--dim)', uploading: 'var(--ok)', dead: 'var(--bad)', cancelled: 'var(--warn)', cancelling: 'var(--warn)' }[u.state] || 'var(--dim)';
    const actions = el('td', { style: 'padding-left:20px' }, el('div', { class: 'row' },
      ...(u.state === 'dead' ? [btn(t('action.retry'), () => act('retry', u)), btn(t('action.stop'), () => act('cancel', u))] : []),
      ...((u.state === 'pending' || u.state === 'uploading') ? [btn(t('action.stop'), () => act('cancel', u))] : []),
      ...(u.state === 'cancelled' ? [btn(t('action.resume'), () => resume(u)), btnDanger(t('action.discard'), () => discard(u))] : [])));
    return el('tr', {},
      el('td', {}, el('div', {}, el('div', {}, u.name), el('div', { class: 'dim', style: 'font-size:12px' }, u.last_error || u.remote))),
      el('td', { class: 'detail' }, u.remote),
      el('td', { class: 'num detail' }, bytes(u.size)),
      el('td', { style: 'padding-left:20px' }, el('span', { style: 'display:flex;align-items:center;gap:7px;color:' + stateColor }, el('span', { class: 'dot', style: 'background:' + stateColor }), t('upload.state.' + u.state))),
      actions);
  }
  const btn = (label, fn) => el('button', { onclick: fn, style: 'padding:6px 12px' }, label);
  const btnDanger = (label, fn) => el('button', { class: 'danger', onclick: fn, style: 'padding:6px 12px' }, label);

  async function act(action, u) {
    try { await api.post('/uploads/' + action, { id: u.id }); toast(action === 'retry' ? t('toast.retried') : t('toast.stopped')); load(); }
    catch (e) { toast(e.message, 'bad'); }
  }
  async function resume(u) {
    const ok = await confirmDelete({ title: t('resume.title'), body: t('resume.body'), confirmToken: u.id, confirmLabel: t('resume.confirm'), danger: false });
    if (!ok) return;
    try { await api.post('/uploads/resume', { id: u.id, confirm: true }); toast(t('toast.resumed')); load(); } catch (e) { toast(e.message, 'bad'); }
  }
  async function discard(u) {
    const ok = await confirmDelete({ title: t('discard.title'), body: t('discard.body'), confirmToken: u.id });
    if (!ok) return;
    try { await api.post('/uploads/drop', { id: u.id, confirm: true }); toast(t('toast.discarded')); load(); } catch (e) { toast(e.message, 'bad'); }
  }

  // Two queue-wide actions. Flush asks the uploader to run the queue now
  // instead of on its own schedule, which is what a person wants after fixing
  // the network. Retrying every dead letter at once is the other half of the
  // same moment; per-row retries make that a hundred clicks.
  async function flushAll(btn) {
    btn.disabled = true;
    // The flush reply carries journal.Stats, whose fields have no JSON tags
    // and so arrive capitalised. What a person wants to know afterwards is
    // what is still queued.
    try { const r = await api.post('/uploads/flush', {}); toast(t('toast.flushed', (r.stats && r.stats.Pending) || 0)); load(); }
    catch (e) { toast(e.message, 'bad'); }
    finally { btn.disabled = false; }
  }
  async function retryAll(btn) {
    btn.disabled = true;
    try { const r = await api.post('/uploads/retry', { all: true }); toast(t('toast.requeued', r.requeued || 0)); load(); }
    catch (e) { toast(e.message, 'bad'); }
    finally { btn.disabled = false; }
  }
  const flushBtn = el('button', {}, t('transfers.flush'));
  flushBtn.addEventListener('click', () => flushAll(flushBtn));
  const retryAllBtn = el('button', {}, t('transfers.retryall'));
  retryAllBtn.addEventListener('click', () => retryAll(retryAllBtn));

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between;gap:12px' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('transfers.eyebrow')), el('h2', { class: 'section', style: 'margin-top:6px' }, t('transfers.title'))),
      el('div', { class: 'row', style: 'gap:9px' }, retryAllBtn, flushBtn)),
    el('div', { style: 'padding:0 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, t('col.file')), el('th', {}, t('col.drive')), el('th', { class: 'num' }, t('col.size')), el('th', { style: 'padding-left:20px' }, t('col.status')), el('th', { style: 'padding-left:20px' }, t('col.actions')))), rows))),
    el('div', { style: 'padding:20px' }, el('div', { class: 'panel' }, summary)));

  // A status tick must not undo what the reader did. Reloading from the
  // first page throws away every page they asked for, seconds after the ask,
  // so live refresh applies only while they are still on the first page.
  const off = subscribe(() => { if (!paged) load(); });
  load();
  return () => off();
}
