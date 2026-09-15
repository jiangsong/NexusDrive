import { api } from '/ui/api.js';
import { el, fill, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { openExtractedText } from '/ui/extracted_text.js';

// The inspector's index line: how the content index sees the selected
// entry, and what can be done about it. main.js hands this module an
// empty host under the entry's other rows and calls it only while the
// daemon reports index.enabled; the request, the wording and the three
// actions live here.

// indexLabel is the state as one phrase. A file has a state of its own; a
// directory has none, so for it the covering rule (or its absence) is the
// answer.
function indexLabel(st) {
  switch (st.state) {
    case 'ok': return t('inspector.index.ok', st.chunks || 0);
    case 'failed': return t('inspector.index.failed', st.error || '');
    case 'pending': case 'dirty': return t('inspector.index.pending');
    case 'uncovered': return t('inspector.index.uncovered');
    default: return st.covered ? t('inspector.index.covered', st.covered) : t('inspector.index.uncovered');
  }
}

// renderIndexInfo asks the daemon how the index sees entry.path and fills
// host with the answer and the actions it allows:
//   add     for an entry no rule covers: one rule for this file or folder
//   remove  only for a rule the console created, and only on the rule's
//           own path; a rule from the configuration file is changed there
//   text    for an indexed file: the extracted text, paged
// A daemon that cannot answer (the index went away between two ticks)
// leaves the host empty rather than saying something wrong.
export async function renderIndexInfo(entry, host) {
  let st;
  try { st = await api.get('/index/status?path=' + encodeURIComponent(entry.path)); } catch (_) { fill(host); return; }
  if (!st.enabled) { fill(host); return; }
  async function add() {
    try {
      await api.post('/index/add', { path: entry.path });
      toast(t('index.added'));
      renderIndexInfo(entry, host);
    } catch (err) { toast(err.message, 'bad'); }
  }
  async function remove() {
    const ok = await confirmDelete({
      title: t('index.rule.remove.title'), body: t('index.rule.remove.body', entry.path),
      confirmToken: entry.path, confirmLabel: t('action.index.remove'),
    });
    if (!ok) return;
    try {
      await api.post('/index/remove', { path: entry.path, confirm: true });
      toast(t('index.rule.removed', entry.path));
      renderIndexInfo(entry, host);
    } catch (err) { toast(err.message, 'bad'); }
  }
  const actions = [
    st.state === 'uncovered' ? el('button', { onclick: add }, t('action.index.add')) : null,
    st.rule_source === 'ui' && st.covered === entry.path ? el('button', { class: 'danger', onclick: remove }, t('action.index.remove')) : null,
    !entry.is_dir && (st.state === 'ok' || st.state === 'dirty') ? el('button', { onclick: () => openExtractedText(entry.path, 0) }, t('action.index.text')) : null,
  ].filter(Boolean);
  fill(host,
    el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px' },
      el('span', { class: 'muted' }, t('inspector.index')),
      el('span', { class: 'detail', style: 'text-align:right;overflow-wrap:anywhere' }, indexLabel(st))),
    actions.length ? el('div', { class: 'row', style: 'flex-wrap:wrap;margin-bottom:10px' }, ...actions) : null);
}
