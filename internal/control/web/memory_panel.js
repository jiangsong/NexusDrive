import { api } from '/ui/api.js';
import { el, fill, toast, openPanel, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { copiesOf, byteCount, splitFrontmatter, mergeDraft } from '/ui/memory_conflicts.js';

// The two overlays of the memory tab (docs/ui-plan.md F7-2 and F7-4,
// TODO.md T-40): the editor for one fact, and the side-by-side view of a
// fact and one of its conflict copies.
//
// The editor writes back with the version it read. The daemon refuses a
// put whose expected_version is stale with a 409, and that refusal is the
// whole point: a fact edited on two devices must not be overwritten by
// whichever save lands last. The 409 becomes a line in the sheet and a
// "reload" that re-reads the fact and discards the draft — and says so —
// rather than a retry that would drop the other device's edit.
//
// Everything shown here was written by an agent or by a drive: the body,
// the description, the copy's bytes. It all goes in as text nodes, and the
// textarea is filled through .value.
//
// A conflict copy has a name outside the fact grammar, so it is read
// through /fs/preview and removed through /fs/delete, the routes the main
// window already uses for any file; the memory routes only ever see the
// fact itself.

// COPY_BYTES is the most of a copy /fs/preview hands back in one call
// (previewMax in internal/control/fs.go); a fact is capped far below it.
const COPY_BYTES = 1 << 20;
const BINARY_RE = /[\u0000-\u0008\u000e-\u001f\ufffd]/;
const PRE_STYLE = 'margin:0;max-height:45vh;overflow:auto;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,monospace;font-size:12.5px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:10px';

// factURL is GET|PUT|DELETE /memory/{agent}/{name}, with both segments
// encoded even though the grammar allows nothing that needs it.
export function factURL(agent, name) {
  return '/memory/' + encodeURIComponent(agent) + '/' + encodeURIComponent(name);
}

function labelled(label, control) {
  return el('div', {}, el('label', { style: 'display:block;font-size:12px;margin-bottom:4px' }, label), control);
}

// openMemoryEditor fetches the fact and opens it. draft, when given,
// replaces the body in the textarea (the merge-by-hand path); maxBytes is
// memory.max_fact_bytes for the counter; onChanged runs after a save, a
// delete or a resolved conflict so the list behind the sheet can reload.
export async function openMemoryEditor({ agent, name, maxBytes = 0, draft, onChanged }) {
  let fact;
  try {
    fact = await api.get(factURL(agent, name));
  } catch (err) {
    toast(err.message, 'bad');
    return null;
  }
  let version = fact.version || '';
  let close = null;

  const description = el('input', { type: 'text', autocomplete: 'off', spellcheck: 'false' });
  const type = el('input', { type: 'text', autocomplete: 'off', spellcheck: 'false', placeholder: 'preference' });
  const body = el('textarea', { rows: '16', spellcheck: 'false', style: 'width:100%;box-sizing:border-box;font-family:ui-monospace,monospace;font-size:12.5px;resize:vertical' });
  const counter = el('span', { class: 'detail tnums', role: 'status' });
  const notice = el('div', { role: 'alert', style: 'display:none;margin-top:10px' });
  const resolveBtn = el('button', { onclick: () => resolve() }, t('memory.resolve'));

  function count() {
    const n = byteCount(body.value);
    counter.textContent = maxBytes ? t('memory.bytes', String(n), String(maxBytes)) : t('memory.bytes.nolimit', String(n));
    counter.style.color = maxBytes && n > maxBytes ? 'var(--bad)' : '';
  }

  // fillFrom puts a fact into the controls. The body goes through .value:
  // it is the agent's text and is never parsed as markup.
  function fillFrom(f) {
    const m = f.meta || {};
    description.value = m.description || '';
    type.value = m.type || '';
    body.value = typeof f.content === 'string' ? f.content : '';
    version = f.version || '';
    resolveBtn.style.display = copiesOf(f).length ? '' : 'none';
    count();
  }

  async function reload() {
    try {
      fact = await api.get(factURL(agent, name));
    } catch (err) {
      toast(err.message, 'bad');
      return;
    }
    fillFrom(fact);
    notice.style.display = 'none';
    fill(notice);
    toast(t('memory.reloaded'));
  }

  async function save() {
    try {
      const saved = await api.put(factURL(agent, name), {
        content: body.value, description: description.value.trim(), type: type.value.trim(),
        expected_version: version,
      });
      fact = saved;
      version = saved.version || '';
      resolveBtn.style.display = copiesOf(saved).length ? '' : 'none';
      toast(t('memory.saved', name));
      if (onChanged) onChanged();
    } catch (err) {
      if (err.status === 409) {
        // The 409 body names the version the fact has now; the reload
        // fetches the fact whole, so the number itself is not shown.
        fill(notice,
          el('div', { class: 'banner warn' },
            el('div', {}, t('memory.changed_elsewhere')),
            el('div', { class: 'detail', style: 'margin-top:6px' }, t('memory.reload.discards')),
            el('div', { style: 'margin-top:8px' }, el('button', { onclick: reload }, t('memory.reload')))));
        notice.style.display = '';
        return;
      }
      toast(err.message, 'bad');
    }
  }

  async function remove() {
    const ok = await confirmDelete({
      title: t('memory.delete.title', name), body: t('memory.delete.body'),
      confirmToken: name, confirmLabel: t('memory.delete'),
    });
    if (!ok) return;
    try {
      await api.del(factURL(agent, name), { confirm: true });
    } catch (err) {
      toast(err.message, 'bad');
      return;
    }
    toast(t('memory.deleted', name));
    if (close) close();
    if (onChanged) onChanged();
  }

  // resolve opens the conflict overlay over the editor. A merge lands in
  // this textarea; any other outcome re-reads the fact.
  function resolve() {
    openConflictOverlay({
      agent, name, fact, maxBytes,
      onChanged: () => { reload(); if (onChanged) onChanged(); },
      onMerge: (text) => { body.value = text; count(); toast(t('memory.merge.note')); },
    });
  }

  fillFrom(fact);
  if (typeof draft === 'string') { body.value = draft; count(); }
  body.addEventListener('input', count);

  const done = el('button', { class: 'primary' }, t('conn.close'));
  done.addEventListener('click', () => { if (close) close(); });
  const content = el('div', { style: 'display:grid;gap:10px' },
    el('div', { class: 'row', style: 'gap:12px;align-items:baseline;flex-wrap:wrap' },
      el('span', { class: 'muted', style: 'font-size:12px' }, t('memory.path')),
      el('code', { class: 'detail', style: 'font-size:12px;word-break:break-all' }, fact.path || '')),
    el('div', { style: 'display:grid;grid-template-columns:1fr 1fr;gap:10px' },
      labelled(t('memory.form.description'), description), labelled(t('memory.form.type'), type)),
    labelled(t('memory.form.content'), body),
    el('div', { class: 'row', style: 'justify-content:space-between;align-items:baseline;flex-wrap:wrap' },
      counter, el('span', { class: 'dim', style: 'font-size:11.5px' }, t('memory.bytes.note'))),
    notice);
  const closePanel = openPanel({
    title: t('memory.edit.title', agent, name),
    content,
    width: 760,
    footer: el('div', { class: 'row', style: 'margin-top:16px;align-items:center' },
      el('button', { class: 'danger', onclick: remove }, t('memory.delete')),
      resolveBtn,
      el('div', { class: 'grow' }),
      done,
      el('button', { class: 'primary', onclick: save }, t('memory.save'))),
    onEscape: () => close(),
  });
  close = (result) => closePanel(result);
  return close;
}

// openConflictOverlay shows one fact beside one of its copies and offers
// the three ways out. fact may be passed by the editor that already holds
// it; otherwise it is read. With several copies a select at the top picks
// which one is on the right. onMerge, when given, receives the merge draft
// instead of a new editor being opened (the editor underneath takes it).
export async function openConflictOverlay({ agent, name, fact, maxBytes = 0, onChanged, onMerge }) {
  if (!fact) {
    try {
      fact = await api.get(factURL(agent, name));
    } catch (err) {
      toast(err.message, 'bad');
      return null;
    }
  }
  const copies = copiesOf(fact);
  if (!copies.length) {
    toast(t('memory.copy.removed'));
    return null;
  }
  let copy = copies[0];
  let copyText = null;
  let close = null;

  const mine = el('pre', { style: PRE_STYLE }, typeof fact.content === 'string' ? fact.content : '');
  const theirs = el('pre', { style: PRE_STYLE }, t('memory.copy.reading'));
  const copyPath = el('code', { class: 'detail', style: 'font-size:12px;word-break:break-all' });
  const useBtn = el('button', { disabled: true, onclick: () => useCopy() }, t('memory.use_copy'));
  const mergeBtn = el('button', { disabled: true, onclick: () => mergeByHand() }, t('memory.merge'));

  // readCopy fetches the copy's bytes through the file preview. Its
  // frontmatter, when it has one, is split off so the right pane and the
  // put both carry the body alone.
  async function readCopy() {
    copyText = null;
    useBtn.disabled = mergeBtn.disabled = true;
    copyPath.textContent = copy;
    fill(theirs, t('memory.copy.reading'));
    let text;
    try {
      text = await api.get('/fs/preview?path=' + encodeURIComponent(copy) + '&length=' + COPY_BYTES);
    } catch (err) {
      fill(theirs, t('memory.copy.read.failed', err.message));
      return;
    }
    if (typeof text !== 'string' || BINARY_RE.test(text)) {
      fill(theirs, t('memory.copy.binary'));
      return;
    }
    copyText = text;
    fill(theirs, splitFrontmatter(text).body);
    useBtn.disabled = mergeBtn.disabled = false;
  }

  async function keepMine() {
    const ok = await confirmDelete({
      title: t('memory.keep_mine.title'), body: t('memory.keep_mine.body', copy),
      confirmToken: name, confirmLabel: t('memory.keep_mine'),
    });
    if (!ok) return;
    try {
      await api.post('/fs/delete', { path: copy, confirm: true });
    } catch (err) {
      toast(err.message, 'bad');
      return;
    }
    toast(t('memory.copy.removed'));
    if (close) close();
    if (onChanged) onChanged();
  }

  async function useCopy() {
    if (copyText === null) return;
    const ok = await confirmDelete({
      title: t('memory.use_copy.title'), body: t('memory.use_copy.body'),
      confirmToken: name, confirmLabel: t('memory.use_copy'),
    });
    if (!ok) return;
    const parsed = splitFrontmatter(copyText);
    try {
      // The put carries the version read into this overlay: if the fact
      // moved on meanwhile, the daemon refuses and the copy is left alone.
      await api.put(factURL(agent, name), {
        content: parsed.body, description: parsed.meta.description || '', type: parsed.meta.type || '',
        expected_version: fact.version || '',
      });
    } catch (err) {
      toast(err.status === 409 ? t('memory.changed_elsewhere') : err.message, 'bad');
      return;
    }
    try {
      await api.post('/fs/delete', { path: copy, confirm: true });
    } catch (err) {
      toast(err.message, 'bad');
    }
    toast(t('memory.copy.used'));
    if (close) close();
    if (onChanged) onChanged();
  }

  function mergeByHand() {
    if (copyText === null) return;
    const theirBody = splitFrontmatter(copyText).body;
    if (close) close();
    if (onMerge) {
      onMerge(mergeDraft(fact.content, theirBody));
      return;
    }
    openMemoryEditor({ agent, name, maxBytes, draft: mergeDraft(fact.content, theirBody), onChanged });
  }

  const picker = copies.length > 1
    ? el('select', { onchange: (ev) => { copy = ev.target.value; readCopy(); } },
      ...copies.map((c) => el('option', { value: c }, c.slice(c.lastIndexOf('/') + 1))))
    : null;
  const done = el('button', { class: 'primary' }, t('conn.close'));
  done.addEventListener('click', () => { if (close) close(); });
  const content = el('div', { style: 'display:grid;gap:10px' },
    el('p', { class: 'detail', style: 'margin:0' }, t('memory.resolve.body')),
    picker ? el('div', { class: 'row', style: 'gap:10px;align-items:center' }, el('span', { class: 'muted', style: 'font-size:12px' }, t('memory.resolve.which')), picker) : null,
    el('div', { style: 'display:grid;grid-template-columns:1fr 1fr;gap:12px' },
      el('div', {},
        el('div', { class: 'eyebrow', style: 'margin-bottom:6px' }, t('memory.mine')),
        el('code', { class: 'detail', style: 'font-size:12px;word-break:break-all;display:block;margin-bottom:6px' }, fact.path || ''),
        mine),
      el('div', {},
        el('div', { class: 'eyebrow', style: 'margin-bottom:6px' }, t('memory.copy')),
        el('div', { style: 'display:block;margin-bottom:6px' }, copyPath),
        theirs)));
  const closePanel = openPanel({
    title: t('memory.resolve.title', name),
    content,
    width: 960,
    footer: el('div', { class: 'row', style: 'margin-top:16px;align-items:center;flex-wrap:wrap' },
      el('button', { class: 'danger', onclick: keepMine }, t('memory.keep_mine')),
      useBtn, mergeBtn,
      el('div', { class: 'grow' }),
      done),
    onEscape: () => close(),
  });
  close = (result) => closePanel(result);
  readCopy();
  return close;
}
