import { api } from '/ui/api.js';
import { el, fill, iconEl, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { snippetParts } from '/ui/snippet.js';
import { openSendToAgent } from '/ui/send_to_agent.js';

// The "content" mode of the main window's search box: the same box and the
// same result table as the name search, answered by the content index
// (/index/search) instead of the file-name index. name_search.js owns the
// segment control and calls runContentSearch when the "content" segment is
// selected; this module renders the hits and remembers the choice.
//
// A hit's path, heading and snippet come out of files on the drive. They
// are appended as text nodes and the highlighted runs become <mark>
// elements around text; nothing from a file ever reaches innerHTML.

// SEARCH_MODE_KEY remembers "names" or "content" across reloads. Storage
// can be denied (a private window, an embedded view); the default is the
// name search, which is always there, so a failure to remember is not an
// error the person needs to hear about.
export const SEARCH_MODE_KEY = 'cloudfs.search.mode';
export function readSearchMode() {
  try { return localStorage.getItem(SEARCH_MODE_KEY) === 'content' ? 'content' : 'name'; } catch (_) { return 'name'; }
}
export function writeSearchMode(mode) {
  try { localStorage.setItem(SEARCH_MODE_KEY, mode); } catch (_) { /* not remembered; the default is the name search anyway */ }
}

const LIMIT = 50;
const COLUMNS = 4;
// seq tells a slow answer from a query the person has already moved past.
let seq = 0;

// contentSearchHeader is the table header for content hits: the file, the
// heading path inside it, the snippet and its state. The screen swaps it
// in place of the directory header while content results are on screen.
export function contentSearchHeader() {
  return el('tr', {},
    el('th', {}, t('col.name')),
    el('th', {}, t('search.content.heading')),
    el('th', {}, t('search.content.snippet')),
    el('th', { style: 'padding-left:20px' }, t('col.state')));
}

function note(text) {
  return el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim', style: 'text-align:center;padding:12px' }, text));
}
function parentOf(path) {
  return path.replace(/\/[^/]*$/, '') || '/';
}

// resultRow renders one hit: the file with its folder under it, the
// heading path ("Chapter 2 > 2.1"), the snippet with the query words
// marked, and a chip when the indexed text is older than the file.
function resultRow(hit, query, onOpen) {
  const name = hit.path.split('/').pop();
  return el('tr', { 'data-hit': hit.path, style: 'cursor:pointer', onclick: () => onOpen(hit) },
    el('td', {},
      el('span', { style: 'display:flex;align-items:center;gap:10px' },
        el('span', { style: 'color:var(--muted)' }, iconEl('file')),
        el('span', {}, name)),
      el('div', { class: 'dim', style: 'font-size:11.5px;margin:2px 0 0 26px;overflow-wrap:anywhere' }, parentOf(hit.path))),
    el('td', { class: 'dim', style: 'font-size:12.5px;overflow-wrap:anywhere' }, hit.heading || ''),
    el('td', { class: 'detail', style: 'font-size:12.5px;overflow-wrap:anywhere' },
      ...snippetParts(hit.snippet || '', query).map((p) => (p.hit ? el('mark', {}, p.text) : p.text))),
    el('td', { style: 'padding-left:20px;font-size:13px' },
      el('span', { style: 'display:flex;align-items:center;gap:8px;justify-content:space-between' },
        hit.stale ? el('span', { class: 'chip', title: t('search.content.stale.title') }, t('search.content.stale')) : el('span', {}),
        // The row itself opens the extracted text; this button hands the
        // hit to an agent instead, so its click stops here.
        el('button', {
          style: 'padding:4px 8px;flex-shrink:0', title: t('action.sendtoagent'), 'aria-label': t('action.sendtoagent'),
          onclick: (ev) => { ev.stopPropagation(); openSendToAgent({ path: hit.path, heading: hit.heading }); },
        }, iconEl('bot')))));
}

// runContentSearch asks the index for the query and fills the rows.
// Options:
//   rows      the <tbody> shared with the directory listing
//   query     what is in the box
//   path      (optional) a subtree to narrow the hits to; omitted, the
//             whole drive is searched, which is what the box says it does
//   onSearch  (optional) called before the rows are filled, for the header
//   onOpen(hit) a row was clicked: show the extracted text at the hit
// A degraded answer (no semantic search configured) and a truncated one
// (a budget stopped the search) are properties of this result set, so
// they stay on screen with the rows instead of fading out of a toast.
export async function runContentSearch({ rows, query, path, onSearch, onOpen }) {
  const mine = ++seq;
  const root = path || '';
  let r;
  try {
    r = await api.get('/index/search?q=' + encodeURIComponent(query) + '&limit=' + LIMIT
      + (root ? '&path=' + encodeURIComponent(root) : ''));
  } catch (err) { toast(err.message, 'bad'); return; }
  if (mine !== seq) return;
  if (onSearch) onSearch();
  const hits = r.hits || [];
  fill(rows, ...hits.map((hit) => resultRow(hit, query, onOpen)));
  if (!hits.length) rows.append(note(t('empty')));
  if (r.degraded) rows.append(note(t('search.content.degraded')));
  if (r.truncated) rows.append(note(t('search.content.truncated')));
}
