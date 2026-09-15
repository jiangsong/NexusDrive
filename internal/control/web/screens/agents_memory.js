import { api } from '/ui/api.js';
import { el, fill, toast, bytes, openForm, moreRow } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { isValidName, copiesOf } from '/ui/memory_conflicts.js';
import { openMemoryEditor, openConflictOverlay, factURL } from '/ui/memory_panel.js';

// The memory tab (docs/ui-plan.md F7, TODO.md T-40): what each agent has
// written under memory.root, one agent at a time. The left column is GET
// /memory/agents; the table is GET /memory/{agent}; the search box is GET
// /memory/search scoped to the chosen agent (plus shared, as the daemon
// defaults). The chosen agent lives in the URL hash beside the tab
// (#/agents?tab=memory&agent=codex) so a reload lands on the same list.
//
// A daemon without memory.root answers enabled:false, and the tab shows
// the reason and the configuration block instead of an empty table; it
// asks for nothing else in that state. Every name, description and snippet
// here was written by an agent and goes in as a text node.
const COLUMNS = 5;
const PAGE = 100;

function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// renderDisabled is the whole tab when memory is off: which of the two
// reasons, and — when the daemon sent one — the configuration example, as
// text in a <pre>.
function renderDisabled(host, r) {
  const reason = r.reason === 'no_root' ? t('memory.disabled.no_root') : t('memory.disabled.unavailable');
  fill(host,
    el('div', { style: 'padding:0 20px 20px;max-width:640px' },
      el('p', { class: 'detail' }, reason),
      r.example ? el('pre', { class: 'panel pad', style: 'font-size:12.5px;white-space:pre' }, r.example) : null));
}

export function renderMemoryTab(host, params) {
  let disposed = false;
  let generation = 0;
  let agents = [];
  let maxFactBytes = 0;
  let selected = (params && params.get('agent')) || '';
  let searching = false;
  let wanted = (params && params.get('memory')) || '';

  const agentList = el('div', { style: 'display:grid;gap:4px' });
  const rows = el('tbody');
  const table = el('div', { class: 'panel', style: 'overflow:auto' },
    el('table', {}, el('thead', {}, el('tr', {},
      el('th', {}, t('memory.col.name')), el('th', {}, t('memory.col.description')),
      el('th', {}, t('memory.col.type')), el('th', {}, t('memory.col.updated')),
      el('th', {}, t('memory.col.conflicts')))), rows));
  const results = el('div', { class: 'panel', style: 'display:none' });
  const search = el('input', { type: 'search', placeholder: t('memory.search.placeholder'), autocomplete: 'off', spellcheck: 'false', style: 'flex-grow:1;min-width:200px' });

  function edit(name) {
    openMemoryEditor({ agent: selected, name, maxBytes: maxFactBytes, onChanged: () => loadAgents() });
  }

  function resolve(name) {
    openConflictOverlay({ agent: selected, name, maxBytes: maxFactBytes, onChanged: () => loadAgents() });
  }

  function conflictCell(f) {
    const copies = copiesOf(f);
    if (!copies.length) return el('td', { class: 'dim' }, '');
    return el('td', {},
      el('button', { 'data-conflicts': String(copies.length), style: 'display:inline-flex;align-items:center;gap:7px', onclick: (ev) => { ev.stopPropagation(); resolve(f.name); } },
        el('span', { class: 'dot bad' }), t('memory.conflict'), el('span', { class: 'dim' }, '· ' + t('memory.conflict.count', String(copies.length)))));
  }

  function factRow(f) {
    const m = f.meta || {};
    return el('tr', { 'data-fact': f.name, style: 'cursor:pointer', onclick: () => edit(f.name) },
      el('td', {}, el('code', {}, f.name)),
      el('td', { class: 'detail', style: 'max-width:360px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap' }, m.description || ''),
      el('td', { class: 'detail' }, m.type || ''),
      el('td', { class: 'detail tnums', style: 'white-space:nowrap' }, when(m.updated_at)),
      conflictCell(f));
  }

  function agentItem(a) {
    const chosen = a.name === selected;
    return el('button', {
      'data-agent': a.name, 'aria-pressed': String(chosen),
      style: 'display:flex;flex-direction:column;align-items:stretch;gap:3px;text-align:left;padding:8px 10px' + (chosen ? ';border-color:var(--accent-border);background:var(--accent-bg)' : ''),
      onclick: () => select(a.name),
    },
    el('span', { class: 'row', style: 'justify-content:space-between;gap:8px' },
      el('code', {}, a.name),
      a.conflicts ? el('span', { class: 'dot bad', title: t('memory.conflict') }) : null),
    el('span', { class: 'dim tnums', style: 'font-size:11.5px' },
      t('memory.facts.count', String(a.facts || 0)) + ' · ' + t('memory.usage', bytes(a.bytes || 0), bytes(a.max_bytes || 0))));
  }

  function fillAgents() {
    fill(agentList, agents.length ? agents.map(agentItem) : el('div', { class: 'dim', style: 'font-size:12.5px;padding:6px 2px' }, t('memory.agents.empty')));
  }

  function select(name) {
    if (name === selected) return;
    selected = name;
    history.replaceState(null, '', '#/agents?tab=memory&agent=' + encodeURIComponent(name));
    fillAgents();
    clearSearch();
    loadFacts();
  }

  async function loadAgents() {
    let r;
    try {
      r = await api.get('/memory/agents');
    } catch (e) {
      if (!disposed) fill(host, el('div', { class: 'pad detail' }, t('memory.load.failed', e.message)));
      return;
    }
    if (disposed) return;
    if (!r.enabled) {
      renderDisabled(host, r);
      return;
    }
    agents = r.agents || [];
    maxFactBytes = Number(r.max_fact_bytes) || 0;
    if (!agents.some((a) => a.name === selected)) selected = agents.length ? agents[0].name : '';
    fillAgents();
    if (!searching) loadFacts();
    // A deep link (#/agents?tab=memory&agent=<a>&memory=<name>) opens that
    // fact's editor straight away, once, the way ?session= does.
    if (wanted && selected) { const name = wanted; wanted = ''; edit(name); }
  }

  function appendPage(r) {
    rows.append(...(r.facts || []).map(factRow));
    if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('memory.empty', selected))));
    if (r.next_cursor) rows.append(moreRow(COLUMNS, () => loadFacts(r.next_cursor)));
  }

  async function loadFacts(cursor) {
    cursor = pageCursor(cursor);
    if (!cursor) fill(rows);
    if (!selected) {
      fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('memory.select'))));
      return;
    }
    const mine = ++generation;
    try {
      const r = await api.get('/memory/' + encodeURIComponent(selected) + '?limit=' + PAGE
        + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      if (disposed || mine !== generation) return;
      appendPage(r);
    } catch (e) {
      if (disposed || mine !== generation) return;
      const failed = el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') rows.append(failed);
      else fill(rows, failed);
    }
  }

  // hitRow is one search hit. A hit inside a fact opens that fact's editor;
  // one outside the fact grammar (a copy, MEMORY.md) is shown and not
  // clickable, because the memory routes cannot address it.
  function hitRow(h) {
    const openable = isValidName(h.agent) && isValidName(h.name);
    return el('div', {
      class: 'detail', 'data-hit': h.path || '',
      style: 'padding:10px 14px;border-bottom:1px solid var(--hairline)' + (openable ? ';cursor:pointer' : ''),
      onclick: openable ? () => openMemoryEditor({ agent: h.agent, name: h.name, maxBytes: maxFactBytes, onChanged: () => loadAgents() }) : null,
      title: openable ? '' : t('memory.search.nohit'),
    },
    el('div', { class: 'row', style: 'gap:8px;align-items:baseline;flex-wrap:wrap' },
      openable ? el('code', {}, h.agent + '/' + h.name) : el('code', { class: 'dim' }, h.path || ''),
      h.heading ? el('span', { class: 'dim', style: 'font-size:12px' }, h.heading) : null),
    el('div', { style: 'font-size:12.5px;margin-top:4px;white-space:pre-wrap;word-break:break-word' }, h.snippet || ''));
  }

  function clearSearch() {
    searching = false;
    search.value = '';
    results.style.display = 'none';
    table.style.display = '';
    fill(results);
  }

  async function runSearch() {
    const q = search.value.trim();
    if (!q) { clearSearch(); loadFacts(); return; }
    searching = true;
    table.style.display = 'none';
    results.style.display = '';
    const mine = ++generation;
    try {
      const r = await api.get('/memory/search?q=' + encodeURIComponent(q) + (selected ? '&agent=' + encodeURIComponent(selected) : ''));
      if (disposed || mine !== generation) return;
      const notes = [];
      if (r.degraded) notes.push(t('memory.search.degraded', r.degraded));
      if (r.truncated) notes.push(t('memory.search.truncated'));
      if (r.pending) notes.push(t('memory.search.pending', String(r.pending)));
      fill(results,
        el('div', { class: 'row', style: 'padding:8px 14px;gap:12px;align-items:center;border-bottom:1px solid var(--hairline)' },
          el('button', { onclick: () => { clearSearch(); loadFacts(); } }, t('memory.search.clear')),
          notes.length ? el('span', { class: 'dim', style: 'font-size:12px' }, notes.join(' · ')) : null),
        ...(r.hits || []).map(hitRow),
        (r.hits || []).length ? null : el('div', { class: 'dim', style: 'padding:12px 14px' }, t('memory.search.empty')));
    } catch (e) {
      if (disposed || mine !== generation) return;
      fill(results, el('div', { class: 'detail', style: 'padding:12px 14px' }, e.message));
    }
  }

  // createMemory is the "new memory" form: agent and name are checked
  // against the store's grammar before anything is sent, and the put is the
  // same one the editor makes, without a version because the file is new.
  async function createMemory() {
    const known = agents.map((a) => a.name);
    if (!known.includes('shared')) known.push('shared');
    const listID = 'memory-agents-' + Date.now();
    const agent = el('input', { type: 'text', list: listID, value: selected || '', autocomplete: 'off', spellcheck: 'false' });
    const options = el('datalist', { id: listID }, ...known.map((n) => el('option', { value: n })));
    const name = el('input', { type: 'text', placeholder: 'style', autocomplete: 'off', spellcheck: 'false' });
    const description = el('input', { type: 'text', autocomplete: 'off', spellcheck: 'false' });
    const type = el('input', { type: 'text', placeholder: 'preference', autocomplete: 'off', spellcheck: 'false' });
    const content = el('textarea', { rows: '8', spellcheck: 'false', style: 'width:100%;box-sizing:border-box;font-family:ui-monospace,monospace;font-size:12.5px' });
    const ok = await openForm({
      title: t('memory.new'),
      width: 560,
      rows: [
        [t('memory.form.agent'), agent], [t('memory.form.name'), name],
        [t('memory.form.description'), description], [t('memory.form.type'), type],
        [t('memory.form.content'), content],
      ],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, options, t('memory.form.note')),
      confirmLabel: t('memory.create'),
      validate: () => {
        if (!isValidName(agent.value.trim())) { agent.focus(); return t('memory.err.agent'); }
        if (!isValidName(name.value.trim())) { name.focus(); return t('memory.err.name'); }
        return '';
      },
    });
    if (!ok) return;
    const a = agent.value.trim();
    const n = name.value.trim();
    try {
      await api.put(factURL(a, n), { content: content.value, description: description.value.trim(), type: type.value.trim() });
    } catch (e) {
      toast(e.message, 'bad');
      return;
    }
    toast(t('memory.created', a + '/' + n));
    if (a !== selected) {
      selected = a;
      history.replaceState(null, '', '#/agents?tab=memory&agent=' + encodeURIComponent(a));
    }
    clearSearch();
    loadAgents();
  }

  search.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); runSearch(); } });
  search.addEventListener('search', () => { if (!search.value) { clearSearch(); loadFacts(); } });

  fill(host,
    el('div', { class: 'pad', style: 'display:flex;align-items:center;gap:12px;padding-top:0;padding-bottom:12px;flex-wrap:wrap' },
      el('div', { class: 'detail', style: 'font-size:13px;flex-basis:100%' }, t('memory.note')),
      search,
      el('button', { onclick: runSearch }, t('memory.search')),
      el('div', { class: 'grow' }),
      el('button', { onclick: () => loadAgents() }, t('action.refresh')),
      el('button', { class: 'primary', onclick: createMemory }, t('memory.new'))),
    el('div', { style: 'display:grid;grid-template-columns:220px minmax(0,1fr);gap:16px;padding:0 20px 20px;align-items:start' },
      el('div', {},
        el('div', { class: 'eyebrow', style: 'margin-bottom:8px' }, t('memory.agents')),
        agentList),
      el('div', {}, table, results)));

  loadAgents();
  return () => { disposed = true; };
}
