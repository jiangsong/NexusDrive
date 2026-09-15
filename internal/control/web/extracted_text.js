import { api } from '/ui/api.js';
import { el, showPanel, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The extracted-text panel: what the index holds for one file, a page at
// a time from /index/text. A content search opens it at the hit, the
// inspector opens it at the top. The text is a file's content and is
// appended as text nodes only.

// PAGE is one request's worth of text: the daemon's own default page.
const PAGE = 65536;
// LEAD is how far before a hit the first page starts, so the hit sits in
// context rather than on the first line.
const LEAD = 1024;

// utf8Len is the byte length of one code point in UTF-8. Offsets from the
// index are bytes into the text; the page is a JS string.
function utf8Len(cp) {
  const c = cp.codePointAt(0);
  return c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
}

// splitAtBytes cuts text into [before, inside, after] around the byte
// window [from, to) counted from the text's first character. Each cut
// lands on a code point boundary, so a window that starts mid-character
// simply starts at the next one.
function splitAtBytes(text, from, to) {
  const out = ['', '', ''];
  let bytes = 0;
  for (const cp of text) {
    out[bytes < from ? 0 : bytes < to ? 1 : 2] += cp;
    bytes += utf8Len(cp);
  }
  return out;
}

// openExtractedText shows the text of path from startOff, with the bytes
// [startOff, endOff) marked and scrolled into view. startOff 0 shows the
// document from its beginning. "Load more" appends the next page until
// the daemon says eof.
export async function openExtractedText(path, startOff, endOff) {
  const hitFrom = Math.max(0, startOff || 0);
  const hitTo = Math.max(hitFrom, endOff || 0);
  let offset = Math.max(0, hitFrom - LEAD);
  const body = el('pre', { class: 'detail', style: 'margin:0;max-height:60vh;overflow:auto;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,monospace;font-size:12.5px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:10px' });
  const more = el('button', { hidden: true, style: 'margin-top:10px' }, t('index.text.more'));
  const anchor = el('mark', {});
  let marked = false;
  async function load() {
    more.disabled = true;
    let r;
    try {
      r = await api.get('/index/text?path=' + encodeURIComponent(path) + '&offset=' + offset + '&max_bytes=' + PAGE);
    } catch (err) { toast(err.message, 'bad'); more.disabled = false; return; }
    // The hit is marked on the page that holds its start; a chunk that
    // runs past the page end is marked as far as the page goes.
    if (!marked && (hitFrom > 0 || hitTo > 0) && hitFrom >= offset && hitFrom < r.next_offset) {
      const [before, inside, after] = splitAtBytes(r.text, hitFrom - offset, hitTo - offset);
      anchor.append(document.createTextNode(inside));
      body.append(document.createTextNode(before), anchor, document.createTextNode(after));
      marked = true;
      anchor.scrollIntoView({ block: 'center' });
    } else {
      body.append(document.createTextNode(r.text));
    }
    if (!body.textContent && r.eof) body.append(document.createTextNode(t('preview.empty')));
    offset = r.next_offset;
    more.hidden = !!r.eof;
    more.disabled = false;
  }
  more.addEventListener('click', load);
  const panel = showPanel({
    title: t('index.text.title', path.split('/').pop()),
    width: 720,
    content: el('div', {},
      offset > 0 ? el('div', { class: 'dim', style: 'font-size:12px;margin-bottom:8px' }, t('index.text.from', offset)) : null,
      body, more),
  });
  await load();
  return panel;
}
