import { api } from '/ui/api.js';
import { el, iconEl, bytes, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { openAddDrive } from '/ui/add_drive.js';
import { onFsChange } from '/ui/app.js';
import { get, subscribe } from '/ui/store.js';

// The main window: connections on the left, the file table in the middle, an
// inspector on the right. Everything it does goes through the /fs and /accounts
// control routes — the same VFS the mount and the agent see.

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

export function renderMain(host) {
  let cwd = '/';
  let selected = null;
  const state = { remotes: [], config: null };

  const sidebar = el('div', { style: 'width:264px;flex-shrink:0;border-right:1px solid var(--border);background:var(--sidebar);display:flex;flex-direction:column' });
  const main = el('div', { style: 'flex-grow:1;display:flex;flex-direction:column;min-width:0' });
  const inspector = el('div', { style: 'width:320px;flex-shrink:0;border-left:1px solid var(--border);background:var(--sidebar);padding:18px;overflow:auto' });
  host.append(el('div', { style: 'flex-grow:1;display:flex;min-height:0' }, sidebar, main, inspector));

  const crumb = el('div', { style: 'display:flex;align-items:center;gap:6px;min-width:0;font-size:14px' });
  const rows = el('tbody');
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
  const unsubscribeHealth = subscribe(() => {
    for (const [name, dot] of dots) {
      dot.className = 'dot ' + dotClass(remoteState(name));
      dot.title = remoteStateTitle(name);
    }
  });

  async function loadAccounts() {
    try {
      const a = await api.get('/accounts');
      state.config = a;
      sidebar.replaceChildren(
        el('div', { class: 'eyebrow', style: 'padding:18px 16px 10px' }, t('nav.connections')),
        el('div', { style: 'flex-grow:1;padding:0 8px;overflow:auto' },
          (a.remotes || []).map((r) => {
            const item = el('div', {
              style: 'display:flex;align-items:center;gap:10px;padding:9px 10px;border-radius:10px;cursor:pointer;border:1px solid transparent',
              onclick: () => selectRemote(r),
            }, el('span', { style: 'width:28px;height:28px;border-radius:8px;background:#16283d;display:flex;align-items:center;justify-content:center;color:var(--accent-text)' }, iconEl('cloud')),
              el('div', { style: 'flex-grow:1;min-width:0' }, el('div', { style: 'font-weight:600' }, r.name), el('div', { class: 'dim', style: 'font-size:11px' }, r.type)),
              healthDot(r.name));
            return item;
          }),
          (a.remotes || []).length ? null : el('div', { class: 'dim', style: 'padding:12px' }, t('empty'))),
        el('div', { style: 'border-top:1px solid var(--border);padding:10px 12px' },
          el('button', { class: 'primary', style: 'width:100%', onclick: openAddDrive }, iconEl('plus'), t('action.add'))));
    } catch (e) { toast(e.message, 'bad'); }
  }

  function selectRemote(r) {
    // A remote maps to a mount prefix; browse from its first configured mount,
    // or the root if none is mounted here.
    cwd = '/';
    load();
  }

  async function load() {
    crumb.replaceChildren(iconEl('folder'),
      ...cwd.split('/').filter(Boolean).flatMap((seg, i, all) => {
        const p = '/' + all.slice(0, i + 1).join('/');
        return [el('span', { class: 'dim' }, iconEl('chevron')), el('a', { href: 'javascript:void 0', style: 'color:var(--detail);text-decoration:none', onclick: () => { cwd = p; load(); } }, seg)];
      }));
    if (cwd === '/') crumb.append(el('span', { class: 'detail' }, ' /'));
    try {
      const page = await api.get('/fs/list?path=' + encodeURIComponent(cwd) + '&limit=500');
      rows.replaceChildren(...(page.entries || []).map((e) => {
        const tr = el('tr', { onclick: () => select(e, tr) },
          el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:10px' },
            el('span', { style: 'color:' + (e.is_dir ? 'var(--accent-text)' : 'var(--muted)') }, iconEl(e.is_dir ? 'folder' : 'file')), e.name)),
          el('td', { class: 'num dim' }, e.is_dir ? '—' : bytes(e.size)),
          el('td', { class: 'detail', style: 'padding-left:20px' }, new Date(e.mtime).toLocaleDateString('zh-CN')),
          el('td', { style: 'padding-left:20px;font-size:13px' }, stateCell(e)));
        if (e.is_dir) tr.addEventListener('dblclick', () => { cwd = e.path; load(); });
        return tr;
      }));
      if (!(page.entries || []).length) rows.replaceChildren(el('tr', {}, el('td', { colspan: '4', class: 'dim' }, t('empty'))));
    } catch (e) { rows.replaceChildren(el('tr', {}, el('td', { colspan: '4' }, e.message))); }
  }

  function select(entry, tr) {
    selected = entry;
    for (const r of rows.children) r.style.background = '';
    tr.style.background = '#131c28';
    renderInspector();
  }

  function renderInspector() {
    if (!selected) { inspector.replaceChildren(el('div', { class: 'eyebrow' }, '详情'), el('p', { class: 'dim' }, '选择一个文件')); return; }
    const e = selected;
    inspector.replaceChildren(
      el('div', { class: 'eyebrow', style: 'margin-bottom:14px' }, '详情'),
      el('div', { style: 'font-weight:620;overflow-wrap:anywhere' }, e.name),
      el('div', { class: 'dim', style: 'font-size:12px;margin-bottom:16px' }, e.is_dir ? '目录' : bytes(e.size)),
      infoRow('虚拟路径', e.path),
      infoRow('本地状态', e.local_only ? t('state.pending') : e.pinned ? t('state.pinned') : e.cached >= 1 ? t('state.cached') : e.cached > 0 ? `${Math.round(e.cached * 100)}%` : t('state.remote')),
      e.availability ? infoRow('副本', `${t('avail.' + e.availability)} ${e.replicas_live}/${e.replicas_target}` + (e.degraded_reason ? `（${e.degraded_reason}）` : '')) : null,
      e.is_dir ? null : el('div', { class: 'progress' + (e.cached < 1 ? ' warn' : ''), style: 'margin:12px 0' }, el('span', { style: `width:${Math.round((e.cached || 0) * 100)}%` })),
      el('div', { class: 'row', style: 'margin-top:16px;flex-wrap:wrap' },
        e.is_dir ? null : el('button', { onclick: () => pin(e) }, iconEl('pin'), e.pinned ? t('action.unpin') : t('action.pin')),
        e.is_dir ? el('button', { onclick: () => warm(e) }, iconEl('up'), t('action.warm')) : null,
        el('button', { class: 'danger', onclick: () => remove(e) }, t('action.delete'))));
  }
  function infoRow(k, v) {
    return el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px' },
      el('span', { class: 'muted' }, k), el('span', { class: 'detail', style: 'text-align:right;overflow-wrap:anywhere' }, v));
  }

  async function pin(e) {
    try { await api.post(e.pinned ? '/cache/unpin' : '/cache/pin', { path: e.path }); toast(e.pinned ? '已取消固定' : '已固定'); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }
  async function warm(e) {
    try { const r = await api.post('/cache/warm', { path: e.path }); toast(`预热了 ${r.directories || 0} 个目录`); }
    catch (err) { toast(err.message, 'bad'); }
  }
  async function remove(e) {
    const ok = await confirmDelete({ title: '删除 ' + e.name, body: '这会同时删除远端上的文件。', confirmToken: e.name, confirmLabel: t('action.delete') });
    if (!ok) return;
    try { await api.post('/fs/delete', { path: e.path, recursive: e.is_dir, confirm: true }); toast('已删除'); selected = null; renderInspector(); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }

  async function newFolder() {
    const name = prompt('文件夹名');
    if (!name) return;
    try { await api.post('/fs/mkdir', { path: (cwd === '/' ? '' : cwd) + '/' + name }); load(); }
    catch (err) { toast(err.message, 'bad'); }
  }

  let searchTimer;
  searchBox.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(async () => {
      const q = searchBox.value.trim();
      if (!q) { load(); return; }
      try {
        const r = await api.get('/search?q=' + encodeURIComponent(q) + '&path=' + encodeURIComponent(cwd) + '&limit=100');
        rows.replaceChildren(...(r.results || []).map((hit) => el('tr', { onclick: () => { cwd = hit.path.replace(/\/[^/]*$/, '') || '/'; load(); } },
          el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:10px' }, iconEl('file'), hit.name)),
          el('td', { class: 'num dim' }, ''), el('td', { class: 'detail', style: 'padding-left:20px' }, hit.path), el('td', {}))));
        if (!r.complete) toast('可能还有更多结果');
      } catch (err) { toast(err.message, 'bad'); }
    }, 250);
  });

  main.append(
    el('div', { style: 'height:52px;flex-shrink:0;border-bottom:1px solid var(--border);display:flex;align-items:center;gap:12px;padding:0 18px' },
      crumb, el('div', { class: 'grow' }), searchBox,
      el('button', { onclick: newFolder }, iconEl('plus')),
      el('button', { onclick: load }, iconEl('refresh'))),
    el('div', { style: 'flex-grow:1;overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, t('col.name')), el('th', { class: 'num' }, t('col.size')),
        el('th', { style: 'padding-left:20px' }, t('col.modified')), el('th', { style: 'padding-left:20px' }, t('col.state')))), rows)));


  const off = onFsChange((c) => { if (c.rescan || (c.paths || []).some((p) => p === cwd || p.startsWith(cwd + '/'))) load(); });
  loadAccounts();
  load();
  renderInspector();
  return () => { off(); unsubscribeHealth(); clearTimeout(searchTimer); };
}
