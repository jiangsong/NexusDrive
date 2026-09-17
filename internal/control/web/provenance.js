import { el, fill, iconEl } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';

// The inspector's provenance rows (docs/agent-first-design.md §6, ui-plan
// G5-1 and G6-4): who last changed the selected file — the origin as an
// icon with its word, the session or principal, the time — with a
// button that opens the file's history; and how often it was read in the
// last thirty days, by kind of reader. main.js hands this module an empty
// host under the entry's other rows; the requests and the wording live
// here. The daemon answers GET /changes?path=&limit=1 from the changes
// table and GET /agent/heat?path=&days=30 from read_heat; a daemon
// without agent.db answers enabled false and the host stays empty.
//
// Paths, origins, principals and counts are daemon data and go in as text
// nodes. An origin the catalog does not know is shown as the daemon sent
// it, beside the generic file icon.

// ORIGINS maps a change's origin to its icon; the word is origin.<name>.
const ORIGINS = { kernel: 'folder', mcp: 'bot', control: 'globe', webdav: 'cloud', remote: 'cloud' };

// HEAT_DAYS is the window of the read line, the thirty days of ui-plan G6-4.
const HEAT_DAYS = 30;

export function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// originCell is the icon and word for an origin, as one inline span.
export function originCell(origin) {
  const known = Object.prototype.hasOwnProperty.call(ORIGINS, origin);
  return el('span', { style: 'display:inline-flex;align-items:center;gap:5px;white-space:nowrap', 'data-origin': origin || '' },
    iconEl(known ? ORIGINS[origin] : 'file'), known ? t('origin.' + origin) : (origin || ''));
}

// actorText names who made a change: the session (shortened) when the
// change came through one, else the principal, else nothing.
export function actorText(c) {
  if (c.session_id) return String(c.session_id).slice(0, 8);
  return c.principal || '';
}

// mountProvenance fills container with the two rows for path; a path the
// record does not know, or a daemon that cannot answer, leaves it empty.
// openSession receives a session id when the actor is clicked; openHistory
// receives the path when the history button is.
export async function mountProvenance(container, path, { api, openSession, openHistory }) {
  const rows = [];
  let changes;
  try { changes = await api.get('/changes?path=' + encodeURIComponent(path) + '&limit=1'); } catch (_) { changes = null; }
  const c = changes && changes.enabled && (changes.changes || [])[0];
  if (c) {
    const actor = actorText(c);
    rows.push(el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px', 'data-last-writer': c.origin || '' },
      el('span', { class: 'muted' }, t('inspector.lastwriter')),
      el('span', { class: 'detail', style: 'display:inline-flex;align-items:center;gap:6px;flex-wrap:wrap;justify-content:flex-end' },
        originCell(c.origin),
        actor ? [' · ', c.session_id
          ? el('button', { style: 'font-size:12px', 'data-actor': c.session_id, onclick: () => openSession(c.session_id) }, actor)
          : el('span', {}, actor)] : null,
        ' · ', when(c.ts),
        c.reliable === false ? el('span', { class: 'dim' }, ' · ', t('changes.unreliable')) : null,
        el('button', { style: 'font-size:12px', 'data-history': path, onclick: () => openHistory(path) }, iconEl('undo'), t('inspector.history')))));
  }
  let heat;
  try { heat = await api.get('/agent/heat?path=' + encodeURIComponent(path) + '&days=' + HEAT_DAYS + '&limit=1'); } catch (_) { heat = null; }
  const h = heat && heat.enabled && (heat.entries || []).find((e) => e.path === path);
  if (h) {
    const by = Object.entries(h.by_kind || {}).filter(([, n]) => n > 0).map(([k, n]) => t('heat.kind.' + k) + ' ' + n).join(' · ');
    rows.push(el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px', 'data-reads': String(h.reads) },
      el('span', { class: 'muted' }, t('inspector.reads', HEAT_DAYS)),
      el('span', { class: 'detail', style: 'text-align:right', title: by }, String(h.reads))));
  }
  fill(container, ...rows);
}
