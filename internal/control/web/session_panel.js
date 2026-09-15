import { api } from '/ui/api.js';
import { el, fill, iconEl, bytes, openPanel, confirmDelete, toast } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { scopeParts } from '/ui/scope_view.js';
import { groupPlan, shortID, planCounts } from '/ui/rollback_plan.js';
import { onFsChange } from '/ui/app.js';

// The session panel: one MCP session in detail — who it is, what it may
// touch, the writes it recorded and the last fifty things it did — with
// the two actions a person can take on it: finishing it, and rolling its
// writes back. GET /sessions/<id> answers all of that in one round trip,
// so the panel opens on a single request.
//
// Everything shown here was written by the agent or its client (client
// name, tool names, paths, summary, error text, plan reasons). It all goes
// in as text nodes.
const TAIL_COLUMNS = 4;
const ARTIFACT_COLUMNS = 4;
const OPS_COLUMNS = 5;

// The reasons a plan item can carry (docs/agent-roadmap.md §4.8) that have
// a phrase of their own; anything else — the text of a write error — is
// shown as the daemon sent it.
const REASONS = new Set(['already', 'too_large', 'not_cached', 'dir', 'not_empty', 'missing', 'incomplete', 'exists', 'from_exists']);

// showInFiles sends the main window to a path: it lands on the parent
// directory with that entry selected (see the deep link in screens/main.js).
function showInFiles(path) {
  location.hash = '#/connections?path=' + encodeURIComponent(path);
}

// openFolder sends the main window into a directory.
function openFolder(path) {
  location.hash = '#/connections?dir=' + encodeURIComponent(path);
}

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// artifactState is the dot and word for one artifact. The manifest's state
// is what the daemon saw when the session finished; the live answer comes
// from /fs/stat, and a change event re-asks. A file that is gone says so
// rather than keeping the manifest's word for it.
function artifactState(a) {
  const state = el('span', { style: 'display:inline-flex;align-items:center;gap:7px' });
  const show = (local) => fill(state, el('span', { class: 'dot ' + (local ? 'warn' : 'ok') }), t('artifact.state.' + (local ? 'local' : 'synced')));
  if (a.state === 'synced' || a.state === 'local') show(a.state === 'local');
  else fill(state, '…');
  api.get('/fs/stat?path=' + encodeURIComponent(a.path))
    .then((st) => show(!!st.local_only))
    .catch(() => fill(state, el('span', { class: 'dot bad' }), t('artifact.state.missing')));
  return state;
}

// artifactRow is one delivered file. "Copy link" asks the daemon for a
// signed URL only when clicked and hands it to the clipboard: the link is
// short-lived, and a table that stays open would otherwise hold a stale one.
// It is never written into the row, and a clipboard failure is reported as
// such rather than by showing the URL.
function artifactRow(a, onNavigate) {
  return el('tr', { 'data-artifact': a.path },
    el('td', { class: 'detail', style: 'word-break:break-all' }, a.path),
    el('td', { class: 'num dim tnums' }, bytes(a.size)),
    el('td', {}, artifactState(a)),
    el('td', { style: 'white-space:nowrap' },
      el('button', { onclick: () => onNavigate(a.path) }, t('artifact.open')),
      ' ',
      el('button', { onclick: async () => {
        try {
          const r = await api.get('/fs/download-url?path=' + encodeURIComponent(a.path));
          await navigator.clipboard.writeText(r.url);
          toast(t('artifact.copied'));
        } catch (_) {
          toast(t('artifact.copyfailed'), 'bad');
        }
      } }, t('artifact.copylink'))));
}

function auditTailRow(r) {
  const paths = r.paths || [];
  return el('tr', { class: r.result === 'denied' ? 'denied' : '', 'data-result': r.result },
    el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(r.ts)),
    el('td', {}, r.tool),
    el('td', { class: 'detail', style: 'word-break:break-all' }, paths.join(', ')),
    el('td', {}, t('audit.result.' + r.result)));
}


// preState is the dot and word for what the session kept of a path before
// it wrote: a preimage that can be restored, one that was too large or not
// read in full, a directory (whose contents were not kept), or nothing,
// because the path did not exist yet.
function preState(o) {
  const key = o.pre_reason || (o.pre_state === 'absent' ? 'absent' : o.pre_state === 'dir' ? 'dir' : 'ok');
  const dot = key === 'ok' ? 'ok' : key === 'absent' ? '' : 'warn';
  return el('span', { style: 'display:inline-flex;align-items:center;gap:7px;white-space:nowrap' },
    el('span', { class: 'dot ' + dot }), t('rollback.pre.' + key));
}

// pathCell is a path, or for a rename the old and the new one.
function pathCell(o) {
  return el('td', { class: 'detail', style: 'word-break:break-all' }, o.to_path ? [o.path, ' → ', o.to_path] : o.path);
}

// opRow is one write the session recorded. The result column is empty
// until a rollback ran; then it says what became of the row.
function opRow(o) {
  return el('tr', { 'data-op': String(o.seq) },
    el('td', { class: 'num dim tnums' }, String(o.seq)),
    el('td', { style: 'white-space:nowrap' }, t('session.op.' + o.op)),
    pathCell(o),
    el('td', {}, preState(o)),
    el('td', { class: 'dim', style: 'word-break:break-all' }, o.rollback_result || ''));
}

function opsTable(ops) {
  return el('div', { class: 'panel', style: 'overflow:auto;max-height:35vh' },
    el('table', {},
      el('thead', {}, el('tr', {},
        el('th', { class: 'num' }, t('session.col.seq')), el('th', {}, t('session.col.op')),
        el('th', {}, t('audit.col.path')), el('th', {}, t('session.col.pre')), el('th', {}, t('session.col.result')))),
      el('tbody', {},
        ops.length ? ops.map(opRow)
          : el('tr', {}, el('td', { colspan: String(OPS_COLUMNS), class: 'dim' }, t('session.ops.empty'))))));
}

// reasonText is the phrase for a plan item's reason. "modified" is the
// conflict the promise names — the file changed after the session — and
// gets its sentence; the other known reasons get a word; an error message
// from a write is shown as it came.
function reasonText(reason) {
  if (!reason) return '';
  if (reason === 'modified') return t('rollback.conflict.modified');
  return REASONS.has(reason) ? t('rollback.reason.' + reason) : reason;
}

// planItem is one line of a group: the path (old → new for a rename) and,
// when there is one, the reason it landed in that group.
function planItem(item) {
  return el('li', { style: 'word-break:break-all' },
    el('span', { class: 'detail' }, item.to_path ? [item.path, ' → ', item.to_path] : item.path),
    item.reason ? el('span', { class: 'dim' }, ' · ', reasonText(item.reason)) : null);
}

// planView draws the three groups of a plan, each with its count in the
// heading and its items below; an empty group still shows its heading with
// a zero, so the reader sees that nothing conflicts rather than wondering.
function planView(plan) {
  const g = groupPlan(plan);
  const group = (key, items, tone, heading) => el('div', { style: 'margin-bottom:12px', 'data-group': key },
    el('div', { class: 'eyebrow', style: 'margin-bottom:4px;display:inline-flex;align-items:center;gap:7px' },
      el('span', { class: 'dot ' + tone }), heading),
    items.length ? el('ul', { style: 'margin:0;padding-left:18px;font-size:13px' }, items.map(planItem)) : null);
  return el('div', {},
    group('restore', g.restore, 'ok', t('rollback.group.restore', g.restore.length)),
    group('skip', g.skip, 'warn', t('rollback.group.skip', g.skip.length)),
    group('conflict', g.conflict, 'bad', t('rollback.group.conflict', g.conflict.length)));
}

// promise is the three sentences of docs/agent-roadmap.md §4.8, under
// every preview: what a rollback is, what it leaves alone, what it covers.
function promise() {
  return el('div', { class: 'muted', style: 'font-size:12px;margin-top:14px;line-height:1.5' },
    el('div', {}, t('rollback.promise.1')),
    el('div', {}, t('rollback.promise.2')),
    el('div', {}, t('rollback.promise.3')));
}

// openRollback is the whole rollback flow for session id, and the only
// code that talks to POST /sessions/{id}/rollback. It always runs in this
// order: a dry run first, whose plan is the preview; then, on Execute, the
// typed confirmation of the short id; and only behind that answer the
// confirming post. The result is shown in the same three groups, with the
// rollback's own session offered for rolling back in turn. onDone runs
// after an executed rollback so whatever opened the flow can reload.
export async function openRollback(id, onDone) {
  const route = '/sessions/' + encodeURIComponent(id) + '/rollback';
  let plan;
  try {
    plan = await api.post(route, { dry_run: true });
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }
  const counts = planCounts(plan);
  const proceed = await new Promise((resolve) => {
    const go = el('button', { class: 'danger', 'data-action': 'execute', disabled: counts.restore === 0 }, iconEl('undo'), t('rollback.execute'));
    const cancel = el('button', {}, t('confirm.cancel'));
    const close = openPanel({
      title: t('rollback.preview.title', shortID(id)),
      danger: true,
      width: 640,
      content: el('div', { 'data-rollback': 'preview' },
        planView(plan),
        counts.restore === 0 ? el('p', { class: 'dim', style: 'font-size:13px' }, t('rollback.nothing')) : null,
        promise()),
      footer: el('div', { class: 'row', style: 'margin-top:16px' }, go, cancel),
      onEscape: () => resolve(close(false)),
    });
    go.addEventListener('click', () => resolve(close(true)));
    cancel.addEventListener('click', () => resolve(close(false)));
  });
  if (!proceed) return;
  const ok = await confirmDelete({
    title: t('rollback.confirm.title'),
    body: t('rollback.confirm.body', counts.restore),
    confirmToken: shortID(id),
    confirmLabel: t('rollback.execute'),
  });
  if (!ok) return;
  let result;
  try {
    result = await api.post(route, { confirm: true });
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }
  toast(t('rollback.done', planCounts(result).restore));
  if (onDone) onDone();
  const done = el('button', { class: 'primary' }, t('conn.close'));
  const again = result.rollback_session_id
    ? el('button', { 'data-action': 'rollback-again', onclick: () => { close(); openRollback(result.rollback_session_id, onDone); } }, iconEl('undo'), t('rollback.again'))
    : null;
  const close = openPanel({
    title: t('rollback.result.title', shortID(id)),
    width: 640,
    content: el('div', { 'data-rollback': 'result' }, planView(result), promise()),
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, again, done),
    onEscape: () => close(),
  });
  done.addEventListener('click', () => close());
}

// openSessionPanel fetches the session and opens it. onFinished, when
// given, runs after a successful finish so the list behind the panel can
// reload without waiting for the session event.
export async function openSessionPanel(id, onFinished) {
  let d;
  try {
    d = await api.get('/sessions/' + encodeURIComponent(id));
  } catch (err) {
    toast(err.message, 'bad');
    return null;
  }
  const s = d.session || {};
  const scope = scopeParts(s.scope).map((p) => t(p.key, ...p.args)).join(' · ');
  let close = null;

  async function finish() {
    try {
      await api.post('/sessions/' + encodeURIComponent(id) + '/finish', {});
      toast(t('session.finished.toast'));
      if (close) close();
      if (onFinished) onFinished();
    } catch (err) {
      toast(err.message, 'bad');
    }
  }

  const finishBtn = s.state === 'active' ? el('button', { class: 'danger', onclick: finish }, t('session.finish')) : null;
  const ops = d.ops || [];
  // Rolling back closes the panel: the session's state and every op's
  // result have changed underneath it, and the list behind reloads.
  const rollbackBtn = s.state !== 'rolled_back' && ops.length
    ? el('button', { 'data-action': 'rollback', onclick: () => openRollback(id, () => { if (close) close(); if (onFinished) onFinished(); }) }, iconEl('undo'), t('rollback.button'))
    : null;
  const done = el('button', { class: 'primary' }, t('conn.close'));
  done.addEventListener('click', () => { if (close) close(); });

  // Navigating to a file closes the panel first: the main window remounts
  // on the hash change, and a sheet left over it would cover the selection
  // the person asked to see.
  function navigate(path, go = showInFiles) {
    if (close) close();
    go(path);
  }

  const artifacts = d.artifacts || [];
  const artRows = el('tbody', {});
  function fillArtifacts() {
    fill(artRows, artifacts.length ? artifacts.map((a) => artifactRow(a, navigate))
      : el('tr', {}, el('td', { colspan: String(ARTIFACT_COLUMNS), class: 'dim' }, t('artifact.empty'))));
  }
  fillArtifacts();
  // An upload finishing is a change event on the artifact's path (or a
  // rescan); either re-asks /fs/stat for the rows, so "uploading" becomes
  // "synced" without anyone reopening the panel.
  const touched = (c) => c.rescan || artifacts.some((a) => (c.paths || []).some((p) => a.path === p || a.path.startsWith(p + '/')));
  const stopFs = artifacts.length ? onFsChange((c) => { if (touched(c)) fillArtifacts(); }) : () => {};

  const facts = el('div', { style: 'display:grid;grid-template-columns:max-content 1fr;gap:6px 14px;font-size:13px' },
    el('div', { class: 'muted' }, t('session.col.client')),
    el('div', { class: 'detail' }, (s.client || s.id || '') + (s.client_version ? ' ' + s.client_version : '')),
    el('div', { class: 'muted' }, t('session.transport')),
    el('div', { class: 'detail' }, s.transport === 'console' ? t('session.transport.console') : (s.transport || '')),
    el('div', { class: 'muted' }, t('session.col.scope')),
    el('div', { class: 'detail' }, scope),
    el('div', { class: 'muted' }, t('session.col.state')),
    el('div', { class: 'detail' }, t('session.state.' + s.state)),
    el('div', { class: 'muted' }, t('session.col.started')),
    el('div', { class: 'detail tnums' }, when(s.started_at)),
    s.finished_at ? el('div', { class: 'muted' }, t('session.finished_at')) : null,
    s.finished_at ? el('div', { class: 'detail tnums' }, when(s.finished_at)) : null,
    s.rolled_back_at ? el('div', { class: 'muted' }, t('session.rolled_back_at')) : null,
    s.rolled_back_at ? el('div', { class: 'detail tnums' }, when(s.rolled_back_at)) : null,
    el('div', { class: 'muted' }, t('session.col.writes')),
    el('div', { class: 'detail tnums' }, String(s.writes || 0)),
    s.workspace ? el('div', { class: 'muted' }, t('session.workspace')) : null,
    s.workspace ? el('div', { class: 'detail' }, s.workspace) : null,
    s.summary ? el('div', { class: 'muted' }, t('session.summary')) : null,
    s.summary ? el('div', { class: 'detail' }, s.summary) : null);

  const tail = d.audit || [];
  const content = el('div', {},
    el('div', { class: 'row', style: 'margin-bottom:10px;align-items:center;gap:10px' },
      el('span', { class: 'dim', style: 'font-size:12px;word-break:break-all' }, s.id || id),
      s.sandbox ? el('span', { class: 'chip' }, iconEl('bot'), t('session.sandbox')) : null),
    facts,
    s.workspace ? el('div', { class: 'row', style: 'margin-top:12px' },
      el('button', { onclick: () => navigate(s.workspace, openFolder) }, iconEl('folder'), t('session.openworkspace'))) : null,
    el('div', { class: 'eyebrow', style: 'margin:16px 0 6px' }, t('session.ops')),
    opsTable(ops),
    el('div', { class: 'eyebrow', style: 'margin:16px 0 6px' }, t('session.artifacts')),
    el('div', { class: 'panel', style: 'overflow:auto;max-height:35vh' },
      el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, t('audit.col.path')), el('th', { class: 'num' }, t('col.size')),
          el('th', {}, t('session.col.state')), el('th', {}))),
        artRows)),
    el('div', { class: 'eyebrow', style: 'margin:16px 0 6px' }, t('session.audit.tail')),
    el('div', { class: 'panel', style: 'overflow:auto;max-height:45vh' },
      el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, t('audit.col.time')), el('th', {}, t('audit.filter.tool')),
          el('th', {}, t('audit.col.path')), el('th', {}, t('audit.filter.result')))),
        el('tbody', {},
          tail.length ? tail.map(auditTailRow)
            : el('tr', {}, el('td', { colspan: String(TAIL_COLUMNS), class: 'dim' }, t('audit.empty')))))));

  const closePanel = openPanel({
    title: t('session.panel.title', s.client || id),
    content,
    width: 760,
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, rollbackBtn, finishBtn, done),
    onEscape: () => close(),
  });
  // Every way out — the button, Escape, a finish, a navigation — goes
  // through close, so the change subscription cannot outlive the panel.
  close = (result) => { stopFs(); return closePanel(result); };
  return close;
}
