import { api } from '/ui/api.js';
import { el, fill, iconEl, bytes, openPanel, toast } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { scopeParts } from '/ui/scope_view.js';
import { onFsChange } from '/ui/app.js';

// The session panel: one MCP session in detail — who it is, what it may
// touch, and the last fifty things it did — with the one action a person
// can take on it, finishing it. GET /sessions/<id> answers all of that in
// one round trip, so the panel opens on a single request.
//
// Everything shown here was written by the agent or its client (client
// name, tool names, paths, summary, error text). It all goes in as text
// nodes.
const TAIL_COLUMNS = 4;
const ARTIFACT_COLUMNS = 4;

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
    el('div', { class: 'detail' }, s.transport || ''),
    el('div', { class: 'muted' }, t('session.col.scope')),
    el('div', { class: 'detail' }, scope),
    el('div', { class: 'muted' }, t('session.col.state')),
    el('div', { class: 'detail' }, t('session.state.' + s.state)),
    el('div', { class: 'muted' }, t('session.col.started')),
    el('div', { class: 'detail tnums' }, when(s.started_at)),
    s.finished_at ? el('div', { class: 'muted' }, t('session.finished_at')) : null,
    s.finished_at ? el('div', { class: 'detail tnums' }, when(s.finished_at)) : null,
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
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, finishBtn, done),
    onEscape: () => close(),
  });
  // Every way out — the button, Escape, a finish, a navigation — goes
  // through close, so the change subscription cannot outlive the panel.
  close = (result) => { stopFs(); return closePanel(result); };
  return close;
}
