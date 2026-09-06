// The storage pool screen: the members with their health and space, how
// well replicated the files are, what repair is doing, and what a person
// has to decide. Every action goes through the same guarded endpoints the
// CLI uses; configuration edits say "restart required" because that is
// what they are.
import { api } from '/ui/api.js';
import { el, bytes, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { openAddDrive } from '/ui/add_drive.js';

export function renderPool(host) {
  const body = el('div');
  let timer = null;

  function dotClass(state) {
    switch (state) {
      case 'up': return 'ok';
      case 'degraded': return 'warn';
      case 'down': case 'out': return 'bad';
      default: return '';
    }
  }

  function bar(used, total) {
    const pct = total > 0 ? Math.min(100, Math.round(used / total * 100)) : 0;
    return el('div', { style: 'height:6px;border-radius:3px;background:#16283d;overflow:hidden;margin-top:6px' },
      el('div', { style: `height:100%;width:${pct}%;background:${pct > 90 ? 'var(--bad)' : 'var(--accent-text)'}` }));
  }

  function card(label, value, sub) {
    return el('div', { class: 'panel pad' },
      el('div', { class: 'muted', style: 'font-size:13px;margin-bottom:9px' }, label),
      el('div', { style: 'font-size:28px;font-weight:720;letter-spacing:-.03em' }, value),
      el('div', { class: 'detail', style: 'font-size:13px;margin-top:12px' }, sub));
  }

  async function act(path, payload, done) {
    try { const r = await api.post(path, payload); toast(done(r)); load(); }
    catch (e) { toast(e.message, 'bad'); }
  }

  function memberRow(p, m) {
    const space = m.total ? `${bytes(m.used)} / ${bytes(m.total)}` : t('pool.space.unknown');
    const actions = el('div', { class: 'row', style: 'justify-content:flex-end;gap:6px' });
    if (m.state === 'draining') {
      actions.append(el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/members/state', { pool: p.name, remote: m.remote, state: 'enabled' }, () => t('pool.enabled')) }, t('pool.action.enable')));
      actions.append(el('button', { class: 'danger', style: 'padding:6px 10px', onclick: () => confirmDelete(t('pool.remove.q').replace('%s', m.remote), () =>
        act('/pool/members/remove', { pool: p.name, remote: m.remote, confirm: true }, () => t('pool.restart'))) }, t('pool.action.remove')));
    } else if (m.state === 'disabled') {
      actions.append(el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/members/state', { pool: p.name, remote: m.remote, state: 'enabled' }, () => t('pool.enabled')) }, t('pool.action.enable')));
    } else {
      actions.append(el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/members/state', { pool: p.name, remote: m.remote, state: 'disabled' }, () => t('pool.disabled')) }, t('pool.action.disable')));
      actions.append(el('button', { class: 'danger', style: 'padding:6px 10px', onclick: () => confirmDelete(t('pool.drain.q').replace('%s', m.remote), () =>
        act('/pool/members/drain', { pool: p.name, remote: m.remote, confirm: true }, () => t('pool.draining'))) }, t('pool.action.drain')));
    }
    return el('tr', {},
      el('td', {}, el('div', { style: 'display:flex;align-items:center;gap:8px' }, el('span', { class: 'dot ' + dotClass(m.state), title: m.last_error || '' }), el('span', { style: 'font-weight:600' }, m.remote),
        el('span', { class: 'dim', style: 'font-size:11px' }, m.root && m.root !== '/' ? m.root : ''))),
      el('td', {}, el('div', {}, t('health.' + (m.state || 'up'))), m.last_error ? el('div', { class: 'dim', style: 'font-size:11px;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap' }, m.last_error) : null),
      el('td', {}, el('div', {}, space), m.total ? bar(m.used, m.total) : null),
      el('td', { style: 'text-align:right' }, String(m.files)),
      el('td', { style: 'text-align:right' }, m.pending_ops ? String(m.pending_ops) : '—'),
      el('td', {}, actions));
  }

  function poolSection(p, res) {
    const target = p.target_capped ? `${p.target} (${t('pool.capped')})` : String(p.target);
    const health = p.unavailable ? 'bad' : (p.under_replicated ? 'warn' : 'ok');
    const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px;padding:0 20px' },
      card(t('pool.space'), p.total ? bytes(p.free) : '—', p.total ? `${t('pool.space.used')} ${bytes(p.used)} / ${bytes(p.total)}` : t('pool.space.unknown')),
      card(t('pool.files'), p.files.toLocaleString(), `${t('pool.replicas')} ${p.replicas}，${t('pool.target')} ${target}`),
      card(t('pool.health'), el('span', { class: 'dot ' + health, style: 'display:inline-block;width:14px;height:14px;vertical-align:middle;margin-right:8px' }),
        `${p.under_replicated} ${t('pool.under')}，${p.unavailable} ${t('pool.unavail')}`),
      card(t('pool.repair'), String(p.repair.queued), `${p.repair.blocked} ${t('pool.waiting')}，${t('pool.holds')} ${bytes(p.holds_bytes)}`));
    const notices = (p.notices || []).map((n) => el('div', { class: 'detail', style: 'padding:8px 12px;border:1px solid var(--warn);border-radius:8px;margin:0 20px 10px' }, n));
    const table = el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
      el('th', {}, t('pool.col.member')), el('th', {}, t('pool.col.state')), el('th', {}, t('pool.col.space')), el('th', { style: 'text-align:right' }, t('pool.col.files')),
      el('th', { style: 'text-align:right' }, t('pool.col.ops')), el('th', {}, ''))),
      el('tbody', {}, ...p.members.map((m) => memberRow(p, m)))));
    const addRow = el('div', { class: 'row', style: 'margin-top:10px;gap:8px;align-items:center' });
    if (res.candidates && res.candidates.length) {
      const sel = el('select', {}, ...res.candidates.map((c) => el('option', { value: c }, c)));
      addRow.append(el('span', { class: 'dim', style: 'font-size:12px' }, t('pool.add.existing')), sel,
        el('button', { onclick: () => act('/pool/members', { pool: p.name, remote: sel.value }, () => t('pool.restart')) }, t('pool.action.add')));
    }
    addRow.append(el('button', { class: 'primary', onclick: () => openAddDrive({ pool: p.name }) }, t('pool.action.newdrive')));
    return el('div', {},
      el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
        el('div', {}, el('div', { class: 'eyebrow' }, t('pool.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, p.name)),
        el('div', { class: 'row', style: 'gap:8px' },
          el('button', { onclick: () => act('/pool/repair', { pool: p.name }, (r) => `${t('pool.repaired')} ${r.made || 0}`) }, t('pool.action.repair')),
          el('button', { onclick: () => act('/pool/scrub', { pool: p.name }, (r) => `${t('pool.scrubbed')} ${r.looked || 0}`) }, t('pool.action.scrub')))),
      cards,
      el('div', { style: 'padding:14px 0 0' }, ...notices),
      el('div', { style: 'padding:20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('pool.members')), table, addRow),
      divergences(p));
  }

  function divergences(p) {
    const rows = el('tbody', {}, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('loading'))));
    api.get('/pool/divergences').then((r) => {
      const list = (r.divergences || []).filter((d) => d.pool === p.name);
      if (!list.length) { rows.replaceChildren(el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('pool.div.none')))); return; }
      rows.replaceChildren(...list.map((d) => el('tr', {},
        el('td', {}, d.path), el('td', {}, d.member), el('td', {}, d.kind), el('td', { class: 'detail' }, d.detail),
        el('td', { style: 'text-align:right;white-space:nowrap' },
          el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/divergences', { pool: p.name, path: d.path, member: d.member, kind: d.kind, action: 'relist' }, () => t('pool.div.relisted')) }, t('pool.div.relist')),
          ' ',
          el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/divergences', { pool: p.name, path: d.path, member: d.member, kind: d.kind, action: 'clear' }, () => t('pool.div.cleared')) }, t('pool.div.clear'))))));
    }).catch((e) => rows.replaceChildren(el('tr', {}, el('td', { colspan: '5' }, e.message))));
    return el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('pool.div.title')),
      el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('pool.div.path')), el('th', {}, t('pool.col.member')), el('th', {}, t('pool.div.kind')), el('th', {}, t('pool.div.detail')), el('th', {}, ''))), rows)));
  }

  function createForm(res) {
    const name = el('input', { type: 'text', value: 'home', autocomplete: 'off', spellcheck: 'false' });
    const replicas = el('input', { type: 'number', value: '3', min: '1', max: '9', style: 'width:80px' });
    const checks = (res.candidates || []).map((c) => {
      const chk = el('input', { type: 'checkbox', value: c, checked: true });
      return el('label', { style: 'display:flex;align-items:center;gap:8px' }, chk, c);
    });
    const create = el('button', { class: 'primary' }, t('pool.create'));
    create.addEventListener('click', () => {
      const members = checks.map((l) => l.querySelector('input')).filter((i) => i.checked).map((i) => i.value);
      if (!members.length) { toast(t('pool.create.nomembers'), 'bad'); return; }
      act('/pool/create', { name: name.value.trim() || 'home', members, replicas: Number(replicas.value) || 3, mount: res.mount || '', prefix: '/' }, () => t('pool.restart'));
    });
    return el('div', { class: 'pad' },
      el('div', { class: 'eyebrow' }, t('pool.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 12px' }, t('pool.none.title')),
      el('p', { class: 'detail', style: 'max-width:640px' }, t('pool.none.body')),
      res.configurable ? el('div', { class: 'panel pad', style: 'max-width:640px;display:grid;gap:12px' },
        el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, t('pool.create.name')), name),
        el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, t('pool.create.members')),
          checks.length ? el('div', { style: 'display:grid;gap:6px' }, ...checks) : el('div', { class: 'dim' }, t('pool.create.nocandidates'))),
        el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, t('pool.replicas')), replicas),
        el('div', { class: 'row', style: 'justify-content:flex-end;gap:8px' },
          el('button', { onclick: () => openAddDrive({}) }, t('pool.action.newdrive')), create)) : el('div', { class: 'dim' }, t('pool.noconfig')));
  }

  async function load() {
    try {
      const res = await api.get('/pool/status');
      try { const m = await api.get('/mounts'); res.mount = (m.mounts && m.mounts[0] && m.mounts[0].path) || ''; } catch (_) { res.mount = ''; }
      if (!res.pools || !res.pools.length) { body.replaceChildren(createForm(res)); return; }
      body.replaceChildren(...res.pools.map((p) => poolSection(p, res)));
    } catch (e) { body.replaceChildren(el('div', { class: 'pad' }, e.message)); }
  }

  host.append(body);
  load();
  timer = setInterval(load, 10000);
  return () => clearInterval(timer);
}
