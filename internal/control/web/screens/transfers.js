import { api } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, moreRow } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { subscribe, get } from '/ui/store.js';
import { batchProgress, rateWindow } from '/ui/transfer_progress.js';

// The transfer queue. A file is committed the moment its journal row lands;
// this is the trip to the remote. Closing the page does not stop it.
//
// Two things a person wants from this page, in this order: how far the
// whole copy has got — one bar, a rate, a time — and then which files are
// still out. The bar comes from the status document the daemon streams every
// second and costs nothing to repaint; the table is fetched only when the
// queue's shape changes, and its rows are kept in place across fetches so
// the page does not flicker while a thousand files go up.
export function renderTransfers(host) {
  const rows = el('tbody');
  // True once the reader has asked for more than the first page.
  let paged = false;
  const summary = el('div', { style: 'display:flex;gap:26px;padding:20px' });
  const progress = el('div', { class: 'panel', style: 'margin:0 20px 20px;padding:18px 20px' });
  const rate = rateWindow();

  // Rows by upload id, so a refetch reuses the node of a row that has not
  // changed: the browser then leaves it alone, and only rows whose state
  // moved are redrawn.
  const known = new Map();
  function rowFor(u) {
    const sig = [u.state, u.attempt, u.last_error || '', u.size].join('|');
    const have = known.get(u.id);
    if (have && have.sig === sig) return have.node;
    const node = uploadRow(u);
    known.set(u.id, { sig, node });
    return node;
  }

  // The queue is paged like every other listing: the journal can hold far
  // more than a hundred rows after a long offline stretch, and the ones past
  // the first page are exactly the ones a person is looking for.
  let loading = false;
  let loadAgain = false;
  async function load(cursor) {
    // Every refresh path calls this, and one of them is an event handler.
    // A cursor is a string the daemon issued; anything else means "the
    // first page", never "encode this object into &cursor=".
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (loading && !cursor) { loadAgain = true; return; }
    loading = true;
    try {
      const r = await api.get('/uploads?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      const nodes = (r.uploads || []).map(rowFor);
      if (cursor) {
        rows.append(...nodes);
      } else {
        const seen = new Set((r.uploads || []).map((u) => u.id));
        for (const id of known.keys()) if (!seen.has(id)) known.delete(id);
        if (!nodes.length) nodes.push(el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('transfers.empty'))));
        fill(rows, ...nodes);
      }
      if (r.next_cursor) rows.append(moreRow(5, () => load(r.next_cursor)));
    } catch (e) {
      // A failed continuation must not take the pages already on screen with
      // it: the rows being read cost nothing to keep.
      const failed = el('tr', {}, el('td', { colspan: '5' }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    } finally {
      loading = false;
      if (loadAgain) { loadAgain = false; load(); }
    }
    refreshSummary();
  }

  function refreshSummary() {
    const s = get().status;
    if (!s || !s.uploads) return;
    const u = s.uploads;
    fill(summary,
      stat(t('transfers.queued'), bytes(u.queued_bytes), t('transfers.tasks', (u.pending || 0) + (u.uploading || 0))),
      stat(t('transfers.retained'), bytes(u.retained_bytes), t('transfers.retained.note'), 'var(--danger-text)'),
      stat(t('transfers.completed'), String(u.done || 0), t('transfers.keep')));
  }

  // The overall bar. Idle with nothing behind it says so; a finished batch
  // stays on screen, full, until the next one starts; a running one shows
  // files and bytes done over the batch, the rate over the last seconds and
  // the time that rate leaves. Dead letters are named here too — a bar that
  // reached the end with three files failed is not "done".
  function refreshProgress() {
    const s = get().status;
    const u = (s && s.uploads) || {};
    const b = u.batch || {};
    const p = batchProgress(b, rate.sample(Date.now(), b.bytes_done || 0, b.active));
    const dead = u.dead || 0;
    const head = el('div', { style: 'display:flex;justify-content:space-between;align-items:baseline;gap:12px;flex-wrap:wrap' });
    const bar = el('div', { class: 'progress' + (p.state === 'active' ? ' active' : ''), style: 'margin-top:12px;height:8px' },
      el('span', { style: 'width:' + p.percent + '%' + (p.state === 'idle' ? ';background:transparent' : '') }));
    let title, detail;
    if (p.state === 'idle') {
      title = t('transfers.progress.idle');
      detail = dead ? t('transfers.progress.dead', dead) : t('transfers.progress.idle.note');
    } else if (p.state === 'done') {
      title = t('transfers.progress.done', p.filesDone, bytes(p.bytesDone));
      detail = (b.finished_at ? t('transfers.progress.finished', new Date(b.finished_at).toLocaleTimeString(locale())) : '') +
        (dead ? ' · ' + t('transfers.progress.dead', dead) : '');
    } else {
      title = t('transfers.progress.active', p.filesDone, p.filesTotal);
      const parts = [bytes(p.bytesDone) + ' / ' + bytes(p.bytesTotal)];
      if (p.rate > 0) parts.push(t('transfers.progress.rate', bytes(p.rate)));
      if (p.eta != null) parts.push(t('transfers.progress.eta', etaText(p.eta)));
      if (dead) parts.push(t('transfers.progress.dead', dead));
      detail = parts.join(' · ');
    }
    fill(head,
      el('div', { style: 'font-size:16px;font-weight:650' }, title),
      el('div', { class: 'detail', style: 'font-size:13px' }, p.state === 'active' ? Math.floor(p.percent) + '%' : ''));
    fill(progress, head, bar, el('div', { class: dead ? 'detail warn-text' : 'detail', style: 'font-size:13px;margin-top:10px' }, detail));
  }
  function etaText(sec) {
    if (sec < 60) return t('transfers.eta.seconds', Math.max(1, Math.round(sec)));
    if (sec < 3600) return t('transfers.eta.minutes', Math.round(sec / 60));
    return t('transfers.eta.hours', Math.round(sec / 360) / 10);
  }
  function stat(label, value, sub, color) {
    return el('div', {}, el('div', { class: 'muted', style: 'font-size:12.5px;margin-bottom:8px' }, label),
      el('div', { style: 'font-size:26px;font-weight:700;letter-spacing:-.03em' + (color ? ';color:' + color : '') }, value),
      el('div', { class: 'dim', style: 'font-size:12px;margin-top:5px' }, sub));
  }

  function uploadRow(u) {
    const stateColor = { pending: 'var(--dim)', uploading: 'var(--ok)', dead: 'var(--bad)', cancelled: 'var(--warn)', cancelling: 'var(--warn)' }[u.state] || 'var(--dim)';
    // A directory creation has no bytes to keep, so it can be retried but
    // not stopped, resumed or discarded: a stopped creation would leave the
    // directory local for good.
    const mkdir = u.kind === 'mkdir';
    const actions = el('td', { style: 'padding-left:20px' }, el('div', { class: 'row' },
      ...(u.state === 'dead' ? [btn(t('action.retry'), () => act('retry', u)), ...(mkdir ? [] : [btn(t('action.stop'), () => act('cancel', u))])] : []),
      ...((u.state === 'pending' || u.state === 'uploading') && !mkdir ? [btn(t('action.stop'), () => act('cancel', u))] : []),
      ...(u.state === 'cancelled' ? [btn(t('action.resume'), () => resume(u)), btnDanger(t('action.discard'), () => discard(u))] : [])));
    const removal = u.kind === 'delete' || u.kind === 'rmdir';
    const name = mkdir || u.kind === 'rmdir' ? u.name + '/' : u.name;
    const note = u.last_error || (mkdir ? t('upload.kind.mkdir') : removal ? t('upload.kind.' + u.kind) : u.remote);
    // A file on the wire gets a moving bar of its own: the queue does not
    // know how far a single request has got, so the bar only says "this
    // one, now" — the overall bar above is the one with numbers.
    const inflight = u.state === 'uploading' ? el('div', { class: 'progress active', style: 'margin-top:6px;width:160px' }, el('span', { style: 'width:100%' })) : null;
    return el('tr', {},
      el('td', {}, el('div', {}, el('div', {}, name), el('div', { class: 'dim', style: 'font-size:12px' }, note), inflight)),
      el('td', { class: 'detail' }, u.remote),
      el('td', { class: 'num detail' }, mkdir ? '—' : bytes(u.size)),
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
    progress,
    el('div', { style: 'padding:0 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, t('col.file')), el('th', {}, t('col.drive')), el('th', { class: 'num' }, t('col.size')), el('th', { style: 'padding-left:20px' }, t('col.status')), el('th', { style: 'padding-left:20px' }, t('col.actions')))), rows))),
    el('div', { style: 'padding:20px' }, el('div', { class: 'panel' }, summary)));

  // Every status tick repaints the bar; the table is refetched only when
  // the queue's shape changed since the last fetch — a row landed, failed,
  // was stopped or added — and never more than once every two seconds. A
  // status tick must not undo what the reader did either: reloading from
  // the first page throws away every page they asked for, so live refresh
  // applies only while they are still on the first page.
  let lastShape = '';
  let lastFetch = 0;
  let pendingFetch = null;
  const off = subscribe(() => {
    refreshProgress();
    const s = get().status;
    const u = (s && s.uploads) || {};
    const shape = [u.pending, u.uploading, u.dead, u.cancelling, u.cancelled, u.purging, (u.batch || {}).files_done].join('|');
    if (shape === lastShape || paged) return;
    lastShape = shape;
    const wait = Math.max(0, 2000 - (Date.now() - lastFetch));
    if (pendingFetch) return;
    pendingFetch = setTimeout(() => { pendingFetch = null; lastFetch = Date.now(); load(); }, wait);
  });
  refreshProgress();
  load();
  return () => { off(); if (pendingFetch) clearTimeout(pendingFetch); };
}
