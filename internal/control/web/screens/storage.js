import { api } from '/ui/api.js';
import { el, bytes, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { get } from '/ui/store.js';

export function renderStorage(host) {
  const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px;padding:0 20px' });
  const pinRows = el('tbody');

  function card(label, value, sub) {
    return el('div', { class: 'panel pad' },
      el('div', { class: 'muted', style: 'font-size:13px;margin-bottom:9px' }, label),
      el('div', { style: 'font-size:28px;font-weight:720;letter-spacing:-.03em' }, value),
      el('div', { class: 'detail', style: 'font-size:13px;margin-top:12px' }, sub));
  }

  function refreshCards() {
    const c = (get().status || {}).cache || {};
    cards.replaceChildren(
      card(t('storage.used'), c.bytes_human || '0 B', `上限 ${bytes(c.max_bytes)}`),
      card(t('storage.hit'), Math.round((c.hit_ratio || 0) * 100) + '%', `${(c.hits || 0).toLocaleString()} 读`),
      card('最近淘汰', String(c.evictions || 0), '固定与打开中的不参与'),
      card('可用磁盘', bytes(c.free_bytes), '逼近留白时拒绝写入'));
  }

  async function loadPins() {
    try {
      const r = await api.get('/cache/pins');
      pinRows.replaceChildren(...(r.pins || []).map((p) => el('tr', {},
        el('td', {}, p.path),
        el('td', { class: 'detail' }, p.recursive ? '递归' : '单个文件'),
        el('td', { class: 'muted' }, p.configured ? '配置' : '界面'),
        el('td', { style: 'text-align:right' }, p.configured ? el('span', { class: 'dimmer' }, '改配置') :
          el('button', { style: 'padding:6px 12px', onclick: () => unpin(p) }, t('action.unpin'))))));
      if (!(r.pins || []).length) pinRows.replaceChildren(el('tr', {}, el('td', { colspan: '4', class: 'dim' }, t('empty'))));
    } catch (e) { pinRows.replaceChildren(el('tr', {}, el('td', { colspan: '4' }, e.message))); }
  }
  async function unpin(p) {
    try { await api.post('/cache/unpin', { path: p.path }); toast('已取消固定'); loadPins(); } catch (e) { toast(e.message, 'bad'); }
  }
  async function gc() {
    try { const r = await api.post('/cache/gc', {}); toast(`回收了 ${bytes(r.freed_bytes || 0)}`); refreshCards(); } catch (e) { toast(e.message, 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
      el('div', {}, el('div', { class: 'eyebrow' }, '本地'), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('storage.title'))),
      el('button', { onclick: gc }, t('storage.gc'))),
    cards,
    el('div', { style: 'padding:20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('storage.rules')),
      el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, '路径'), el('th', {}, '范围'), el('th', {}, '来源'), el('th', { style: 'text-align:right' }, '操作'))), pinRows))));

  refreshCards();
  loadPins();
  return () => {};
}
