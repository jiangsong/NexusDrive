import { api } from '/ui/api.js';
import { el, fill, openPanel, confirmDelete, toast, iconEl } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';

// The suggestions overlay (ui-plan G6-3): the drafts GET /agent/suggestions
// makes from read heat — pin what agents read but the cache does not hold,
// index what they read that no rule covers, look at what is hot and has
// not changed in ninety days, release a pin nothing reads. Each draft has
// an "adopt" button; adopting is the existing guarded route with its own
// confirmation — /cache/pin, /cache/unpin, /index/add — and this overlay
// writes nothing else. The daemon never adopts a draft by itself: a pin is
// a download, and "no download by default" outranks convenience.
//
// Paths and counts are daemon data and go in as text.

const KINDS = ['pin', 'index', 'stale', 'unpin'];

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// adopt runs the guarded route for a draft after the person typed the
// file's name; stale is a notice, not an action.
async function adopt(sg) {
  const name = String(sg.path || '').split('/').pop() || sg.path;
  const ok = await confirmDelete({ title: t('suggest.adopt.title', t('suggest.kind.' + sg.kind)), body: sg.path, confirmToken: name, confirmLabel: t('suggest.adopt'), danger: false });
  if (!ok) return false;
  try {
    if (sg.kind === 'pin') await api.post('/cache/pin', { path: sg.path, confirm: true });
    else if (sg.kind === 'unpin') await api.post('/cache/unpin', { path: sg.path, confirm: true });
    else if (sg.kind === 'index') await api.post('/index/add', { path: sg.path });
    toast(t('suggest.adopted'));
    return true;
  } catch (err) { toast(err.message, 'bad'); return false; }
}

function draftRow(sg, onDone) {
  const actionable = sg.kind !== 'stale';
  const btn = actionable ? el('button', { style: 'font-size:12px', 'data-adopt': sg.kind, onclick: async () => { if (await adopt(sg)) { btn.disabled = true; onDone(); } } }, t('suggest.adopt')) : null;
  return el('tr', { 'data-suggestion': sg.kind },
    el('td', { style: 'white-space:nowrap' }, t('suggest.kind.' + sg.kind)),
    el('td', { class: 'detail', style: 'word-break:break-all' }, sg.path),
    el('td', { class: 'num tnums' }, sg.reads ? String(sg.reads) : ''),
    el('td', { class: 'dim', style: 'white-space:nowrap' }, sg.mtime ? when(sg.mtime) : ''),
    el('td', {}, btn));
}

// openSuggestions opens the overlay for the heat tab's current path and
// window. onAdopted is called after a draft was adopted, so the tab can
// reload.
export function openSuggestions({ path, days, onAdopted }) {
  const rows = el('tbody');
  const close = openPanel({ title: t('suggest.title'), width: 760,
    content: el('div', {},
      el('p', { class: 'dim', style: 'margin:0 0 10px;font-size:12px' }, t('suggest.note')),
      el('div', { class: 'panel', style: 'overflow:auto;max-height:60vh' },
        el('table', {}, el('thead', {}, el('tr', {},
          el('th', {}, t('suggest.col.kind')), el('th', {}, t('heat.col.path')), el('th', { class: 'num' }, t('heat.col.reads')),
          el('th', {}, t('heat.col.mtime')), el('th', {}))), rows))) });
  fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('heat.loading'))));
  const q = new URLSearchParams();
  if (path) q.set('path', path);
  q.set('days', String(days || 30));
  api.get('/agent/suggestions?' + q.toString()).then((r) => {
    if (!r.enabled) { fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('heat.disabled')))); return; }
    const list = (r.suggestions || []).filter((s) => KINDS.includes(s.kind));
    fill(rows, list.length ? list.map((s) => draftRow(s, onAdopted || (() => {})))
      : el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('suggest.empty'))));
  }).catch((err) => fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'detail' }, err.message))));
  return close;
}

// suggestButton is the heat tab's entry point.
export function suggestButton(opts) {
  return el('button', { 'data-suggest': '', onclick: () => openSuggestions(opts()) }, iconEl('bolt'), t('suggest.button'));
}
