import { api } from '/ui/api.js';
import { el, fill } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';

// The heat tab: what is being read, by whom (agents, programs on the
// mount, the console, WebDAV), and how long ago each file changed —
// GET /agent/heat. The quadrant that matters is hot-stale: files everyone
// relies on that nobody has updated. Nothing here pins or indexes; the
// row only says what a person might do. Counts are per kind of reader,
// never per person: heat is about files.
const COLUMNS = 6;
const QUADRANTS = ['hot_stale', 'hot_fresh', 'warm_stale', 'warm_fresh'];
const KINDS = ['agent', 'kernel', 'console', 'webdav'];

function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

function kindCell(byKind) {
  const parts = [];
  for (const k of KINDS) {
    const n = (byKind || {})[k] || 0;
    if (n) parts.push(t('heat.kind.' + k) + ' ' + n);
  }
  return parts.join(' · ');
}

function quadrantCell(q) {
  const dot = q === 'hot_stale' ? 'warn' : q === 'hot_fresh' ? 'ok' : '';
  return el('span', { style: 'display:inline-flex;align-items:center;gap:7px;white-space:nowrap' },
    el('span', { class: 'dot ' + dot }), t('heat.quadrant.' + q));
}

function row(e) {
  return el('tr', { 'data-quadrant': e.quadrant },
    el('td', { class: 'detail', style: 'word-break:break-all' }, e.path),
    el('td', { class: 'num tnums' }, String(e.reads)),
    el('td', { class: 'dim' }, kindCell(e.by_kind)),
    el('td', { class: 'dim', style: 'white-space:nowrap' }, when(e.last_read)),
    el('td', { class: 'dim', style: 'white-space:nowrap' }, when(e.mtime)),
    el('td', {}, quadrantCell(e.quadrant)));
}

export function renderHeatTab(host) {
  let disposed = false;
  const rows = el('tbody');
  const path = el('input', { type: 'text', placeholder: t('heat.filter.path'), 'aria-label': t('heat.filter.path'), autocomplete: 'off', spellcheck: 'false', style: 'width:260px' });
  const days = el('select', { 'aria-label': t('heat.filter.days'), style: 'width:auto' },
    ...['1', '7', '30'].map((v) => el('option', { value: v, selected: v === '7' }, t('heat.days.' + v))));
  const only = el('select', { 'aria-label': t('heat.filter.quadrant'), style: 'width:auto' },
    el('option', { value: '' }, t('heat.filter.quadrant')),
    ...QUADRANTS.map((q) => el('option', { value: q }, t('heat.quadrant.' + q))));
  const note = el('p', { class: 'detail', style: 'margin:0 0 10px' }, t('heat.note'));

  async function load() {
    const q = new URLSearchParams();
    if (path.value.trim()) q.set('path', path.value.trim());
    q.set('days', days.value);
    fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('heat.loading'))));
    try {
      const r = await api.get('/agent/heat?' + q.toString());
      if (disposed) return;
      if (!r.enabled) {
        fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('heat.disabled'))));
        return;
      }
      const entries = (r.entries || []).filter((e) => !only.value || e.quadrant === only.value);
      fill(rows, entries.length ? entries.map(row)
        : el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('heat.empty'))));
    } catch (e) {
      if (disposed) return;
      fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'detail' }, e.message)));
    }
  }
  path.addEventListener('change', load);
  days.addEventListener('change', load);
  only.addEventListener('change', load);

  fill(host,
    el('div', { style: 'padding:14px 20px 0' },
      note,
      el('div', { class: 'row', style: 'gap:8px;flex-wrap:wrap;margin-bottom:10px' }, path, days, only),
      el('div', { class: 'table-wrap' },
        el('table', { class: 'table' },
          el('thead', {}, el('tr', {},
            el('th', {}, t('heat.col.path')), el('th', { class: 'num' }, t('heat.col.reads')), el('th', {}, t('heat.col.by')),
            el('th', {}, t('heat.col.last_read')), el('th', {}, t('heat.col.mtime')), el('th', {}, t('heat.col.quadrant')))),
          rows))));
  load();
  return () => { disposed = true; };
}
