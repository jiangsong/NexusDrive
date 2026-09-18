// The storage pool screen: the members with their health and space, how
// well replicated the files are, what repair is doing, and what a person
// has to decide. Every action goes through the same guarded endpoints the
// CLI uses; configuration edits say "restart required" because that is
// what they are.
import { api } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, promptText, openForm, showPanel } from '/ui/ui.js';
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

  // Rebuilding walks every member and rewrites the index from what is really
  // there. It is the recovery of last resort — long, and it makes the pool
  // unavailable while it runs — so the daemon demands confirm=true and this
  // asks for the pool's name before sending it.
  async function rebuild(p) {
    const ok = await confirmDelete({
      title: t('pool.rebuild.title', p.name), body: t('pool.rebuild.body'),
      confirmToken: p.name, confirmLabel: t('pool.rebuild'),
    });
    if (!ok) return;
    act('/pool/rebuild', { pool: p.name, confirm: true }, () => t('pool.rebuilt'));
  }

  // Scrubbing one path answers "is this one file really where the index says
  // it is", which is the question someone has after a single bad read. The
  // whole-pool scrub is the other button and can take hours.
  async function scrubPath(p) {
    const path = await promptText({
      title: t('pool.scrubpath.title'), label: t('pool.scrubpath.label'),
      initial: '/', confirmLabel: t('pool.action.scrub'),
    });
    if (!path) return;
    act('/pool/scrub', { pool: p.name, path }, (r) => `${t('pool.scrubbed')} ${r.looked || 0}`);
  }

  // Joining adopts a pool that already exists on a remote, from the marker
  // file that pool wrote. It rewrites this machine's configuration to point at
  // someone else's layout, so it is confirmed and says a restart is needed.
  async function joinPool(res) {
    const remotes = (res.candidates || []);
    if (!remotes.length) { toast(t('pool.join.nocandidates'), 'bad'); return; }
    const sel = el('select', {}, ...remotes.map((c) => el('option', { value: c }, c)));
    const root = el('input', { type: 'text', value: '/', autocomplete: 'off', spellcheck: 'false' });
    const ok = await openForm({
      title: t('pool.join.title'),
      rows: [[t('pool.join.remote'), sel], [t('pool.join.root'), root]],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('pool.join.note')),
      confirmLabel: t('pool.join'),
    });
    if (!ok) return;
    act('/pool/join', { remote: sel.value, root: root.value.trim() || '/', confirm: true }, () => t('pool.restart'));
  }

  function memberRow(p, m) {
    const space = m.total ? `${bytes(m.used)} / ${bytes(m.total)}` : t('pool.space.unknown');
    const actions = el('div', { class: 'row', style: 'justify-content:flex-end;gap:6px' });
    const pendingKey = m.pending_restart ? 'pool.member.pending.' + m.pending_restart : '';
    // t() substitutes %s itself, so the question is written once per action.
    const stateBtn = (labelKey, state, doneKey) => el('button', {
      style: 'padding:6px 10px',
      onclick: () => act('/pool/members/state', { pool: p.name, remote: m.remote, state }, () => t(doneKey)),
    }, t(labelKey));
    const confirmedBtn = (labelKey, questionKey, path, doneKey) => el('button', {
      class: 'danger', style: 'padding:6px 10px',
      onclick: async () => {
        const question = t(questionKey, m.remote);
        const ok = await confirmDelete({ title: question, body: question, confirmToken: m.remote, confirmLabel: t(labelKey) });
        if (ok) act(path, { pool: p.name, remote: m.remote, confirm: true }, () => t(doneKey));
      },
    }, t(labelKey));
    if (pendingKey) {
      actions.append(el('span', { class: 'dim', style: 'font-size:12px' }, t(pendingKey)));
    } else if (m.state === 'draining') {
      actions.append(stateBtn('pool.action.enable', 'enabled', 'pool.enabled'));
      actions.append(confirmedBtn('pool.action.remove', 'pool.remove.q', '/pool/members/remove', 'pool.restart'));
    } else if (m.state === 'disabled') {
      actions.append(stateBtn('pool.action.enable', 'enabled', 'pool.enabled'));
    } else {
      actions.append(stateBtn('pool.action.disable', 'disabled', 'pool.disabled'));
      actions.append(confirmedBtn('pool.action.drain', 'pool.drain.q', '/pool/members/drain', 'pool.draining'));
    }
    return el('tr', {},
      el('td', {}, el('div', { style: 'display:flex;align-items:center;gap:8px' }, el('span', { class: 'dot ' + dotClass(m.state), title: m.last_error || '' }), el('span', { style: 'font-weight:600' }, m.remote),
        el('span', { class: 'dim', style: 'font-size:11px' }, m.root && m.root !== '/' ? m.root : ''))),
      el('td', {}, el('div', {}, pendingKey ? t(pendingKey) : t('health.' + (m.state || 'up'))), m.last_error ? el('div', { class: 'dim', style: 'font-size:11px;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap' }, m.last_error) : null),
      el('td', {}, el('div', {}, space), m.total ? bar(m.used, m.total) : null),
      el('td', { style: 'text-align:right' }, String(m.files)),
      el('td', { style: 'text-align:right' }, m.pending_ops ? String(m.pending_ops) : '—'),
      el('td', {}, actions));
  }

  // Rebalancing moves whole files between drives, which costs upload
  // bandwidth on both. Show the plan before asking for it: a person
  // deciding whether to spend that wants to see how much it is.
  async function rebalance(p) {
    const skew = el('input', { type: 'text', value: '10%', autocomplete: 'off', spellcheck: 'false' });
    const ok = await openForm({
      title: t('pool.rebalance.title'),
      rows: [[t('pool.rebalance.skew'), skew]],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('pool.rebalance.note')),
      confirmLabel: t('pool.rebalance.preview'),
    });
    if (!ok) return;
    const target = parseSkew(skew.value);
    let plan;
    try {
      plan = await api.post('/pool/rebalance', { pool: p.name, target_skew: target, dry_run: true });
    } catch (e) { toast(e.message, 'bad'); return; }
    const moves = plan.moves || [];
    if (!moves.length) { toast(t('pool.rebalance.none', plan.reason || ''), 'warn'); return; }
    const go = await confirmDelete({
      title: t('pool.rebalance.title'),
      body: t('pool.rebalance.plan', moves.length, bytes(plan.bytes || 0)),
      confirmLabel: t('pool.action.rebalance'),
    });
    if (!go) return;
    act('/pool/rebalance', { pool: p.name, target_skew: target, confirm: true },
      (r) => t('pool.rebalance.queued', (r.moves || []).length));
  }

  async function editPolicy(p) {
    const cfg = p.config || {};
    const replicas = el('input', { type: 'number', min: '1', max: String(Math.max(1, p.members.length)), value: String(p.replicas) });
    const minimum = el('input', { type: 'number', min: '1', max: String(Math.max(1, p.members.length)), value: String(p.min_replicas) });
    const domain = el('select', {}, ...['account', 'provider', 'member'].map((x) => el('option', { value: x }, x)));
    domain.value = cfg.failure_domain || 'account';
    const mode = el('select', {}, ...['relaxed', 'strict'].map((x) => el('option', { value: x }, x)));
    mode.value = cfg.write_mode || 'relaxed';
    const timeout = el('input', { type: 'text', value: cfg.min_replicas_timeout || '2m' });
    const outAfter = el('input', { type: 'text', value: cfg.out_after || '10m' });
    const repair = el('input', { type: 'number', min: '1', max: '64', value: String(cfg.repair_concurrency || 1) });
    const skew = el('input', { type: 'text', value: `${Math.round((cfg.target_skew || .1) * 100)}%` });
    const backfill = el('input', { type: 'checkbox', checked: cfg.auto_backfill !== false });
    const maxRate = el('input', { type: 'number', min: '1', value: String(cfg.rebalance_max_rate || 31457280) });
    const pause = el('input', { type: 'text', value: cfg.pause_between || '500ms' });
    const classes = el('textarea', { rows: '4', spellcheck: 'false' });
    classes.value = (p.members || []).map((m) => `${m.remote}=${(m.class || []).join(',')}`).join('\n');
    const rules = el('textarea', { rows: '6', spellcheck: 'false' });
    rules.value = (cfg.rules || []).map((r) => [r.prefix, r.replicas || 0, (r.prefer || []).join(','), (r.avoid || []).join(','), (r.require || []).join(',')].join('|')).join('\n');
    const ok = await openForm({
      title: t('pool.config.title'), width: 760,
      rows: [[t('pool.replicas'), replicas], [t('pool.config.minimum'), minimum], [t('pool.config.domain'), domain],
        [t('pool.config.mode'), mode], [t('pool.config.timeout'), timeout], [t('pool.config.outafter'), outAfter],
        [t('pool.config.repair'), repair], [t('pool.config.skew'), skew], [t('pool.config.backfill'), backfill],
        [t('pool.config.rate'), maxRate], [t('pool.config.pause'), pause], [t('pool.config.classes'), classes], [t('pool.config.rules'), rules]],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('pool.config.note')),
      confirmLabel: t('pool.config.save'),
      validate: () => Number(minimum.value) > Number(replicas.value) ? t('pool.config.badminimum') : '',
    });
    if (!ok) return;
    const split = (s) => s.split(',').map((x) => x.trim()).filter(Boolean);
    const memberClasses = {};
    for (const line of classes.value.split('\n')) {
      const [remote, values = ''] = line.split('=', 2);
      if (remote.trim()) memberClasses[remote.trim()] = split(values);
    }
    const parsedRules = rules.value.split('\n').map((line) => line.trim()).filter(Boolean).map((line) => {
      const [prefix, count, prefer = '', avoid = '', require = ''] = line.split('|');
      return { prefix: prefix.trim(), replicas: Number(count) || 0, prefer: split(prefer), avoid: split(avoid), require: split(require) };
    });
    try {
      const result = await api.post('/pool/config', {
        pool: p.name, replicas: Number(replicas.value), min_replicas: Number(minimum.value),
        failure_domain: domain.value, write_mode: mode.value, min_replicas_timeout: timeout.value.trim(), out_after: outAfter.value.trim(),
        repair_concurrency: Number(repair.value), target_skew: parseSkew(skew.value), auto_backfill: backfill.checked,
        rebalance_max_rate: Number(maxRate.value), pause_between: pause.value.trim(), member_classes: memberClasses, rules: parsedRules,
      });
      toast(result.restart_required ? t('pool.restart') : t('pool.config.applied'));
      load();
    } catch (e) { toast(e.message, 'bad'); }
  }

  async function previewPlacement(p) {
    const path = await promptText({ title: t('pool.preview.title'), label: t('pool.preview.path'), initial: '/', confirmLabel: t('pool.preview.run') });
    if (!path) return;
    try {
      const result = await api.post('/pool/preview', { pool: p.name, path });
      const rows = (result.candidates || []).map((c) => el('tr', {},
        el('td', {}, c.remote), el('td', {}, c.domain), el('td', {}, (c.class || []).join(', ') || '—'),
        el('td', {}, c.selected ? t('pool.preview.selected') : (c.eligible ? t('pool.preview.candidate') : t('pool.preview.excluded'))),
        el('td', { class: 'detail' }, (c.reasons || []).join('; '))));
      await showPanel({ title: t('pool.preview.title'), width: 820, content: el('div', {},
        el('div', { class: 'detail', style: 'margin-bottom:10px' }, t('pool.preview.summary', result.path, result.rule || t('pool.preview.default'), result.replicas)),
        el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
          el('th', {}, t('pool.col.member')), el('th', {}, t('pool.config.domain')), el('th', {}, t('pool.config.classes')),
          el('th', {}, t('col.status')), el('th', {}, t('pool.preview.reason')))), el('tbody', {}, ...rows)))) });
    } catch (e) { toast(e.message, 'bad'); }
  }

  // The skew is a fill-ratio difference; people write it as a percentage.
  function parseSkew(v) {
    const raw = String(v || '').trim();
    if (!raw) return 0;
    const n = parseFloat(raw.replace('%', ''));
    if (!isFinite(n) || n <= 0) return 0;
    return raw.includes('%') ? n / 100 : n;
  }

  function poolSection(p, res) {
    const target = p.target_capped ? `${p.target} (${t('pool.capped')})` : String(p.target);
    const health = p.unavailable ? 'bad' : (p.under_replicated ? 'warn' : 'ok');
    const rb = p.rebalance || { queued: 0, skew: 0 };
    const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:14px;padding:0 20px' },
      card(t('pool.space'), p.total ? bytes(p.free) : '—', p.total ? `${t('pool.space.used')} ${bytes(p.used)} / ${bytes(p.total)}` : t('pool.space.unknown')),
      card(t('pool.files'), p.files.toLocaleString(), t('pool.files.sub', p.replicas, target)),
      card(t('pool.health'), el('span', { class: 'dot ' + health, style: 'display:inline-block;width:14px;height:14px;vertical-align:middle;margin-right:8px' }),
        t('pool.health.sub', p.under_replicated, p.unavailable)),
      card(t('pool.repair'), String(p.repair.queued), t('pool.repair.sub', p.repair.blocked, bytes(p.holds_bytes))),
      card(t('pool.rebalance'), `${Math.round((rb.skew || 0) * 100)}%`, t('pool.rebalance.sub', `${Math.round((rb.skew || 0) * 100)}%`, rb.queued || 0)));
    // The write path returns once one member has the data; the rest is repair's
    // job, so say so where the replica counts are read.
    const asyncNote = el('div', { class: 'detail', style: 'padding:10px 20px 0' }, t('pool.protect.async', p.replicas - 1));
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
    addRow.append(el('button', { class: 'primary', onclick: () => openAddDrive({ pool: p.name, onDone: load }) }, t('pool.action.newdrive')));
    return el('div', {},
      el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
        el('div', {}, el('div', { class: 'eyebrow' }, t('pool.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, p.name)),
        el('div', { class: 'row', style: 'gap:8px' },
          p.config ? el('button', { onclick: () => editPolicy(p) }, t('pool.config.edit')) : null,
          p.config ? el('button', { onclick: () => previewPlacement(p) }, t('pool.preview.action')) : null,
          el('button', { onclick: () => act('/pool/repair', { pool: p.name }, (r) => `${t('pool.repaired')} ${r.made || 0}`) }, t('pool.action.repair')),
          el('button', { onclick: () => act('/pool/scrub', { pool: p.name }, (r) => `${t('pool.scrubbed')} ${r.looked || 0}`) }, t('pool.action.scrub')),
          el('button', { onclick: () => scrubPath(p) }, t('pool.action.scrubpath')),
          el('button', { onclick: () => rebalance(p) }, t('pool.action.rebalance')),
          el('button', { class: 'danger', onclick: () => rebuild(p) }, t('pool.rebuild')))),
      cards,
      asyncNote,
      el('div', { style: 'padding:14px 0 0' }, ...notices),
      el('div', { style: 'padding:20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('pool.members')), table, addRow),
      divergences(p));
  }

  function divergences(p) {
    const rows = el('tbody', {}, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('loading'))));
    api.get('/pool/divergences').then((r) => {
      const list = (r.divergences || []).filter((d) => d.pool === p.name);
      if (!list.length) { fill(rows, el('tr', {}, el('td', { colspan: '5', class: 'dim' }, t('pool.div.none')))); return; }
      fill(rows, ...list.map((d) => el('tr', {},
        el('td', {}, d.path), el('td', {}, d.member), el('td', {}, d.kind), el('td', { class: 'detail' }, d.detail),
        el('td', { style: 'text-align:right;white-space:nowrap' },
          el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/divergences', { pool: p.name, path: d.path, member: d.member, kind: d.kind, action: 'relist' }, () => t('pool.div.relisted')) }, t('pool.div.relist')),
          ' ',
          el('button', { style: 'padding:6px 10px', onclick: () => act('/pool/divergences', { pool: p.name, path: d.path, member: d.member, kind: d.kind, action: 'clear' }, () => t('pool.div.cleared')) }, t('pool.div.clear'))))));
    }).catch((e) => fill(rows, el('tr', {}, el('td', { colspan: '5' }, e.message))));
    return el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('pool.div.title')),
      el('div', { class: 'panel', style: 'overflow:auto' }, el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('pool.div.path')), el('th', {}, t('pool.col.member')), el('th', {}, t('pool.div.kind')), el('th', {}, t('pool.div.detail')), el('th', {}, ''))), rows)));
  }

  function createForm(res) {
    const name = el('input', { type: 'text', value: 'home', autocomplete: 'off', spellcheck: 'false' });
    // Where the pool is mounted. A pool with no mount point is a pool nobody
    // can open: the daemon builds its filesystem from the mounts in the
    // configuration, so creating one without this left a person with a
    // restarted daemon and an empty filesystem, and nothing on this page had
    // asked. The daemon suggests the mount that already exists, or the same
    // folder the guided setup proposes.
    const mountPath = el('input', { type: 'text', value: res.mount || '', placeholder: '~/CloudFS', autocomplete: 'off', spellcheck: 'false' });
    const replicas = el('input', { type: 'number', value: '3', min: '1', max: '9', style: 'width:80px' });
    const minimum = el('input', { type: 'number', value: '1', min: '1', max: '9', style: 'width:80px' });
    const domain = el('select', {}, ...['account', 'provider', 'member'].map((x) => el('option', { value: x }, x)));
    const mode = el('select', {}, ...['relaxed', 'strict'].map((x) => el('option', { value: x }, x)));
    const timeout = el('input', { type: 'text', value: '2m' });
    const outAfter = el('input', { type: 'text', value: '10m' });
    const repair = el('input', { type: 'number', value: '1', min: '1', max: '64' });
    const skew = el('input', { type: 'text', value: '10%' });
    const backfill = el('input', { type: 'checkbox', checked: true });
    const classes = el('textarea', { rows: '3', spellcheck: 'false' });
    const rules = el('textarea', { rows: '4', spellcheck: 'false' });
    const checks = (res.candidates || []).map((c) => {
      const chk = el('input', { type: 'checkbox', value: c, checked: true });
      return el('label', { style: 'display:flex;align-items:center;gap:8px' }, chk, c);
    });
    const create = el('button', { class: 'primary' }, t('pool.create'));
    create.addEventListener('click', () => {
      const members = checks.map((l) => l.querySelector('input')).filter((i) => i.checked).map((i) => i.value);
      if (!members.length) { toast(t('pool.create.nomembers'), 'bad'); return; }
      if (Number(minimum.value) > Number(replicas.value)) { toast(t('pool.config.badminimum'), 'bad'); minimum.focus(); return; }
      const split = (s) => s.split(',').map((x) => x.trim()).filter(Boolean);
      const memberClasses = {};
      for (const line of classes.value.split('\n')) { const [remote, values = ''] = line.split('=', 2); if (remote.trim()) memberClasses[remote.trim()] = split(values); }
      const parsedRules = rules.value.split('\n').map((line) => line.trim()).filter(Boolean).map((line) => {
        const [prefix, count, prefer = '', avoid = '', require = ''] = line.split('|');
        return { prefix: prefix.trim(), replicas: Number(count) || 0, prefer: split(prefer), avoid: split(avoid), require: split(require) };
      });
      act('/pool/create', { name: name.value.trim() || 'home', members, replicas: Number(replicas.value) || 3,
        min_replicas: Number(minimum.value) || 1, mount: mountPath.value.trim(), prefix: '/', settings: {
          failure_domain: domain.value, write_mode: mode.value, min_replicas_timeout: timeout.value.trim(), out_after: outAfter.value.trim(),
          repair_concurrency: Number(repair.value) || 1, target_skew: parseSkew(skew.value), auto_backfill: backfill.checked,
          rebalance_max_rate: 31457280, pause_between: '500ms', member_classes: memberClasses, rules: parsedRules,
        } }, () => t('pool.restart'));
    });
    let createFieldID = 0;
    const field = (label, control) => {
      control.id = `pool-create-${++createFieldID}`;
      return el('div', {}, el('label', { for: control.id, style: 'display:block;font-size:12px;margin-bottom:4px' }, label), control);
    };
    return el('div', { class: 'pad' },
      el('div', { class: 'eyebrow' }, t('pool.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 12px' }, t('pool.none.title')),
      el('p', { class: 'detail', style: 'max-width:640px' }, t('pool.none.body')),
      res.configurable ? el('div', { class: 'panel pad', style: 'max-width:760px;display:grid;gap:12px' },
        field(t('pool.create.name'), name),
        field(t('pool.create.mount'), mountPath),
        el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:-6px' }, t('pool.create.mount.note')),
        el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, t('pool.create.members')),
          checks.length ? el('div', { style: 'display:grid;gap:6px' }, ...checks) : el('div', { class: 'dim' }, t('pool.create.nocandidates'))),
        field(t('pool.replicas'), replicas), field(t('pool.config.minimum'), minimum),
        field(t('pool.config.domain'), domain), field(t('pool.config.mode'), mode), field(t('pool.config.timeout'), timeout),
        field(t('pool.config.outafter'), outAfter), field(t('pool.config.repair'), repair), field(t('pool.config.skew'), skew),
        field(t('pool.config.backfill'), backfill), field(t('pool.config.classes'), classes), field(t('pool.config.rules'), rules),
        el('div', { class: 'row', style: 'justify-content:flex-end;gap:8px' },
          el('button', { onclick: () => openAddDrive({ onDone: load }) }, t('pool.action.newdrive')),
          el('button', { onclick: () => joinPool(res) }, t('pool.join')), create)) : el('div', { class: 'dim' }, t('pool.noconfig')));
  }

  async function load() {
    try {
      const res = await api.get('/pool/status');
      try { const m = await api.get('/mounts'); res.mount = (m.mounts && m.mounts[0] && m.mounts[0].path) || ''; } catch (_) { res.mount = ''; }
      if (!res.pools || !res.pools.length) { fill(body, createForm(res)); return; }
      fill(body, ...res.pools.map((p) => poolSection(p, res)));
    } catch (e) { fill(body, el('div', { class: 'pad' }, e.message)); }
  }

  host.append(body);
  load();
  timer = setInterval(load, 10000);
  return () => clearInterval(timer);
}
