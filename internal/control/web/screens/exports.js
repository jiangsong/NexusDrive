import { api, ApiError } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, openForm, showPanel, moreRow } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onExportChange } from '/ui/app.js';

// Export copies files out of the mount and onto this machine's own storage —
// a directory, or an external drive. A job is durable and resumable, so it
// outlives the page, the daemon and the drive being unplugged; this screen is
// how a person sees where one got to and steers it.
//
// Two actions ask for a typed confirmation, for the same reason the daemon
// demands confirm: true for them. A mirror deletes whatever the destination
// holds that the plan does not, and forget throws away the record and the
// partial files underneath it.
const STATE_COLOR = {
  planning: 'var(--dim)', running: 'var(--ok)', paused: 'var(--warn-text)',
  done: 'var(--ok)', failed: 'var(--bad)', cancelled: 'var(--warn)', purging: 'var(--dim)',
};

// A paused job is only actionable once you know why, so the reason is shown
// next to the state rather than folded into it.
function stateLabel(j) {
  const state = t('export.state.' + j.state);
  return j.pause_reason ? state + ' · ' + t('export.pause.' + j.pause_reason) : state;
}

// rate and eta_seconds are live figures the daemon fills for a job that is
// still moving, so a finished row simply has no second line.
function rateLabel(j) {
  if (!(j.rate > 0)) return '';
  const eta = j.eta_seconds > 0 ? '  ' + t('exports.eta') + ' ' + duration(j.eta_seconds) : '';
  return bytes(Math.round(j.rate)) + '/s' + eta;
}

function duration(seconds) {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
  return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
}

export function renderExports(host) {
  const rows = el('tbody');
  let paged = false;

  function appendResponse(r) {
    rows.append(...(r.jobs || []).map(jobRow));
    if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('exports.empty'))));
    if (r.next_cursor) rows.append(moreRow(5, () => load(r.next_cursor)));
  }

  async function load(cursor) {
    // A cursor is a string the daemon issued; a click event is not one.
    cursor = pageCursor(cursor);
    paged = !!cursor;
    if (!cursor) fill(rows);
    try {
      const r = await api.get('/exports?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      appendResponse(r);
    } catch (e) {
      const failed = el('tr', {}, el('td', { colspan: '5' }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  function jobRow(j) {
    const color = STATE_COLOR[j.state] || 'var(--dim)';
    const done = j.bytes_total > 0 ? Math.min(1, (j.bytes_done || 0) / j.bytes_total) : 0;
    const actions = el('div', { class: 'row' });
    actions.append(btn(t('exports.details'), () => details(j)));
    if (j.state === 'running' || j.state === 'planning') actions.append(btn(t('action.pause'), () => act('pause', j)));
    if (j.state === 'paused') actions.append(btn(t('action.resume'), () => act('resume', j)));
    if (j.state !== 'done' && j.state !== 'failed' && j.state !== 'cancelled') actions.append(btn(t('action.stop'), () => act('cancel', j)));
    else actions.append(btn(t('exports.forget'), () => forget(j), true));
    const files = (j.files_done || 0) + (j.files_skipped || 0);
    return el('tr', {},
      el('td', {}, el('div', {},
        el('div', {}, (j.sources || []).join(', ')),
        el('div', { class: 'dim', style: 'font-size:12px' }, '→ ' + j.dest),
        j.mirror ? el('div', { style: 'font-size:12px;color:var(--warn-text)' }, t('exports.mirror.badge')) : null,
        j.last_error ? el('div', { style: 'font-size:12px;color:var(--bad)' }, j.last_error) : null)),
      el('td', { class: 'num detail' }, files + ' / ' + (j.files_total || 0)),
      el('td', { style: 'padding-left:20px;min-width:140px' },
        el('div', { class: 'progress' + (j.state === 'paused' ? ' warn' : '') }, el('span', { style: `width:${Math.round(done * 100)}%` })),
        el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:4px' },
          bytes(j.bytes_done || 0) + ' / ' + bytes(j.bytes_total || 0)),
        rateLabel(j) ? el('div', { class: 'dim', style: 'font-size:11.5px' }, rateLabel(j)) : null),
      el('td', { style: 'padding-left:20px' },
        el('span', { style: 'display:flex;align-items:center;gap:7px;color:' + color },
          el('span', { class: 'dot', style: 'background:' + color }), stateLabel(j)),
        j.files_failed > 0 ? el('div', { class: 'dim', style: 'font-size:11.5px;color:var(--bad)' }, t('exports.failed.files', String(j.files_failed))) : null),
      el('td', { style: 'padding-left:20px' }, actions));
  }

  async function details(job) {
    const itemRows = el('tbody');
    const state = el('select', {},
      el('option', { value: '' }, t('exports.items.all')),
      ...['pending', 'active', 'done', 'skipped', 'failed'].map((value) =>
        el('option', { value }, t('export.item.' + value))));
    const summary = el('div', { class: 'detail', style: 'margin-bottom:12px' },
      `${(job.sources || []).join(', ')} → ${job.dest}`);
    const members = el('div');

    const loadItems = async (cursor) => {
      if (!cursor) fill(itemRows);
      try {
        const query = new URLSearchParams({ limit: '100' });
        if (cursor) query.set('cursor', cursor);
        if (state.value) query.set('state', state.value);
        const r = await api.get('/exports/' + job.id + '/items?' + query.toString());
        itemRows.append(...(r.items || []).map((item) => el('tr', {},
          el('td', {}, item.rel),
          el('td', { class: 'num detail' }, item.kind === 'directory' ? '—' : bytes(item.size)),
          el('td', {}, t('export.item.' + item.state)),
          el('td', { class: 'num detail' }, String(item.attempts || 0)),
          el('td', { class: 'detail' }, item.last_error || '—'))));
        if (!itemRows.children.length) fill(itemRows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('exports.items.empty'))));
        if (r.item_next_cursor) itemRows.append(moreRow(5, () => loadItems(r.item_next_cursor)));
      } catch (e) {
        fill(itemRows, el('tr', {}, el('td', { colspan: '5', class: 'detail' }, e.message)));
      }
    };

    try {
      const r = await api.get('/exports/' + job.id);
      const p = r.progress || {};
      if ((p.members || []).length) {
        fill(members,
          el('div', { class: 'eyebrow', style: 'margin:12px 0 6px' }, t('exports.members')),
          ...p.members.map((m) => el('div', { class: 'detail' },
            `${m.remote}: ${bytes(m.bytes || 0)} · ${m.inflight || 0} · ${bytes(Math.round(m.rate || 0))}/s`)));
      }
    } catch (e) { toast(e.message, 'bad'); }

    state.addEventListener('change', () => loadItems());
    const content = el('div', {}, summary, members,
      el('div', { class: 'row', style: 'margin:0 0 8px' }, el('label', { for: state.id = `export-items-${job.id}` }, t('exports.items.state')), state),
      el('div', { class: 'panel', style: 'overflow:auto;max-height:55vh' }, el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, t('col.path')), el('th', { class: 'num' }, t('col.size')),
          el('th', {}, t('col.status')), el('th', {}, t('exports.items.attempts')),
          el('th', {}, t('exports.items.error')))), itemRows)));
    loadItems();
    await showPanel({ title: t('exports.details.title'), content, width: 920 });
  }

  const btn = (label, fn, danger) => el('button', { class: danger ? 'danger' : '', onclick: fn, style: 'padding:6px 12px' }, label);

  async function act(action, j) {
    try {
      await api.post('/exports/' + action, { id: j.id });
      toast(action === 'resume' ? t('toast.resumed') : action === 'pause' ? t('toast.paused') : t('toast.stopped'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  async function forget(j) {
    const ok = await confirmDelete({
      title: t('exports.forget.title'), body: t('exports.forget.body'),
      confirmToken: j.id, confirmLabel: t('exports.forget'),
    });
    if (!ok) return;
    try {
      await api.post('/exports/forget', { id: j.id, confirm: true });
      toast(t('exports.forgotten'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  async function pickSource(input) {
    let cwd = '/';
    const current = el('span', { class: 'detail' }, cwd);
    const list = el('select', { size: '12', style: 'height:auto;width:100%' });
    const up = el('button', { type: 'button' }, t('exports.pick.up'));
    const load = async (path) => {
      const r = await api.get('/fs/list?path=' + encodeURIComponent(path));
      cwd = r.path || path;
      current.textContent = cwd;
      fill(list, el('option', { value: cwd, 'data-dir': 'true' }, t('exports.pick.current')),
        ...(r.entries || []).map((entry) => el('option', {
          value: entry.path, 'data-dir': entry.is_dir ? 'true' : 'false',
        }, (entry.is_dir ? '📁 ' : '📄 ') + entry.name)));
      list.value = cwd;
    };
    up.addEventListener('click', () => load(cwd === '/' ? '/' : (cwd.replace(/\/[^/]+$/, '') || '/')));
    list.addEventListener('dblclick', () => {
      const opt = list.selectedOptions[0];
      if (opt && opt.getAttribute('data-dir') === 'true') load(opt.value);
    });
    try { await load('/'); }
    catch (e) { toast(e.message, 'bad'); return; }
    const ok = await openForm({
      title: t('exports.pick.title'), rows: [[t('exports.pick.path'), list]],
      note: el('div', {}, el('div', { class: 'row', style: 'margin-bottom:8px' }, up, current),
        el('div', { class: 'dim', style: 'font-size:11.5px' }, t('exports.pick.note'))),
      confirmLabel: t('exports.pick.choose'), width: 620,
      validate: () => list.value ? '' : t('exports.pick.required'),
    });
    if (ok) input.value = list.value;
  }

  async function pickDestination(input) {
    const picker = globalThis.cloudfsChooseDirectory;
    if (typeof picker !== 'function') {
      toast(t('exports.dest.manual'));
      input.focus();
      return;
    }
    try {
      const chosen = await picker();
      if (chosen) input.value = chosen;
    } catch (e) { toast(e.message || String(e), 'bad'); }
  }

  async function startExport() {
    const sources = el('input', { type: 'text', placeholder: '/mnt/prefix/photos', autocomplete: 'off', spellcheck: 'false' });
    const dest = el('input', { type: 'text', placeholder: '/Volumes/backup', autocomplete: 'off', spellcheck: 'false' });
    const verify = el('input', { type: 'checkbox' });
    const mirror = el('input', { type: 'checkbox' });
    const ok = await openForm({
      title: t('exports.new'),
      rows: [[t('col.source'), sources], [t('exports.dest'), dest], [t('exports.verify'), verify], [t('exports.mirror'), mirror]],
      note: el('div', {},
        el('div', { class: 'row', style: 'margin-bottom:8px' },
          el('button', { type: 'button', onclick: () => pickSource(sources) }, t('exports.pick.source')),
          el('button', { type: 'button', onclick: () => pickDestination(dest) }, t('exports.pick.dest'))),
        el('div', { class: 'dim', style: 'font-size:11.5px' }, t('exports.new.note'))),
      confirmLabel: t('exports.start'),
      validate: () => {
        if (sources.value.split(',').some((s) => s.trim()) && dest.value.trim()) return '';
        (sources.value.trim() ? dest : sources).focus();
        return t('exports.new.needpaths');
      },
    });
    if (!ok) return;
    const list = sources.value.split(',').map((s) => s.trim()).filter(Boolean);
    const q = { sources: list, dest: dest.value.trim(), verify: verify.checked };
    if (mirror.checked) {
      // A mirror is the only export that deletes anything, so it is typed
      // out in full before the daemon is asked, and confirm goes with it.
      const agreed = await confirmDelete({
        title: t('exports.mirror.title'), body: t('exports.mirror.body'),
        confirmToken: q.dest, confirmLabel: t('exports.start'),
      });
      if (!agreed) return;
      q.mirror = true;
      q.confirm = true;
    }
    try {
      await api.post('/export', q);
      toast(t('exports.started'));
      load();
    } catch (e) { toast(e instanceof ApiError ? e.message : String(e), 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between;gap:12px' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('exports.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('exports.title'))),
      el('div', { class: 'row', style: 'gap:9px' },
        el('button', { onclick: () => load() }, t('action.refresh')),
        el('button', { class: 'primary', onclick: startExport }, t('exports.new')))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('col.path')), el('th', { class: 'num' }, t('exports.files')),
        el('th', { style: 'padding-left:20px' }, t('exports.progress')),
        el('th', { style: 'padding-left:20px' }, t('col.status')),
        el('th', { style: 'padding-left:20px' }, t('col.actions')))), rows))),
    el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px' }, t('exports.note')));

  load();
  // The daemon publishes export snapshots every second on the page's one SSE
  // stream. A reader who paged deeper keeps that stable view until explicitly
  // refreshing; otherwise the first page tracks progress without polling.
  const off = onExportChange((r) => {
    if (paged || document.hidden) return;
    fill(rows);
    appendResponse(r);
  });
  return () => off();
}
