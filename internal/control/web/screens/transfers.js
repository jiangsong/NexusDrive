import { api } from '/ui/api.js';
import { el, iconEl, bytes, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { subscribe, get } from '/ui/store.js';

// The transfer queue. A file is committed the moment its journal row lands;
// this is the trip to the remote. Closing the page does not stop it.
export function renderTransfers(host) {
  const rows = el('tbody');
  const summary = el('div', { style: 'display:flex;gap:26px;padding:20px' });

  async function load() {
    try {
      const r = await api.get('/uploads?limit=100');
      rows.replaceChildren(...(r.uploads || []).map(uploadRow));
      if (!(r.uploads || []).length) rows.replaceChildren(el('tr', {}, el('td', { colspan: '5', class: 'dim' }, '当前没有活动、死信或已停止的上传')));
    } catch (e) { rows.replaceChildren(el('tr', {}, el('td', { colspan: '5' }, e.message))); }
    const s = get().status;
    if (s && s.uploads) {
      const u = s.uploads;
      summary.replaceChildren(
        stat('待传数据', bytes(u.queued_bytes), `${(u.pending || 0) + (u.uploading || 0)} 个任务`),
        stat('死信持有', bytes(u.retained_bytes), '重试要用，不会自动清', 'var(--danger-text)'),
        stat('已完成', String(u.done || 0), '最近保留 200 条'));
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
      ...(u.state === 'cancelled' ? [btn('恢复…', () => resume(u)), btnDanger('丢弃本地版本…', () => discard(u))] : [])));
    return el('tr', {},
      el('td', {}, el('div', {}, el('div', {}, u.name), el('div', { class: 'dim', style: 'font-size:12px' }, u.last_error || u.remote))),
      el('td', { class: 'detail' }, u.remote),
      el('td', { class: 'num detail' }, bytes(u.size)),
      el('td', { style: 'padding-left:20px' }, el('span', { style: 'display:flex;align-items:center;gap:7px;color:' + stateColor }, el('span', { class: 'dot', style: 'background:' + stateColor }), u.state)),
      actions);
  }
  const btn = (label, fn) => el('button', { onclick: fn, style: 'padding:6px 12px' }, label);
  const btnDanger = (label, fn) => el('button', { class: 'danger', onclick: fn, style: 'padding:6px 12px' }, label);

  async function act(action, u) {
    try { await api.post('/uploads/' + action, { id: u.id }); toast(`${u.name}: ${action}`); load(); }
    catch (e) { toast(e.message, 'bad'); }
  }
  async function resume(u) {
    const ok = await confirmDelete({ title: '恢复上传', body: '恢复可能在远端产生重复文件或覆盖。确认已核对目标。', confirmToken: u.id, confirmLabel: '确认恢复', danger: false });
    if (!ok) return;
    try { await api.post('/uploads/resume', { id: u.id, confirm: true }); toast('已恢复'); load(); } catch (e) { toast(e.message, 'bad'); }
  }
  async function discard(u) {
    const ok = await confirmDelete({ title: '永久丢弃本地版本', body: '不删除远端数据，但会永久丢弃本地保留的这一版。若远端从未收到，内容就没了。', confirmToken: u.id });
    if (!ok) return;
    try { await api.post('/uploads/drop', { id: u.id, confirm: true }); toast('已丢弃'); load(); } catch (e) { toast(e.message, 'bad'); }
  }

  host.append(
    el('div', { class: 'pad' }, el('div', { class: 'eyebrow' }, '写入'), el('h2', { class: 'section', style: 'margin-top:6px' }, t('transfers.title'))),
    el('div', { style: 'padding:0 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, '文件'), el('th', {}, '网盘'), el('th', { class: 'num' }, '大小'), el('th', { style: 'padding-left:20px' }, '状态'), el('th', { style: 'padding-left:20px' }, '操作'))), rows))),
    el('div', { style: 'padding:20px' }, el('div', { class: 'panel' }, summary)));

  const off = subscribe(() => load());
  load();
  return () => off();
}
