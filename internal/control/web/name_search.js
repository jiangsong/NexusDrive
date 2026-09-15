import { api } from '/ui/api.js';
import { el, fill, iconEl, bytes, toast, confirmDelete } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { parseQueryString, buildQueryString, searchURL, highlightParts, sizeRange, splitSizeRange, pushRecent } from '/ui/search_query.js';

// The name search of the main window: Everything's two rules, on this page.
// The index is the whole drive, so the default scope is the whole drive and
// a line above the results says how much of the tree the index has seen;
// typing is searching, so the box debounces into a request and the rows
// carry everything the daemon knows about a hit. main.js hands this module
// the box, the table body and three callbacks and wires nothing else; the
// request, the scope control, the shortcut keys, the sortable header and
// the coverage line all live here.

// SEARCH_SCOPE_KEY remembers "whole drive" or "this folder" across reloads.
// localStorage can be denied (a private window, an embedded view) and the
// default is the whole drive anyway, so a failure to remember is not an
// error the person needs to hear about.
export const SEARCH_SCOPE_KEY = 'cloudfs.search.scope';
export function readSearchScope() {
  try { return localStorage.getItem(SEARCH_SCOPE_KEY) === 'cwd' ? 'cwd' : 'all'; } catch (_) { return 'all'; }
}
export function writeSearchScope(scope) {
  try { localStorage.setItem(SEARCH_SCOPE_KEY, scope); } catch (_) { /* not remembered; the default is the whole drive anyway */ }
}

// RECENT_KEY holds the last ten queries, most recent first. The list is
// bounded by pushRecent and guarded the same way as the scope: a browser
// that refuses storage, or one whose stored value is not a list, gets an
// empty history and a search box that still searches.
export const RECENT_KEY = 'cloudfs.search.recent';
export function readRecent() {
  try {
    const list = JSON.parse(localStorage.getItem(RECENT_KEY) || '[]');
    return Array.isArray(list) ? list.filter((q) => typeof q === 'string').slice(0, 10) : [];
  } catch (_) { return []; }
}
export function writeRecent(list) {
  try { localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, 10))); } catch (_) { /* not remembered */ }
}

// DATE_ONLY is what the date control can show: a modified-after filter on
// one calendar day. Any other dm: value (a range, a month, a "before")
// stays in the query string, which the person can edit by hand.
const DATE_ONLY = /^>=?(\d{4}-\d{2}-\d{2})$/;

// DEBOUNCE_MS is how long the box waits after a keystroke. A name query
// answers in a few milliseconds; the wait is for the next key, not the
// daemon.
const DEBOUNCE_MS = 250;
const LIMIT = 100;
const COLUMNS = 4;

// mountNameSearch owns everything between the search box and the result
// rows. Options:
//   searchBox   the <input type=search> main.js put in the header bar
//   rows        the <tbody> the directory listing also fills
//   getCwd      the directory on screen, for the "this folder" scope
//   onClear     the box is empty again: show the directory
//   onSearch    a name search is about to render: swap in the sort header
//   onOpen(hit) a result was double-clicked: go to its folder
//   onSelect(hit, tr) a result was clicked: show it in the inspector
//   extraModes  (B12) segments after "whole drive / this folder", each
//               { id, when(), label, active(), select(on), run(query) };
//               a selected extra mode answers the query itself.
// It returns the scope control (segments, the filter toggle and the recent
// list), the filter bar, the status line and the sortable header for the
// screen to place.
export function mountNameSearch({ searchBox, rows, getCwd, onClear, onSearch, onOpen, onSelect, extraModes = [] }) {
  let scope = readSearchScope();
  let sort = '';
  let parsed = parseQueryString('');
  let timer;
  // seq tells a slow answer from a query the person has already moved past.
  let seq = 0;

  const scopeControl = el('div', { class: 'row', role: 'group', 'aria-label': t('search.scope'), style: 'gap:4px' });
  const statusLine = el('div', { class: 'dim', role: 'status', style: 'display:none;align-items:center;gap:6px;flex-wrap:wrap;padding:8px 18px;font-size:12px;border-bottom:1px solid var(--border)' });
  const header = el('tr', { 'aria-label': t('search.sort') });

  // The filter bar is the query grammar with controls on it. Every change
  // is written into the box as key:value words, so what the person sees in
  // the box is exactly what is sent, and a hand edit there flows back into
  // the controls on the next run. The recent list is a datalist on the box.
  const recentList = el('datalist', { id: 'search-recent' });
  searchBox.setAttribute('list', 'search-recent');
  const kindSelect = el('select', { 'aria-label': t('search.filter.kind'), onchange: applyFilters },
    el('option', { value: '' }, t('search.filter.kind.any')),
    el('option', { value: 'file' }, t('search.filter.kind.file')),
    el('option', { value: 'dir' }, t('search.filter.kind.dir')));
  const extInput = el('input', { type: 'text', 'aria-label': t('search.filter.ext'), placeholder: 'go,md', style: 'width:90px', onchange: applyFilters });
  const minInput = el('input', { type: 'text', 'aria-label': t('search.filter.size.min'), placeholder: '1m', style: 'width:70px', onchange: applyFilters });
  const maxInput = el('input', { type: 'text', 'aria-label': t('search.filter.size.max'), placeholder: '10m', style: 'width:70px', onchange: applyFilters });
  const afterInput = el('input', { type: 'date', 'aria-label': t('search.filter.after'), onchange: applyFilters });
  const filterError = el('div', { class: 'dim', role: 'alert', style: 'display:none;font-size:12px;color:var(--warn-text)' });
  const filterToggle = el('button', { 'aria-expanded': 'false', 'aria-controls': 'search-filters', onclick: toggleFilters }, iconEl('filter'), t('search.filters'));
  const filterBar = el('div', { id: 'search-filters', style: 'display:none;align-items:center;gap:14px;flex-wrap:wrap;padding:8px 18px;font-size:12.5px;border-bottom:1px solid var(--border)' },
    labelled(t('search.filter.kind'), kindSelect),
    labelled(t('search.filter.ext'), extInput),
    labelled(t('search.filter.size.min'), minInput),
    labelled(t('search.filter.size.max'), maxInput),
    labelled(t('search.filter.after'), afterInput),
    filterError);
  function labelled(text, control) {
    return el('label', { class: 'row', style: 'gap:6px;align-items:center' }, el('span', { class: 'muted' }, text), control);
  }
  function toggleFilters() {
    const open = filterToggle.getAttribute('aria-expanded') !== 'true';
    filterToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    filterToggle.className = open ? 'primary' : '';
    filterBar.style.display = open ? 'flex' : 'none';
    if (open) syncFilters(parseQueryString(searchBox.value));
  }
  // applyFilters reads the controls into the filters of whatever is in the
  // box right now and writes the whole query back, words first. A filter the
  // controls cannot show (a size range typed as 1m..2m is shown; a dm: range
  // is not) is left as the person wrote it.
  function applyFilters() {
    parsed = parseQueryString(searchBox.value);
    const filters = { ...parsed.filters };
    if (kindSelect.value) filters.kind = kindSelect.value; else delete filters.kind;
    if (extInput.value.trim()) filters.ext = extInput.value.trim(); else delete filters.ext;
    const size = sizeRange(minInput.value, maxInput.value);
    if (size) filters.size = size; else delete filters.size;
    if (afterInput.value) filters.dm = '>' + afterInput.value;
    else if (DATE_ONLY.test(filters.dm || '')) delete filters.dm;
    searchBox.value = buildQueryString(parsed.terms, filters);
    run();
  }
  // syncFilters is the other direction: the box was edited, or a recent
  // query was picked, and the controls follow it.
  function syncFilters(p) {
    const f = p.filters;
    kindSelect.value = f.kind || '';
    extInput.value = f.ext || '';
    const size = splitSizeRange(f.size || '');
    minInput.value = size.min;
    maxInput.value = size.max;
    const m = DATE_ONLY.exec(f.dm || '');
    afterInput.value = m ? m[1] : '';
    filterError.style.display = p.errors.length ? '' : 'none';
    fill(filterError, p.errors.length ? t('search.filter.invalid', p.errors.join(', ')) : '');
  }
  function renderRecent() {
    fill(recentList, ...readRecent().map((q) => el('option', { value: q })));
  }

  function segment(label, active, onclick) {
    return el('button', { class: active ? 'primary' : '', 'aria-pressed': active ? 'true' : 'false', style: 'padding:5px 10px', onclick }, label);
  }
  function currentMode() {
    return extraModes.find((m) => m.when() && m.active());
  }
  // renderScope draws the segments. The two scopes are always there; an
  // extra mode shows only while its when() holds, so a mode whose backing
  // service is off does not offer itself.
  function renderScope() {
    const mode = currentMode();
    fill(scopeControl,
      ...[['all', t('search.scope.all')], ['cwd', t('search.scope.cwd')]].map(([s, label]) =>
        segment(label, !mode && scope === s, () => {
          scope = s;
          writeSearchScope(scope);
          extraModes.forEach((m) => m.select(false));
          renderScope();
          run();
        })),
      ...extraModes.filter((m) => m.when()).map((m) =>
        segment(m.label, m.active(), () => {
          extraModes.forEach((x) => x.select(x === m));
          renderScope();
          run();
        })),
      filterToggle, recentList);
    searchBox.placeholder = scope === 'cwd' && !mode ? t('search.placeholder.cwd') : t('search.placeholder');
  }

  // The header is sortable where the daemon can sort: name, size and
  // modified time. A click asks for the daemon's order for that key (A to
  // Z, largest first, newest first), a second click reverses it, a third
  // goes back to the default order (depth, then path). The state column
  // has no order of its own.
  const SORTS = [['name', t('col.name'), 'ascending'], ['size', t('col.size'), 'descending'], ['mtime', t('col.modified'), 'descending']];
  function renderHeader() {
    const reversed = sort.startsWith('-');
    const active = sort.replace(/^-/, '');
    fill(header,
      ...SORTS.map(([key, label, natural]) => {
        const dir = natural === 'ascending' === !reversed ? 'ascending' : 'descending';
        return el('th', { 'data-sort': key, class: key === 'size' ? 'num' : '', 'aria-sort': active === key ? dir : 'none', style: 'cursor:pointer;user-select:none' + (key === 'mtime' ? ';padding-left:20px' : ''), onclick: () => setSort(key) },
          label, active === key ? (dir === 'ascending' ? ' \u2191' : ' \u2193') : '');
      }),
      el('th', { style: 'padding-left:20px' }, t('col.state')));
  }
  function setSort(key) {
    sort = sort === key ? '-' + key : sort === '-' + key ? '' : key;
    renderHeader();
    run();
  }

  function stateCell(hit) {
    if (hit.kind === 'dir') return el('span', { class: 'dim' }, iconEl('folder'), ' ' + t('inspector.dir'));
    if (hit.cached) return el('span', { class: 'dim' }, el('span', { class: 'dot ok' }), ' ' + t('state.cached'));
    return el('span', { class: 'dim' }, el('span', { class: 'dot' }), ' ' + t('state.remote'));
  }
  function parentOf(path) {
    return path.replace(/\/[^/]*$/, '') || '/';
  }

  // resultRow renders one hit. The name and its folder come from the
  // daemon and are appended as text nodes; the highlighted runs become
  // <mark> elements around text, never markup.
  function resultRow(hit) {
    const tr = el('tr', { 'data-hit': hit.path, onclick: () => onSelect(hit, tr) },
      el('td', {},
        el('span', { style: 'display:flex;align-items:center;gap:10px' },
          el('span', { style: 'color:' + (hit.kind === 'dir' ? 'var(--accent-text)' : 'var(--muted)') }, iconEl(hit.kind === 'dir' ? 'folder' : 'file')),
          el('span', {}, ...highlightParts(hit.name, parsed).map((p) => (p.hit ? el('mark', {}, p.text) : p.text)))),
        el('div', { class: 'dim', style: 'font-size:11.5px;margin:2px 0 0 26px;overflow-wrap:anywhere' }, parentOf(hit.path))),
      el('td', { class: 'num dim', 'data-size': String(hit.size) }, hit.kind === 'dir' ? '—' : bytes(hit.size)),
      el('td', { class: 'detail', style: 'padding-left:20px' }, new Date(hit.mtime).toLocaleDateString(locale())),
      el('td', { style: 'padding-left:20px;font-size:13px' }, stateCell(hit)));
    tr.addEventListener('dblclick', () => {
      // Opening the folder leaves search: the rows are the directory now,
      // and a coverage line about results that are gone would mislead.
      searchBox.value = '';
      statusLine.style.display = 'none';
      onOpen(hit);
    });
    return tr;
  }

  async function run() {
    const query = searchBox.value.trim();
    const mine = ++seq;
    if (!query) { statusLine.style.display = 'none'; syncFilters(parseQueryString('')); onClear(); return; }
    parsed = parseQueryString(query);
    // The controls follow the box, and a value the bar cannot read is named
    // under it. The request still goes out: the daemon's own refusal is the
    // authoritative one and arrives as a toast.
    syncFilters(parsed);
    const mode = currentMode();
    // An extra mode answers the query itself; the coverage line describes
    // the name index and would be wrong under its rows.
    if (mode) { statusLine.style.display = 'none'; await mode.run(query); return; }
    let r;
    try { r = await api.get(searchURL({ query, scope, cwd: getCwd(), sort, limit: LIMIT })); } catch (err) { toast(err.message, 'bad'); return; }
    if (mine !== seq) return;
    writeRecent(pushRecent(readRecent(), query));
    renderRecent();
    if (onSearch) onSearch();
    const hits = r.results || [];
    fill(rows, ...hits.map(resultRow));
    if (!hits.length) fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('empty'))));
    // Truncation is a property of this result set, so it stays on screen
    // with the rows instead of fading out of a toast.
    if (!r.complete) rows.append(el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim', style: 'text-align:center;padding:12px' }, t('search.truncated'))));
    renderCoverage(r, hits.length);
  }

  // renderCoverage is the line above the results: how many hits, and how
  // much of the tree the index holds. The index only knows directories
  // somebody listed; when that is not all of them and no crawl is running,
  // the offer to list the rest sits right here, where the gap is visible.
  function renderCoverage(r, count) {
    if (!r.coverage) { statusLine.style.display = 'none'; return; }
    const full = r.coverage.listed >= r.coverage.known;
    statusLine.style.display = 'flex';
    fill(statusLine,
      el('span', {}, t('search.count', count)),
      el('span', {}, '·'),
      el('span', {}, t('search.coverage', r.coverage.listed, r.coverage.known)),
      r.coverage.crawling ? el('span', {}, '·') : null,
      r.coverage.crawling ? el('span', {}, t('search.coverage.crawling')) : null,
      full || r.coverage.crawling ? null : el('button', { style: 'margin-left:6px;padding:3px 10px', onclick: indexWholeTree }, t('search.coverage.index')));
  }

  // indexWholeTree is one provider call per directory the index lacks, on
  // every mount. That is a lot of calls on a large drive, and on an
  // unofficial API it is the kind of traffic that gets an account flagged,
  // so it goes through the typed confirmation and sends the confirm flag
  // the daemon insists on for depth -1.
  async function indexWholeTree() {
    const ok = await confirmDelete({ title: t('confirm.warm.title'), body: t('confirm.warm.body'), confirmToken: 'warm', confirmLabel: t('search.coverage.index'), danger: false });
    if (!ok) return;
    try {
      const r = await api.post('/cache/warm', { path: '/', depth: -1, all: true, confirm: true });
      toast(r.queued ? t('toast.warm.started') : t('toast.warmed', r.directories || 0));
      run();
    } catch (err) { toast(err.message, 'bad'); }
  }

  // Ctrl/⌘+K reaches the box from anywhere on the screen; Escape in the
  // box clears it, which is how the directory comes back.
  function onKey(ev) {
    if ((ev.metaKey || ev.ctrlKey) && (ev.key || '').toLowerCase() === 'k') {
      ev.preventDefault();
      searchBox.focus();
      searchBox.select();
    } else if (ev.key === 'Escape' && document.activeElement === searchBox && searchBox.value) {
      // A search input clears itself on Escape and fires input for it;
      // doing it here, once, keeps the directory from loading twice.
      ev.preventDefault();
      searchBox.value = '';
      run();
    }
  }

  searchBox.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(run, DEBOUNCE_MS); });
  document.addEventListener('keydown', onKey);
  renderScope();
  renderHeader();
  renderRecent();
  return {
    scopeControl, filterBar, statusLine, header, run,
    refresh: renderScope,
    dispose() { clearTimeout(timer); document.removeEventListener('keydown', onKey); },
  };
}
