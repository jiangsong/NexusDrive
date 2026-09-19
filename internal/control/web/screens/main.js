import { api } from '/ui/api.js';
import { el, fill, iconEl, bytes, toast, confirmDelete, promptText, showPanel, moreRow, copyBtn } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode, latestOnly, coalesce } from '/ui/paged.js';
import { linkExpiry } from '/ui/expiry.js';
import { openAddDrive } from '/ui/add_drive.js';
import { openConnection } from '/ui/connection.js';
import { onFsChange } from '/ui/app.js';
import { mountNameSearch } from '/ui/name_search.js';
import { get, subscribe } from '/ui/store.js';
import { workspaceMark, sessionDirOf } from '/ui/workspace_view.js';
import { openSessionPanel } from '/ui/session_panel.js';
import { readSearchMode, writeSearchMode, runContentSearch, contentSearchHeader } from '/ui/content_search.js';
import { openExtractedText } from '/ui/extracted_text.js';
import { renderIndexInfo } from '/ui/index_inspector.js';
import { openSendToAgent } from '/ui/send_to_agent.js';
import { mountProvenance } from '/ui/provenance.js';
import { openHistoryPanel } from '/ui/history_panel.js';
import { decorateHeat } from '/ui/heat_dots.js';
import { mountAgentTouch } from '/ui/agent_touch.js';

// The main window: connections on the left, the file table in the middle, an
// inspector on the right. Everything it does goes through the /fs and /accounts
// control routes — the same VFS the mount and the agent see.

// validName is what both the rename and the new-folder inputs mean by a
// name: one component, no separator. "a/b" would put the result somewhere
// other than the directory on screen, which is the kind of surprise a person
// only notices later.
function validName(name) {
  if (!name.includes('/')) return true;
  toast(t('rename.noslash'), 'bad');
  return false;
}

// PREVIEW_BYTES is what the inspector asks for. The daemon caps a preview at
// 1 MiB; a first screenful is what a person is actually looking at.
const PREVIEW_BYTES = 8192;

function stateCell(entry) {
  if (entry.is_dir) return el('span', { class: 'dim' }, el('span', { class: 'dot ok' }), ' ' + t('state.dir'));
  if (entry.local_only) return el('span', { style: 'color:var(--warn-text)' }, iconEl('up'), ' ' + t('state.pending'));
  if (entry.availability === 'unavailable') return el('span', { style: 'color:var(--danger-text)', title: entry.degraded_reason || '' }, el('span', { class: 'dot bad' }), ' ' + t('avail.unavailable'));
  if (entry.availability === 'degraded') return el('span', { style: 'color:var(--warn-text)', title: entry.degraded_reason || '' }, el('span', { class: 'dot warn' }), ` ${t('avail.degraded')} ${entry.replicas_live}/${entry.replicas_target}`);
  if (entry.pinned) return el('span', { style: 'color:var(--accent-text)' }, iconEl('pin'), ' ' + t('state.pinned'));
  if (entry.cached >= 1) return el('span', { class: 'dim' }, el('span', { class: 'dot ok' }), ' ' + t('state.cached'));
  if (entry.cached > 0) return el('span', { style: 'color:var(--warn-text)' }, `${t('state.partial')} ${Math.round(entry.cached * 100)}%`);
  return el('span', { class: 'dim' }, el('span', { class: 'dot' }), ' ' + t('state.remote'));
}

// workspaceRoot is the agent workspace the daemon reported on its last
// status tick, or '' before the first one — in which case no row is marked
// until the next load, which is the honest answer.
function workspaceRoot() {
  const s = get().status;
  return s && s.agent ? s.agent.workspace || '' : '';
}

// indexEnabled is whether the daemon has a content index, from its last
// status tick. The "content" search segment and the inspector's index line
// exist only while it does.
function indexEnabled() {
  const status = get().status;
  return !!(status && status.index && status.index.enabled);
}

// workspaceMarkEl is the small bot beside a workspace or session directory.
// It carries its meaning as text for assistive technology and as a title
// for everyone else; a bare icon would be a glyph nobody could look up.
function workspaceMarkEl(e) {
  const mark = e.is_dir ? workspaceMark(e.path, workspaceRoot()) : '';
  return mark ? el('span', { class: 'dim', title: t('workspace.mark.' + mark), 'aria-label': t('workspace.mark.' + mark) }, iconEl('bot')) : null;
}

export function renderMain(host) {
  // A deep link opens a directory (#/connections?dir=/a) or the parent of
  // an entry with that entry selected (#/connections?path=/a/b); the
  // session panel's "show in files" and "open workspace folder" land here.
  const link = new URLSearchParams(location.hash.split('?')[1] || '');
  let wanted = link.get('path') || '';
  let cwd = link.get('dir') || (wanted ? wanted.replace(/\/[^/]*$/, '') || '/' : '/');
  let linkShareable = true;
  let selected = null;
  let selectedRow = null;
  // True once the reader has asked for more than the first page of this
  // directory.
  let paged = false;
  // Only the newest load paints. Two change events used to start two loads
  // that each cleared the table and then each appended their page: every
  // row drawn twice, three times, for as long as a copy ran.
  const loads = latestOnly();
  const state = { remotes: [], config: null };

  const sidebar = el('div', { style: 'width:264px;flex-shrink:0;border-right:1px solid var(--border);background:var(--sidebar);display:flex;flex-direction:column' });
  const main = el('div', { style: 'flex-grow:1;display:flex;flex-direction:column;min-width:0' });
  const inspector = el('div', { style: 'width:320px;flex-shrink:0;border-left:1px solid var(--border);background:var(--sidebar);padding:18px;overflow:auto' });
  host.append(el('div', { style: 'flex-grow:1;display:flex;min-height:0' }, sidebar, main, inspector));

  const crumb = el('div', { style: 'display:flex;align-items:center;gap:6px;min-width:0;font-size:14px' });
  const rows = el('tbody');
  // The directory's header; the search module swaps its sortable one in.
  const thead = el('thead');
  const browseHeader = el('tr', {}, el('th', {}, t('col.name')), el('th', { class: 'num' }, t('col.size')),
    el('th', { style: 'padding-left:20px' }, t('col.modified')), el('th', { style: 'padding-left:20px' }, t('col.state')));
  const searchBox = el('input', { type: 'search', placeholder: t('search.placeholder'), style: 'width:220px' });

  // The dot beside each connection is its reachability from /status: the
  // daemon marks a remote down from the calls it actually makes, so a drive
  // whose API stopped answering shows red here without anyone restarting.
  const dots = new Map();
  function healthDot(name) {
    const dot = el('span', { class: 'dot ' + dotClass(remoteState(name)), title: remoteStateTitle(name) });
    dots.set(name, dot);
    return dot;
  }
  function remoteOf(name) {
    return ((get().status || {}).remotes || []).find((r) => r.remote === name);
  }
  function remoteState(name) {
    const r = remoteOf(name);
    return r ? r.state : 'up';
  }
  function remoteStateTitle(name) {
    const r = remoteOf(name);
    if (!r) return t('health.up');
    const base = t('health.' + (r.state || 'up'));
    return r.last_error ? base + ': ' + r.last_error : base;
  }
  function dotClass(state) {
    switch (state) {
      case 'up': return 'ok';
      case 'degraded': return 'warn';
      case 'down': case 'out': return 'bad';
      default: return '';
    }
  }
  // markedWith is the workspace the table was last drawn with. On a cold
  // page the first directory can land before the first status tick, so the
  // rows would carry no workspace mark; the tick that brings the workspace
  // redraws them once. The workspace does not change while the daemon runs,
  // so this is one extra list per page, not one per tick.
  let markedWith = '';
  // indexWas is what the search segments and the inspector were last drawn
  // with; the tick that turns the index on or off redraws both (a deep link
  // selects its entry before the first tick), and reruns a search that is
  // on screen so it is answered in the mode the box now shows.
  let indexWas = indexEnabled();
  const unsubscribeHealth = subscribe(() => {
    for (const [name, dot] of dots) {
      dot.className = 'dot ' + dotClass(remoteState(name));
      dot.title = remoteStateTitle(name);
    }
    if (!paged && workspaceRoot() !== markedWith) load();
    if (indexEnabled() !== indexWas) {
      indexWas = indexEnabled();
      search.refresh();
      if (searchBox.value.trim()) search.run();
      if (selected) renderInspector();
    }
  });

  async function loadAccounts() {
    try {
      const a = await api.get('/accounts');
      state.config = a;
      // The sidebar is rebuilt on the next line, so every dot in the map is
      // about to be detached. Keeping them would leave the health subscriber
      // writing to nodes nobody can see, forever — and clearing them *before*
      // the fetch was the mirror of that fault: a failed /accounts left the
      // old sidebar on screen with its dots orphaned from the map, so health
      // stopped moving there until some later load happened to succeed.
      dots.clear();
      fill(sidebar,
        el('div', { class: 'eyebrow', style: 'padding:18px 16px 10px' }, t('nav.connections')),
        el('div', { style: 'flex-grow:1;padding:0 8px;overflow:auto' },
          (a.remotes || []).map((r) => {
            const item = el('div', {
              style: 'display:flex;align-items:center;gap:10px;padding:9px 10px;border-radius:10px;cursor:pointer;border:1px solid transparent',
              // Clicking a connection opens its settings: proxy, rate limits,
              // extra fields, the reachability probe, and the mounts that point
              // at it. Browsing is what the file table on the right is for.
              onclick: () => openConnection(r.name, { onClose: loadAccounts }),
            }, el('span', { style: 'width:28px;height:28px;border-radius:8px;background:#16283d;display:flex;align-items:center;justify-content:center;color:var(--accent-text)' }, iconEl('cloud')),
              el('div', { style: 'flex-grow:1;min-width:0' }, el('div', { style: 'font-weight:600' }, r.name), el('div', { class: 'dim', style: 'font-size:11px' }, r.type)),
              healthDot(r.name),
              el('button', {
                class: 'icon-btn remove', title: t('conn.remove'), 'aria-label': t('conn.remove') + ' ' + r.name,
                onclick: (ev) => { ev.stopPropagation(); removeRemote(r.name); },
              }, iconEl('trash')));
            item.className = 'conn';
            return item;
          }),
          // With nothing configured yet, the useful offer is the guided flow,
          // not the word "empty": adding four drives one dialog at a time is
          // exactly what it exists to do.
          (a.remotes || []).length ? null : el('div', { style: 'padding:12px;display:grid;gap:9px' },
            el('div', { class: 'dim' }, t('pool.none.title')),
            el('a', { class: 'btn primary', href: '#/setup' }, t('setup.start')))),
        el('div', { style: 'border-top:1px solid var(--border);padding:10px 12px' },
          el('button', { class: 'primary', style: 'width:100%', onclick: () => openAddDrive({ onDone: loadAccounts }) }, iconEl('plus'), t('action.add'))));
    } catch (e) { toast(e.message, 'bad'); }
  }

  // Deleting a connection edits the configuration; the daemon keeps serving
  // the layout it started with until it restarts. config.RemoveRemote refuses
  // while a mount or storage pool still points at the drive. Read both here so
  // the reader sees the safe next step in their language before being asked to
  // type a destructive confirmation. The server repeats these checks inside
  // the atomic config edit; this preflight is only the friendlier fast path.
  async function removeRemote(name) {
    let mounted = [];
    let pooled = [];
    try {
      const [m, p] = await Promise.all([
        api.get('/mounts'),
        api.get('/pool/status').catch(() => ({ pools: [] })),
      ]);
      mounted = (m.mounts || []).filter((x) => x.remote === name).map((x) => x.path + x.prefix);
      pooled = (p.pools || []).filter((x) => (x.members || []).some((member) => member.remote === name && member.pending_restart !== 'remove')).map((x) => x.name);
    } catch (e) { toast(e.message, 'bad'); return; }
    if (mounted.length) { toast(t('conn.remove.mounted', name, mounted.join(', ')), 'bad'); return; }
    if (pooled.length) { toast(t('conn.remove.pooled', name, pooled.join(', ')), 'bad'); return; }
    const ok = await confirmDelete({
      title: t('conn.remove.title', name), body: t('conn.remove.body'),
      confirmToken: name, confirmLabel: t('action.delete'),
    });
    if (!ok) return;
    try {
      await api.del('/accounts/' + encodeURIComponent(name) + '?confirm=true');
      toast(t('toast.conn.removed', name));
      loadAccounts();
    } catch (e) { toast(e.message, 'bad'); }
  }

  // A directory arrives one page at a time. The daemon returns next_cursor
  // for exactly this, and dropping it showed the first 500 names as if they
  // were all of them — data that is missing without looking missing.
  async function load(cursor) {
    // Every refresh path calls this, and one of them is an event handler.
    // A cursor is a string the daemon issued; anything else means "the first
    // page", never "encode this object into &cursor=".
    cursor = pageCursor(cursor);
    const ticket = loads.take();
    // True once the reader has asked for more than the first page: a change
    // event must not then reload the directory from the top and take the
    // pages they walked to with it.
    paged = !!cursor;
    // A continuation appends to what is already on screen; only a fresh load
    // clears the table and redraws the breadcrumb, which cannot have changed.
    if (!cursor) {
      markedWith = workspaceRoot();
      fill(rows);
      fill(thead, browseHeader);
      fill(crumb, iconEl('folder'),
        ...cwd.split('/').filter(Boolean).flatMap((seg, i, all) => {
          const p = '/' + all.slice(0, i + 1).join('/');
          return [el('span', { class: 'dim' }, iconEl('chevron')), el('a', { href: 'javascript:void 0', style: 'color:var(--detail);text-decoration:none', onclick: () => { cwd = p; load(); } }, seg)];
        }));
      if (cwd === '/') crumb.append(el('span', { class: 'detail' }, ' /'));
    }
    try {
      const page = await api.get('/fs/list?path=' + encodeURIComponent(cwd) + '&limit=500'
        + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      if (!loads.current(ticket)) return;
      // Whether this directory's remote hands out download links at all.
      // Drive and Box do not (private bytes are served only with the
      // account's credential), so the inspector leaves the button out
      // instead of offering one every click refuses. Absent from an older
      // daemon's reply means "unknown", which keeps the button.
      linkShareable = page.link_shareable !== false;
      rows.append(...(page.entries || []).map((e) => {
        const tr = el('tr', { 'data-path': e.path, onclick: () => select(e, tr) },
          el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:10px' },
            el('span', { style: 'color:' + (e.is_dir ? 'var(--accent-text)' : 'var(--muted)') }, iconEl(e.is_dir ? 'folder' : 'file')), e.name, workspaceMarkEl(e))),
          el('td', { class: 'num dim' }, e.is_dir ? '—' : bytes(e.size)),
          el('td', { class: 'detail', style: 'padding-left:20px' }, new Date(e.mtime).toLocaleDateString(locale())),
          el('td', { style: 'padding-left:20px;font-size:13px' }, stateCell(e)));
        if (e.is_dir) tr.addEventListener('dblclick', () => { cwd = e.path; load(); });
        if (wanted && e.path === wanted) { wanted = ''; select(e, tr); }
        return tr;
      }));
      if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: '4', class: 'dim' }, t('empty'))));
      const next = page.next_cursor;
      if (next) rows.append(moreRow(4, () => load(next)));
      // The read-heat dots land after the list; heat_dots.js asks once per
      // directory and leaves the list alone when there is no heat table.
      decorateHeat(rows, cwd, { api });
    } catch (e) {
      if (!loads.current(ticket)) return;
      // A failed continuation must not take the pages already on screen with
      // it: the rows being read cost nothing to keep.
      const failed = el('tr', {}, el('td', { colspan: '4' }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  function select(entry, tr) {
    selected = entry;
    if (selectedRow) selectedRow.style.background = '';
    selectedRow = tr;
    tr.style.background = '#131c28';
    renderInspector();
  }

  function renderInspector() {
    if (!selected) { fill(inspector, el('div', { class: 'eyebrow' }, t('inspector.title')), el('p', { class: 'dim' }, t('inspector.empty'))); return; }
    const e = selected;
    // The index line arrives after the rest: index_inspector.js asks the
    // daemon and fills the host, or leaves it empty when there is no index.
    const indexHost = el('div', {});
    if (indexEnabled()) renderIndexInfo(e, indexHost);
    // So does the "modified by an agent" marker: agent_touch.js asks which
    // session wrote the file lately and fills the host, or leaves it empty.
    const touchHost = el('div', {});
    if (!e.is_dir) mountAgentTouch(touchHost, e.path, { api, openSession: openSessionPanel });
    // And the provenance rows: who last changed it and how often it was
    // read; provenance.js asks the change record and the heat table.
    const provHost = el('div', {});
    mountProvenance(provHost, e.path, { api, openSession: openSessionPanel, openHistory: (p) => openHistoryPanel(p, { openSession: openSessionPanel }) });
    fill(inspector,
      el('div', { class: 'eyebrow', style: 'margin-bottom:14px' }, t('inspector.title')),
      el('div', { style: 'font-weight:620;overflow-wrap:anywhere' }, e.name),
      el('div', { class: 'dim', style: 'font-size:12px;margin-bottom:16px' }, e.is_dir ? t('inspector.dir') : bytes(e.size)),
      infoRow(t('inspector.path'), e.path),
      infoRow(t('col.state'), e.local_only ? t('state.pending') : e.pinned ? t('state.pinned') : e.cached >= 1 ? t('state.cached') : e.cached > 0 ? `${Math.round(e.cached * 100)}%` : t('state.remote')),
      e.availability ? infoRow(t('inspector.replicas'), e.degraded_reason
        ? t('inspector.replicas.reason', t('avail.' + e.availability), e.replicas_live, e.replicas_target, e.degraded_reason)
        : `${t('avail.' + e.availability)} ${e.replicas_live}/${e.replicas_target}`) : null,
      indexHost,
      touchHost,
      provHost,
      e.is_dir ? null : el('div', { class: 'progress' + (e.cached < 1 ? ' warn' : ''), style: 'margin:12px 0' }, el('span', { style: `width:${Math.round((e.cached || 0) * 100)}%` })),
      el('div', { class: 'row', style: 'margin-top:16px;flex-wrap:wrap' },
        e.is_dir ? null : el('button', { onclick: () => pin(e) }, iconEl('pin'), e.pinned ? t('action.unpin') : t('action.pin')),
        e.is_dir ? el('button', { onclick: () => warm(e) }, iconEl('up'), t('action.warm')) : null,
        e.is_dir ? el('button', { onclick: () => warmAll(e) }, iconEl('layers'), t('action.warm.all')) : null,
        el('button', { onclick: () => rename(e) }, t('action.rename')),
        e.is_dir ? null : el('button', { onclick: () => preview(e) }, t('action.preview')),
        e.is_dir || !linkShareable ? null : el('button', { onclick: () => downloadLink(e) }, t('action.link')),
        sessionDirOf(e.path, workspaceRoot()) ? el('button', { onclick: () => fromSession(e) }, iconEl('bot'), t('inspector.fromsession') + ' ' + sessionDirOf(e.path, workspaceRoot()).split('/').pop()) : null,
        // Files and directories alike: the prompt tells a directory to list itself first.
        el('button', { onclick: () => openSendToAgent({ path: e.path }) }, iconEl('bot'), t('action.sendtoagent')),
        // The render page holds the sharing surface: the console link to
        // copy and the public link to create (ui-plan G8-2).
        e.is_dir ? null : el('a', { 'data-open-page': e.path, class: 'btn', href: '#/fs/' + e.path.split('/').filter(Boolean).map(encodeURIComponent).join('/') }, iconEl('globe'), t('action.openpage')),
        el('button', { class: 'danger', onclick: () => remove(e) }, t('action.delete'))));
  }
  // fromSession asks the daemon which session owns this path rather than
  // parsing the directory name: the name is a convention, the audit trail
  // is the record.
  async function fromSession(e) {
    try {
      const r = await api.get('/sessions?path=' + encodeURIComponent(e.path) + '&limit=1');
      const s = (r.sessions || [])[0];
      if (s) openSessionPanel(s.id);
      else toast(t('inspector.fromsession.none'));
    } catch (err) { toast(err.message, 'bad'); }
  }
  function infoRow(k, v) {
    return el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px' },
      el('span', { class: 'muted' }, k), el('span', { class: 'detail', style: 'text-align:right;overflow-wrap:anywhere' }, v));
  }

  async function pin(e) {
    try { await api.post(e.pinned ? '/cache/unpin' : '/cache/pin', { path: e.path }); toast(e.pinned ? t('toast.unpinned') : t('toast.pinned')); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }
  async function warm(e) {
    try { const r = await api.post('/cache/warm', { path: e.path }); toast(t('toast.warmed', r.directories || 0)); }
    catch (err) { toast(err.message, 'bad'); }
  }
  // Listing a whole subtree is one provider call per directory under it,
  // with no depth to stop at; on an unofficial API that is the traffic
  // that gets an account flagged. It goes through the same typed
  // confirmation as "index the whole tree" and sends the confirm flag the
  // daemon insists on for depth -1.
  async function warmAll(e) {
    const ok = await confirmDelete({ title: t('confirm.warm.title'), body: t('confirm.warm.body'), confirmToken: 'warm', confirmLabel: t('action.warm.all'), danger: false });
    if (!ok) return;
    try { const r = await api.post('/cache/warm', { path: e.path, depth: -1, confirm: true }); toast(t('toast.warmed', r.directories || 0)); }
    catch (err) { toast(err.message, 'bad'); }
  }
  async function remove(e) {
    const ok = await confirmDelete({ title: t('confirm.delete.title', e.name), body: t('confirm.delete.body'), confirmToken: e.name, confirmLabel: t('action.delete') });
    if (!ok) return;
    try { await api.post('/fs/delete', { path: e.path, recursive: e.is_dir, confirm: true }); toast(t('toast.deleted')); selected = null; renderInspector(); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }

  // Rename moves within the same directory. Moving across directories is a
  // drag of a path the table does not have yet; /fs/rename takes both, so the
  // day the table grows one this call does not change.
  async function rename(e) {
    const next = await promptText({
      title: t('rename.title', e.name), label: t('rename.label'),
      initial: e.name, confirmLabel: t('action.rename'),
    });
    if (!next || next === e.name) return;
    if (!validName(next)) return;
    const parent = e.path.replace(/\/[^/]*$/, '');
    try {
      await api.post('/fs/rename', { from: e.path, to: (parent || '') + '/' + next });
      toast(t('toast.renamed', next));
      selected = null;
      renderInspector();
      load();
    } catch (err) { toast(err.message, 'bad'); }
  }

  // Preview asks for the first bytes only; the daemon caps the range anyway.
  // Bytes that are not text are named as such rather than painted into the
  // document as replacement characters.
  async function preview(e) {
    let text;
    try {
      text = await api.get('/fs/preview?path=' + encodeURIComponent(e.path) + '&length=' + PREVIEW_BYTES);
    } catch (err) { toast(err.message, 'bad'); return; }
    const binary = /[\u0000-\u0008\u000e-\u001f\ufffd]/.test(text);
    await showPanel({
      title: e.name,
      content: binary
        ? el('div', { class: 'dim', style: 'font-size:12.5px' }, t('preview.binary'))
        : el('pre', {
          style: 'margin:0;max-height:50vh;overflow:auto;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,monospace;font-size:12.5px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:10px',
        }, text || t('preview.empty')),
    });
  }

  // A download link is signed and short-lived. It is copied on request rather
  // than rendered into a page that may sit open for an hour, which is how a
  // link becomes a stale one that fails with no explanation.
  async function downloadLink(e) {
    let link;
    try {
      link = await api.get('/fs/download-url?path=' + encodeURIComponent(e.path));
    } catch (err) { toast(err.message, 'bad'); return; }
    // expires_at is always present — see expiry.js — so the question is
    // whether it names a real instant, not whether the field came back.
    const expires = linkExpiry(link.expires_at);
    await showPanel({
      title: t('link.title', e.name),
      content: el('div', { style: 'display:grid;gap:9px' },
        el('div', { class: 'dim', style: 'font-size:12px' },
          expires ? t('link.expires', new Date(expires).toLocaleString(locale())) : t('link.noexpiry')),
        el('div', { style: 'font-family:ui-monospace,monospace;font-size:12px;word-break:break-all;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:9px' }, link.url),
        el('div', { class: 'row' }, copyBtn(link.url)),
        link.headers && Object.keys(link.headers).length
          ? el('div', { class: 'dim', style: 'font-size:11.5px' }, t('link.headers'))
          : null),
    });
  }

  async function newFolder() {
    const name = await promptText({ title: t('newfolder.title'), label: t('newfolder.label'), confirmLabel: t('newfolder.create') });
    if (!name) return;
    if (!validName(name)) return;
    try { await api.post('/fs/mkdir', { path: (cwd === '/' ? '' : cwd) + '/' + name }); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }

  // Scope, shortcuts, request, result rows and the coverage line live in
  // name_search.js; this screen only says what clear, render, open and
  // select do here. Opening goes to the folder and selects the row by data-path.
  // The "keyword" and "semantic" segments are extra modes of that control:
  // offered only while the daemon reports index.enabled, remembered by
  // content_search.js so a reload keeps it, and answered by /index/search
  // (mode=keyword and mode=hybrid) with the extracted text panel behind
  // each row. Deselecting one mode clears the stored choice only if it was
  // that mode's, so selecting the other one is not undone a step later.
  const contentHeader = contentSearchHeader();
  const indexMode = (id, label) => ({
    id, label,
    active: () => readSearchMode() === id,
    select: (on) => { if (on) writeSearchMode(id); else if (readSearchMode() === id) writeSearchMode('name'); },
    run: (q) => runContentSearch({
      rows, query: q, mode: id,
      onSearch: () => fill(thead, contentHeader),
      onOpen: (hit) => openExtractedText(hit.path, hit.start_off, hit.end_off),
    }),
    // Offered only while the daemon has an index.
    when: () => indexEnabled(),
  });
  const search = mountNameSearch({
    extraModes: [
      indexMode('content', t('search.mode.content')),
      indexMode('semantic', t('search.mode.semantic')),
    ],
    searchBox, rows, getCwd: () => cwd,
    onClear: () => load(),
    onSearch: () => fill(thead, search.header),
    onOpen: (hit) => { cwd = hit.path.replace(/\/[^/]*$/, '') || '/'; load().then(() => selectPath(hit.path)); },
    onSelect: (hit, tr) => select({ name: hit.name, path: hit.path, size: hit.size, mtime: hit.mtime, is_dir: hit.kind === 'dir', cached: hit.cached ? 1 : 0 }, tr),
  });
  function selectPath(p) { const tr = rows.querySelector('tr[data-path="' + CSS.escape(p) + '"]'); if (tr) tr.click(); }

  main.append(
    el('div', { style: 'height:52px;flex-shrink:0;border-bottom:1px solid var(--border);display:flex;align-items:center;gap:12px;padding:0 18px' },
      crumb, el('div', { class: 'grow' }), search.scopeControl, searchBox,
      el('button', { onclick: newFolder, 'aria-label': t('action.newfolder'), title: t('action.newfolder') }, iconEl('plus')),
      el('button', { onclick: () => load(), 'aria-label': t('action.refresh'), title: t('action.refresh') }, iconEl('refresh'))),
    search.filterBar,
    search.statusLine,
    el('div', { style: 'flex-grow:1;overflow:auto' }, el('table', {}, thead, rows)));

  // An upload finishing is not a reason to throw away the page the reader
  // walked to: reloading from the top is what a directory of more than 500
  // entries looked like snapping back to its first page mid-read.
  // A copy into this directory raises one change event per file, and the
  // subtree test below matches every one of them. Reloading per event was
  // hundreds of /fs/list calls for a listing that changed once; a burst is
  // now one reload, and a copy that runs for minutes refreshes every 400 ms.
  const reload = coalesce(() => { if (!paged) load(); }, 400);
  const off = onFsChange((c) => {
    if (paged) return;
    if (c.rescan || (c.paths || []).some((p) => p === cwd || p.startsWith(cwd + '/'))) reload();
  });
  loadAccounts();
  renderInspector();
  // A deep link (#/connections?q=plan) searches after the directory is in;
  // &mode=content (keyword) or &mode=semantic asks the content index, once
  // the status tick that says there is one has arrived.
  searchBox.value = link.get('q') || '';
  if (link.get('mode') === 'content' || link.get('mode') === 'semantic') { writeSearchMode(link.get('mode')); search.refresh(); }
  load().then(() => searchBox.value && search.run());
  return () => { off(); reload.cancel(); unsubscribeHealth(); search.dispose(); };
}
