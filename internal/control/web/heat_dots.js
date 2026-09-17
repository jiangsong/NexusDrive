import { el } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The heat dots of the file list (ui-plan G6-4): after a directory is
// listed, one request to GET /agent/heat?path=<dir>&days=30 and a small
// dot with the thirty-day read count beside every entry the heat table
// knows, hovering to the split by kind of reader. The list is drawn first
// and the dots land after: a daemon without agent.db (enabled false) or a
// failed request leaves the list as it was. Counts are daemon numbers and
// go in as text.

const HEAT_DAYS = 30;
const KINDS = ['agent', 'kernel', 'console', 'webdav'];

// splitText is the hover text: "agent 7 · terminal 2".
export function splitText(byKind) {
  return KINDS.filter((k) => (byKind || {})[k] > 0).map((k) => t('heat.kind.' + k) + ' ' + byKind[k]).join(' · ');
}

// decorateHeat adds the dots to the rows (a <tbody> of tr[data-path]) of
// dir. It resolves when done; nothing awaits it.
export async function decorateHeat(rows, dir, { api }) {
  let r;
  try {
    r = await api.get('/agent/heat?path=' + encodeURIComponent(dir) + '&days=' + HEAT_DAYS + '&limit=1000');
  } catch (_) { return; }
  if (!r || !r.enabled) return;
  for (const e of r.entries || []) {
    const tr = rows.querySelector('tr[data-path="' + CSS.escape(e.path) + '"]');
    if (!tr || tr.querySelector('[data-heat]')) continue;
    const cell = tr.querySelector('td span');
    if (!cell) continue;
    cell.append(el('span', { class: 'chip heat-dot', 'data-heat': String(e.reads), title: splitText(e.by_kind), 'aria-label': t('heat.dot', HEAT_DAYS, e.reads) },
      el('span', { class: 'dot ' + (e.quadrant === 'hot_stale' ? 'warn' : 'ok'), 'aria-hidden': 'true' }), String(e.reads)));
  }
}
