import { api } from '/ui/api.js';
import { el, fill, bytes, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { get, subscribe } from '/ui/store.js';

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
    fill(cards,
      card(t('storage.used'), c.bytes_human || '0 B', c.max_bytes ? t('storage.limit', bytes(c.max_bytes)) : t('storage.nolimit')),
      card(t('storage.hit'), Math.round((c.hit_ratio || 0) * 100) + '%', t('storage.reads', (c.hits || 0).toLocaleString())),
      card(t('storage.evictions'), String(c.evictions || 0), t('storage.evictions.note')),
      card(t('storage.free'), bytes(c.free_bytes), t('storage.free.note')));
  }

  async function loadPins() {
    try {
      const r = await api.get('/cache/pins');
      fill(pinRows, ...(r.pins || []).map((p) => el('tr', {},
        el('td', {}, p.path),
        el('td', { class: 'detail' }, p.recursive ? t('storage.recursive') : t('storage.single')),
        el('td', { class: 'muted' }, p.configured ? t('storage.source.config') : t('storage.source.ui')),
        el('td', { style: 'text-align:right' }, p.configured
          ? el('span', { class: 'dimmer' }, t('storage.editconfig')) :
          el('button', { style: 'padding:6px 12px', onclick: () => unpin(p) }, t('action.unpin'))))));
      if (!(r.pins || []).length) fill(pinRows, el('tr', {}, el('td', { colspan: '4', class: 'dim' }, t('empty'))));
    } catch (e) { fill(pinRows, el('tr', {}, el('td', { colspan: '4' }, e.message))); }
  }
  async function unpin(p) {
    try { await api.post('/cache/unpin', { path: p.path }); toast(t('toast.unpinned')); loadPins(); } catch (e) { toast(e.message, 'bad'); }
  }
  // Dropping the kernel's cached pages for our files frees memory and forces
  // the next read to come from the block cache or the remote. It is safe but
  // never free, so it says how many files it touched rather than nothing.
  async function dropCaches() {
    try { const r = await api.post('/cache/drop', {}); toast(t('toast.dropped', r.files_dropped || 0)); refreshCards(); }
    catch (e) { toast(e.message, 'bad'); }
  }
  async function gc() {
    try { const r = await api.post('/cache/gc', {}); toast(t('toast.gc', bytes(r.freed_bytes || 0))); refreshCards(); } catch (e) { toast(e.message, 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('storage.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('storage.title'))),
      el('div', { class: 'row', style: 'gap:9px' },
        el('button', { onclick: dropCaches }, t('storage.drop')),
        el('button', { onclick: gc }, t('storage.gc')))),
    cards,
    el('div', { style: 'padding:20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('storage.rules')),
      el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('col.path')), el('th', {}, t('col.scope')), el('th', {}, t('col.source')), el('th', { style: 'text-align:right' }, t('col.actions')))), pinRows))));

  // A pin that came from the configuration cannot be removed here: the daemon
  // reads it from the file at every start, so an API removal would come back.
  // Saying where to change it once, under the table, beats a tooltip nobody
  // hovers.
  host.append(el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('storage.editconfig.help')));

  refreshCards();
  loadPins();
  const off = subscribe(() => refreshCards());
  return off;
}
