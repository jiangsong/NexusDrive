import { api } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, openForm, moreRow, iconEl } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { get, subscribe } from '/ui/store.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onIndexChange } from '/ui/app.js';
import { includeFor, parseSize, PRESET_NAMES } from '/ui/index_presets.js';
import { mountEmbeddingPanel } from '/ui/embedding_panel.js';

// The index screen: what the content index holds, what it is doing, which
// rules make it download files, and which files it could not read.
//
// The screen has two shapes. A daemon without an index answers
// /index/status with {enabled:false} and 404 for everything else, so that
// answer ends the load with an explanation and a configuration snippet —
// no empty tables, and no second request the daemon would refuse. With an
// index, the four cards follow the status ticks the shell already
// receives, the progress bar follows the index SSE event, and the two
// tables load once and reload after the actions that change them. The
// embedding endpoint panel between the progress bar and the rules is its
// own module (embedding_panel.js); this screen only mounts it.
//
// Two actions are destructive and ask for a typed confirmation, for the
// same reason the daemon demands confirm: true for them: removing a rule
// deletes the text extracted under it, and a rebuild empties the index and
// downloads every rule-covered file again.
const DISABLED_EXAMPLE = 'index:\n  enabled: true\n  pinned: true\n  rules:\n    - path: /work/notes\n      max_file_size: 20MiB\n';

const RULE_COLUMNS = 7;
const FAILED_COLUMNS = 5;
const FAILED_PAGE = 50;

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || String(iso).startsWith('0001') ? '' : d.toLocaleString(locale());
}

// figures reads the card values from either document that carries them:
// the /index/status snapshot the screen loads first, and the index line
// of the /status tick that keeps them fresh afterwards. The two spell the
// budget differently (an object against two numbers), so both are read
// here and nowhere else.
function figures(st) {
  const budget = st.fetch_budget;
  const nested = budget && typeof budget === 'object';
  return {
    docs: st.docs || {},
    chunks: st.chunks_total != null ? st.chunks_total : (st.chunks || 0),
    text: st.text_bytes || 0,
    maxText: st.max_total_text || 0,
    fetched: nested ? (budget.used || 0) : (st.fetched_this_hour || 0),
    limit: nested ? (budget.limit || 0) : (budget || 0),
    vectors: st.vectors || 0,
    maxChunks: st.max_chunks || 0,
  };
}

function card(label, value, sub) {
  return el('div', { class: 'panel pad' },
    el('div', { class: 'muted', style: 'font-size:13px;margin-bottom:9px' }, label),
    el('div', { class: 'tnums', style: 'font-size:28px;font-weight:720;letter-spacing:-.03em' }, value),
    el('div', { class: 'detail', style: 'font-size:13px;margin-top:12px' }, sub));
}

function btn(label, onclick, cls) {
  return el('button', { style: 'padding:6px 12px', class: cls || '', onclick }, label);
}

function section(title, table, aside) {
  return el('div', { style: 'padding:0 20px 20px' },
    el('div', { class: 'row', style: 'justify-content:space-between;margin-bottom:12px' },
      el('div', { class: 'eyebrow' }, title), aside || null),
    el('div', { class: 'panel', style: 'overflow:auto' }, table));
}

function renderDisabled(host) {
  fill(host,
    el('div', { class: 'pad', style: 'padding-bottom:12px' },
      el('div', { class: 'eyebrow' }, t('index.eyebrow')),
      el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('index.title'))),
    el('div', { style: 'padding:0 20px 20px;max-width:640px' },
      el('p', { class: 'detail' }, t('index.disabled.body')),
      el('pre', { class: 'panel pad', style: 'font-size:12.5px;white-space:pre' }, DISABLED_EXAMPLE)));
}

export function renderIndex(host) {
  let disposed = false;
  const stops = [];
  const cards = el('div', { style: 'display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:14px;padding:0 20px' });
  const progress = el('div', { style: 'padding:14px 20px 0' });
  const embedding = el('div', { class: 'panel pad', 'data-embedding-panel': '' });
  const rulesBody = el('tbody');
  const failedBody = el('tbody');
  // A reader who paged deeper into the failures keeps that view; a
  // progress frame reloads the first page only when it is all there is.
  let failedPaged = false;
  let lastFailed = null;
  // The embedding panel loads itself; the screen refreshes it after the
  // actions that change what it shows (a rebuild empties the vectors).
  let panel = null;

  // The load order is the contract with the daemon: status first, and only
  // an enabled index goes on to ask for rules and failures. The functions
  // below are declarations, so they are in scope here.
  (async () => {
    let st;
    try { st = await api.get('/index/status'); } catch (err) { toast(err.message, 'bad'); return; }
    if (disposed) return;
    if (!st.enabled) {
      renderDisabled(host);
      return;
    }
    renderEnabled(st);
    panel = mountEmbeddingPanel(embedding);
    stops.push(() => panel.dispose());
    await loadRules();
    await loadFailed('');
    if (disposed) return;
    stops.push(onIndexChange((p) => renderProgress(p)));
    // The status tick the shell receives every couple of seconds carries
    // the same counts the cards show, so they stay current without a
    // request of their own.
    stops.push(subscribe(() => {
      const s = get().status;
      if (s && s.index && s.index.enabled) renderCards(s.index);
    }));
  })();

  function renderCards(st) {
    const f = figures(st);
    fill(cards,
      card(t('index.card.docs'), String((f.docs.ok || 0) + (f.docs.dirty || 0) + (f.docs.failed || 0)),
        t('index.card.docs.detail', f.docs.ok || 0, f.docs.dirty || 0, f.docs.failed || 0)),
      card(t('index.card.chunks'), String(f.chunks), t('index.card.vectors', f.vectors, f.maxChunks)),
      card(t('index.card.text'), bytes(f.text), f.maxText ? t('index.card.limit', bytes(f.maxText)) : t('index.card.nolimit')),
      card(t('index.card.fetch'), bytes(f.fetched), f.limit ? t('index.card.limit', bytes(f.limit)) : t('index.card.nolimit')));
  }

  // The bar shows the queue while something is in it and the reason while
  // the worker stands still; an idle index shows nothing here, because a
  // full bar that never moves reads as a stuck one.
  function renderProgress(p) {
    p = p || {};
    const active = (p.extracting || 0) + (p.pending || 0) > 0;
    if (!active && !p.paused) { fill(progress); progress.hidden = true; return; }
    progress.hidden = false;
    const total = (p.extracted || 0) + (p.pending || 0);
    const done = total > 0 ? (p.extracted || 0) / total : 0;
    const lines = [];
    if (active) lines.push(t('index.progress', p.extracting || 0, p.pending || 0));
    if (p.paused) {
      const at = p.resume_at ? new Date(p.resume_at) : null;
      const resumes = at && !Number.isNaN(at.getTime()) && !String(p.resume_at).startsWith('0001');
      lines.push(t('index.paused.' + p.paused) + (resumes ? ' · ' + t('index.resume', at.toLocaleTimeString(locale())) : ''));
    }
    fill(progress,
      el('div', { class: 'progress' + (p.paused ? ' warn' : '') }, el('span', { style: `width:${Math.round(done * 100)}%` })),
      el('div', { class: 'detail', style: 'margin-top:8px', 'data-paused': p.paused || '' }, lines.join(' · ')));
    // A frame that raised the failure count means the failed table is
    // out of date; the first page is cheap to fetch again.
    if (lastFailed != null && p.failed != null && p.failed !== lastFailed && !failedPaged) loadFailed('');
    if (p.failed != null) lastFailed = p.failed;
  }

  async function refreshStatus() {
    try {
      const st = await api.get('/index/status');
      if (disposed || !st.enabled) return;
      renderCards(st);
      renderProgress(st.progress);
    } catch (e) { toast(e.message, 'bad'); }
  }

  function ruleRow(rule) {
    const include = rule.include || [];
    const exclude = rule.exclude || [];
    return el('tr', { 'data-source': rule.source },
      el('td', {}, rule.path),
      el('td', { class: 'detail' }, include.length ? include.join(', ') : t('index.rule.defaults')),
      el('td', { class: 'detail' }, exclude.length ? exclude.join(', ') : ''),
      el('td', { class: 'detail tnums' }, rule.max_file_size > 0 ? bytes(rule.max_file_size) : t('index.rule.defaults')),
      el('td', { class: 'muted' }, t('index.rule.source.' + rule.source)),
      el('td', { class: 'tnums' }, String(rule.documents || 0)),
      el('td', { style: 'text-align:right' }, rule.source === 'config' || rule.source === 'builtin'
        ? el('span', { class: 'dimmer' }, t('index.rule.inconfig'))
        : btn(t('index.rule.remove'), () => removeRule(rule))));
  }

  function renderRules(rules) {
    fill(rulesBody, ...(rules || []).map(ruleRow));
    if (!(rules || []).length) fill(rulesBody, el('tr', {}, el('td', { colspan: String(RULE_COLUMNS), class: 'dim' }, t('index.rules.empty'))));
  }

  async function loadRules() {
    try {
      const r = await api.get('/index/rules');
      if (!disposed) renderRules(r.rules);
    } catch (e) { fill(rulesBody, el('tr', {}, el('td', { colspan: String(RULE_COLUMNS) }, e.message))); }
  }

  async function removeRule(rule) {
    const ok = await confirmDelete({
      title: t('index.rule.remove.title'), body: t('index.rule.remove.body', rule.path),
      confirmToken: rule.path, confirmLabel: t('index.rule.remove'),
    });
    if (!ok) return;
    try {
      const r = await api.post('/index/remove', { path: rule.path, confirm: true });
      if (disposed) return;
      renderRules(r.rules);
      toast(t('index.rule.removed', rule.path));
      refreshStatus();
    } catch (e) { toast(e.message, 'bad'); }
  }

  async function addRule() {
    const path = el('input', { type: 'text', placeholder: '/work/notes', autocomplete: 'off', spellcheck: 'false' });
    const preset = el('select', {}, ...PRESET_NAMES.map((n) => el('option', { value: n }, t('index.preset.' + n))));
    const size = el('input', { type: 'text', placeholder: '20MiB', autocomplete: 'off', spellcheck: 'false' });
    const ok = await openForm({
      title: t('index.rule.add'),
      rows: [[t('index.rule.col.path'), path], [t('index.rule.preset'), preset], [t('index.rule.col.maxsize'), size]],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('index.rule.note')),
      confirmLabel: t('index.rule.add'),
      validate: () => {
        if (!path.value.trim().startsWith('/')) { path.focus(); return t('index.rule.needpath'); }
        if (Number.isNaN(parseSize(size.value))) { size.focus(); return t('index.rule.badsize'); }
        return '';
      },
    });
    if (!ok) return;
    const body = { path: path.value.trim(), include: includeFor(preset.value) };
    const max = parseSize(size.value);
    if (max > 0) body.max_file_size = max;
    try {
      const r = await api.post('/index/add', body);
      if (disposed) return;
      renderRules(r.rules);
      toast(t('index.rule.added', body.path));
      refreshStatus();
    } catch (e) { toast(e.message, 'bad'); }
  }

  function failedRow(d) {
    return el('tr', {},
      el('td', {}, d.path),
      el('td', { class: 'muted' }, d.kind || ''),
      el('td', { class: 'detail' }, d.error || ''),
      el('td', { class: 'detail tnums' }, when(d.indexed_at)),
      el('td', { style: 'text-align:right' }, btn(t('index.retry'), () => retry(d.path))));
  }

  async function loadFailed(cursor) {
    // A cursor is a string the daemon issued; a click event is not one.
    cursor = pageCursor(cursor);
    failedPaged = !!cursor;
    if (!cursor) fill(failedBody);
    try {
      const r = await api.get('/index/failed?limit=' + FAILED_PAGE + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      if (disposed) return;
      failedBody.append(...(r.documents || []).map(failedRow));
      if (!failedBody.children.length) fill(failedBody, el('tr', {}, el('td', { colspan: String(FAILED_COLUMNS), class: 'dim' }, t('index.failed.empty'))));
      if (r.next_cursor) failedBody.append(moreRow(FAILED_COLUMNS, () => loadFailed(r.next_cursor)));
    } catch (e) {
      const failed = el('tr', {}, el('td', { colspan: String(FAILED_COLUMNS) }, e.message));
      if (pageFailureMode(cursor) === 'append') failedBody.append(failed);
      else fill(failedBody, failed);
    }
  }

  // retry with a path requeues that document; with none, every failed one.
  async function retry(path) {
    try {
      const r = await api.post('/index/retry', path ? { path } : {});
      if (disposed) return;
      toast(t('index.retry.toast', r.requeued || 0));
      loadFailed('');
    } catch (e) { toast(e.message, 'bad'); }
  }

  async function rebuild() {
    const ok = await confirmDelete({
      title: t('index.rebuild.title'), body: t('index.rebuild.body'),
      confirmToken: 'rebuild', confirmLabel: t('index.rebuild'),
    });
    if (!ok) return;
    try {
      const st = await api.post('/index/rebuild', { confirm: true });
      if (disposed) return;
      toast(t('index.rebuild.started'));
      renderCards(st);
      renderProgress(st.progress);
      if (panel) panel.refresh();
      loadRules();
      loadFailed('');
    } catch (e) { toast(e.message, 'bad'); }
  }

  function renderEnabled(st) {
    fill(host,
      el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
        el('div', {}, el('div', { class: 'eyebrow' }, t('index.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('index.title'))),
        el('div', { class: 'row', style: 'gap:9px' },
          el('button', { onclick: addRule }, iconEl('plus'), t('index.rule.add')),
          el('button', { class: 'danger', onclick: rebuild }, t('index.rebuild')))),
      cards, progress,
      el('div', { style: 'padding:20px 20px 20px' }, embedding),
      section(t('index.rules'), el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('index.rule.col.path')), el('th', {}, t('index.rule.col.include')), el('th', {}, t('index.rule.col.exclude')),
        el('th', {}, t('index.rule.col.maxsize')), el('th', {}, t('index.rule.col.source')), el('th', {}, t('index.rule.col.documents')),
        el('th', { style: 'text-align:right' }, t('col.actions')))), rulesBody)),
      el('div', { class: 'dim', style: 'padding:0 20px 20px;font-size:12px;margin-top:-8px' }, t('index.rules.help')),
      section(t('index.failed'), el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('col.path')), el('th', {}, t('index.failed.col.kind')), el('th', {}, t('index.failed.col.error')),
        el('th', {}, t('index.failed.col.time')), el('th', { style: 'text-align:right' }, t('col.actions')))), failedBody),
        btn(t('index.retry.all'), () => retry(''))));
    renderCards(st);
    renderProgress(st.progress);
  }

  return () => { disposed = true; for (const stop of stops) stop(); };
}
