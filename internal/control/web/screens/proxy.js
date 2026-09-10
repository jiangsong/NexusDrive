import { api, ApiError } from '/ui/api.js';
import { el, fill, toast, confirmDelete, openForm } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { planOutboundEdit, renameOutboundReferences, outboundReferences } from '/ui/screens/proxy_edit.js';

// The proxy page: outbounds, groups and the rule list, all editable. The
// section is written whole (PUT /proxy/config) because a rule names a group
// which names an outbound — a partial edit can leave three individually valid
// parts that together mean something nobody asked for. The daemon applies the
// new routing live when it can and says so; when it cannot, the page says a
// restart is needed instead of claiming success.
//
// An outbound address may carry a username and password. The daemon shows it
// redacted, so this page must never send that redacted string back as if it
// were the address: it marks such outbounds and sends keep_credentials, which
// tells the daemon to leave the stored address alone.

const OUTBOUND_TYPES = ['direct', 'http', 'socks5'];
const GROUP_TYPES = ['fallback', 'url-test'];

export function renderProxy(host) {
  let cfg = { outbounds: [], groups: [], rules: [], default: 'direct' };
  let health = {};

  const outRows = el('tbody');
  const rulesBox = el('div', { class: 'panel', style: 'overflow:auto' });
  const groupsBox = el('div', { class: 'stack' });
  const saveBtn = el('button', { class: 'primary', disabled: true }, t('proxy.save'));
  const explainOut = el('div', { class: 'dim', style: 'font-size:12px;min-height:18px' });
  const explainInput = el('input', { type: 'text', placeholder: t('proxy.explain.ph'), autocomplete: 'off', spellcheck: 'false', style: 'max-width:280px' });

  saveBtn.addEventListener('click', save);
  explainInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') explain(); });

  function markDirty() { saveBtn.disabled = false; }

  async function load() {
    try {
      // The config read and the health probe do not depend on each other,
      // and the probe is the slow one: it dials every outbound.
      const [next, probed] = await Promise.all([
        api.get('/proxy/config'),
        api.post('/proxy/check', {}).catch(() => []),
      ]);
      cfg = next;
      health = {};
      for (const h of probed) health[h.name] = h;
      saveBtn.disabled = true;
      renderOutbounds();
      renderGroups();
      renderRules();
    } catch (e) { toast(e.message, 'bad'); }
  }

  // Re-probing asks how the outbounds are doing; it says nothing about what
  // the section should contain. Reloading the config to answer it discarded
  // whatever was being edited — a group added and then checked for
  // reachability before saving disappeared with no warning.
  async function reprobe() {
    try {
      const probed = await api.post('/proxy/check', {});
      health = {};
      for (const h of probed) health[h.name] = h;
      renderOutbounds();
    } catch (e) { toast(e.message, 'bad'); }
  }

  function renderOutbounds() {
    fill(outRows, ...(cfg.outbounds || []).map((o, i) => {
      const h = health[o.name] || {};
      const color = h.healthy ? 'var(--ok)' : h.error ? 'var(--bad)' : 'var(--warn)';
      const edit = el('button', {}, t('action.edit'));
      edit.addEventListener('click', () => editOutbound(i));
      const del = el('button', { class: 'danger' }, t('action.delete'));
      del.addEventListener('click', () => removeOutbound(i));
      return el('tr', {},
        el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:9px' }, el('span', { class: 'dot', style: 'background:' + color }), o.name)),
        el('td', { class: 'muted' }, o.type),
        el('td', { class: 'dim', style: 'font-family:ui-monospace,monospace;font-size:12.5px' },
          o.addr || t('proxy.local'),
          o.has_credentials ? el('span', { class: 'dim', style: 'margin-left:8px' }, t('proxy.hascreds')) : null),
        el('td', { class: 'num', style: 'color:' + color }, h.latency_ms != null ? h.latency_ms + ' ms' : '—'),
        el('td', { class: 'detail', style: 'padding-left:16px' }, h.error || (h.healthy ? t('proxy.healthy') : t('proxy.unprobed'))),
        el('td', { style: 'text-align:right;white-space:nowrap' }, edit, del));
    }));
  }

  function renderGroups() {
    const cards = (cfg.groups || []).map((g, i) => {
      const edit = el('button', {}, t('action.edit'));
      edit.addEventListener('click', () => editGroup(i));
      const del = el('button', { class: 'danger' }, t('action.delete'));
      del.addEventListener('click', () => removeGroup(i));
      return el('div', { class: 'panel pad' },
        el('div', { style: 'display:flex;align-items:center;gap:10px' },
          el('span', { style: 'font-weight:620' }, g.name),
          el('span', { style: 'padding:2px 9px;border-radius:999px;background:#1d3350;color:var(--accent-text);font-size:11px' }, g.type),
          el('span', { style: 'flex-grow:1' }), edit, del),
        el('div', { class: 'dim', style: 'font-size:12px;margin-top:8px' }, t('proxy.members') + (g.members || []).join(' · ')));
    });
    if (!cards.length) cards.push(el('div', { class: 'dim', style: 'padding:12px' }, t('empty')));
    fill(groupsBox, ...cards);
  }

  // The rules are edited as text, one per line, because that is the form they
  // have in the file and the form every example in the documentation uses.
  // Reshaping them into three dropdowns would make the page and the file
  // describe the same thing differently.
  function renderRules() {
    const area = el('textarea', {
      rows: '12', spellcheck: 'false',
      style: 'width:100%;font-family:ui-monospace,monospace;font-size:12.5px;padding:10px;background:#121a25;border:1px solid var(--border);border-radius:6px;resize:vertical',
    });
    area.value = (cfg.rules || []).join('\n');
    area.addEventListener('input', () => {
      cfg.rules = area.value.split('\n').map((r) => r.trim()).filter(Boolean);
      markDirty();
    });
    fill(rulesBox,
      el('div', { style: 'padding:12px' }, area,
        el('div', { class: 'dim', style: 'font-size:12px;margin-top:8px' }, t('proxy.rules.hint')),
        el('div', { class: 'dim', style: 'font-size:12px;margin-top:4px' }, t('proxy.default') + (cfg.default || 'direct'))));
  }

  function outboundNames() { return (cfg.outbounds || []).map((o) => o.name); }

  async function editOutbound(index) {
    const o = index < 0 ? { name: '', type: 'socks5', addr: '' } : cfg.outbounds[index];
    const name = el('input', { type: 'text', value: o.name, autocomplete: 'off', spellcheck: 'false' });
    const type = el('select', {}, ...OUTBOUND_TYPES.map((x) => el('option', { value: x }, x)));
    type.value = o.type || 'socks5';
    const addr = el('input', { type: 'text', value: o.addr || '', placeholder: 'host:port', autocomplete: 'off', spellcheck: 'false' });
    const refs = index < 0 ? { groups: [], rules: [] } : outboundReferences(cfg, o.name);
    // Remotes select an outbound from outside this section; the daemon reports
    // them so the page can say a rename is not its to make.
    const referrers = (index < 0 ? [] : (cfg.referrers || {})[o.name]) || [];
    const notes = [];
    if (o.has_credentials) notes.push(t('proxy.creds.note'));
    if (refs.groups.length || refs.rules.length) notes.push(t('proxy.rename.refs', refs.groups.length, refs.rules.length));
    if (referrers.length) notes.push(t('proxy.rename.blocked', referrers.join(', ')));
    const note = notes.length
      ? el('div', { class: 'dim', style: 'font-size:11.5px' }, ...notes.map((text) => el('div', {}, text)))
      : null;
    const ok = await openForm({
      title: t(index < 0 ? 'proxy.outbound.add' : 'proxy.outbound.edit'),
      rows: [[t('proxy.field.name'), name], [t('col.type'), type], [t('col.address'), addr]],
      note,
    });
    if (!ok) return;
    const plan = planOutboundEdit(index < 0 ? null : o, { name: name.value.trim(), type: type.value, addr: addr.value.trim() }, referrers);
    if (plan.error) { toast(t(plan.error, ...(plan.args || [])), 'bad'); return; }
    const next = plan.outbound;
    if (index < 0) {
      cfg = { ...cfg, outbounds: [...(cfg.outbounds || []), next] };
    } else {
      // A rename has to take the references with it. The daemon validates the
      // section as a whole and refuses a group or rule that names an outbound
      // no longer there, so leaving them behind fails the save with nothing
      // the page has offered to fix.
      const renamed = renameOutboundReferences(cfg, o.name, next.name);
      cfg = { ...renamed, outbounds: renamed.outbounds.map((x, i) => (i === index ? next : x)) };
    }
    markDirty();
    renderOutbounds();
    renderGroups();
    renderRules();
  }

  async function removeOutbound(index) {
    const o = cfg.outbounds[index];
    const ok = await confirmDelete({ title: t('proxy.outbound.remove', o.name), body: t('proxy.outbound.remove.body'), confirmToken: o.name });
    if (!ok) return;
    cfg.outbounds = cfg.outbounds.filter((_, i) => i !== index);
    markDirty();
    renderOutbounds();
    // The group cards list their members by name, so one of them is now
    // naming an outbound that is gone. Redrawing only the table left that on
    // screen until something else happened to redraw the page — and it is
    // also what the daemon refuses the save for, so the reader needs to see
    // which group to fix.
    renderGroups();
  }

  async function editGroup(index) {
    const g = index < 0 ? { name: '', type: 'url-test', members: [], check_url: '', interval: '', timeout: '' } : cfg.groups[index];
    const name = el('input', { type: 'text', value: g.name, autocomplete: 'off', spellcheck: 'false' });
    const type = el('select', {}, ...GROUP_TYPES.map((x) => el('option', { value: x }, x)));
    type.value = g.type || 'url-test';
    const members = el('select', { multiple: true, size: '5', style: 'height:auto' },
      ...outboundNames().map((n) => el('option', { value: n }, n)));
    for (const opt of members.options) opt.selected = (g.members || []).includes(opt.value);
    const checkURL = el('input', { type: 'text', value: g.check_url || '', placeholder: 'https://…', autocomplete: 'off', spellcheck: 'false' });
    const interval = el('input', { type: 'text', value: g.interval || '', placeholder: '5m', autocomplete: 'off' });
    const timeout = el('input', { type: 'text', value: g.timeout || '', placeholder: '5s', autocomplete: 'off' });
    const ok = await openForm({
      title: t(index < 0 ? 'proxy.group.add' : 'proxy.group.edit'),
      rows: [
        [t('proxy.field.name'), name], [t('col.type'), type], [t('proxy.field.members'), members],
        [t('proxy.field.checkurl'), checkURL], [t('proxy.field.interval'), interval], [t('proxy.field.timeout'), timeout],
      ],
    });
    if (!ok) return;
    const next = {
      name: name.value.trim(), type: type.value,
      members: [...members.selectedOptions].map((x) => x.value),
      check_url: checkURL.value.trim(), interval: interval.value.trim(), timeout: timeout.value.trim(),
    };
    if (!next.name || !next.members.length) { toast(t('proxy.group.needmembers'), 'bad'); return; }
    if (index < 0) cfg.groups = [...(cfg.groups || []), next];
    else cfg.groups = cfg.groups.map((x, i) => (i === index ? next : x));
    markDirty();
    renderGroups();
  }

  async function removeGroup(index) {
    const g = cfg.groups[index];
    const ok = await confirmDelete({ title: t('proxy.group.remove', g.name), body: t('proxy.group.remove.body'), confirmToken: g.name });
    if (!ok) return;
    cfg.groups = cfg.groups.filter((_, i) => i !== index);
    markDirty();
    renderGroups();
  }

  async function save() {
    saveBtn.disabled = true;
    try {
      const payload = {
        outbounds: (cfg.outbounds || []).map((o) => ({
          name: o.name, type: o.type, addr: o.addr,
          // An outbound still marked as carrying a credential is one whose
          // address this page never replaced: keep the stored one.
          keep_credentials: !!o.keep_credentials || !!o.has_credentials,
        })),
        groups: cfg.groups || [],
        rules: cfg.rules || [],
      };
      const res = await api.put('/proxy/config', payload);
      if (res.warning) toast(res.warning, 'bad');
      else if (res.applied) toast(t('proxy.saved.applied'));
      else toast(t('proxy.saved.restart'));
      await load();
    } catch (e) {
      saveBtn.disabled = false;
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }

  async function explain() {
    const hostname = explainInput.value.trim();
    if (!hostname) return;
    explainOut.textContent = '…';
    try {
      const r = await api.get('/proxy/explain?host=' + encodeURIComponent(hostname));
      explainOut.textContent = r.error
        ? t('proxy.explain.error', r.error)
        : t('proxy.explain.result', r.rule || t('proxy.explain.norule'), r.outbound, r.resolved || r.outbound);
    } catch (e) {
      explainOut.textContent = e instanceof ApiError ? e.message : String(e);
    }
  }

  const addOutbound = el('button', {}, t('proxy.outbound.add'));
  addOutbound.addEventListener('click', () => editOutbound(-1));
  const addGroup = el('button', {}, t('proxy.group.add'));
  addGroup.addEventListener('click', () => editGroup(-1));
  const explainBtn = el('button', {}, t('proxy.explain.go'));
  explainBtn.addEventListener('click', explain);

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between;gap:12px' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('proxy.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('proxy.title'))),
      el('div', { class: 'row', style: 'gap:9px' },
        el('button', { onclick: reprobe }, t('proxy.recheck')), saveBtn)),
    el('div', { style: 'padding:0 20px 12px', class: 'row' },
      explainInput, explainBtn, explainOut),
    el('div', { style: 'padding:0 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('proxy.outbounds')), el('th', {}, t('col.type')), el('th', {}, t('col.address')),
        el('th', { class: 'num' }, t('col.latency')), el('th', { style: 'padding-left:16px' }, t('col.status')),
        el('th', { style: 'text-align:right' }, t('col.actions')))), outRows)),
      el('div', { class: 'row', style: 'margin-top:10px' }, addOutbound)),
    el('div', { style: 'display:grid;grid-template-columns:420px 1fr;gap:20px;padding:20px' },
      el('div', {},
        el('div', { class: 'row', style: 'margin-bottom:12px' },
          el('div', { class: 'eyebrow', style: 'flex-grow:1' }, t('proxy.groups')), addGroup),
        groupsBox),
      el('div', {}, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('proxy.rules')), rulesBox)));

  load();
  return () => {};
}
