import { api } from '/ui/api.js';
import { el, openPanel, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The "send to agent" panel: a prompt for one path, ready to paste into an
// MCP client that has this daemon registered. The daemon writes the prompt
// (/agent/prompt) so it names the file the way the MCP tools do and only
// mentions tools this daemon offers; the person edits it here and copies
// it. That is the whole panel in phase one — nothing runs, nothing is
// written, the only request is the GET for the prompt. The run button that
// hands the prompt to a local agent is phase two (T-41/T-42); nothing of it
// is rendered here.
//
// The prompt goes into the textarea as its value: it contains the path,
// which comes from the drive, and never touches innerHTML.

// openSendToAgent opens the panel for path. heading, from a content-search
// hit, points the prompt at the passage the search found.
export async function openSendToAgent({ path, heading }) {
  let r;
  try {
    r = await api.get('/agent/prompt?path=' + encodeURIComponent(path)
      + (heading ? '&heading=' + encodeURIComponent(heading) : ''));
  } catch (err) { toast(err.message, 'bad'); return; }
  const textarea = el('textarea', { rows: '12', style: 'width:100%;box-sizing:border-box;resize:vertical', 'aria-label': t('agent.prompt.title'), spellcheck: 'false' });
  textarea.value = r.prompt;
  const copy = el('button', {
    class: 'primary',
    onclick: async () => {
      try { await navigator.clipboard.writeText(textarea.value); toast(t('agent.prompt.copied')); }
      catch (_) { toast(t('agent.prompt.copyfailed'), 'bad'); }
    },
  }, t('agent.prompt.copy'));
  const done = el('button', {}, t('conn.close'));
  const close = openPanel({
    title: t('agent.prompt.title'),
    width: 560,
    content: el('div', {},
      el('p', { class: 'detail', style: 'margin-top:0' }, t('agent.prompt.body')),
      el('div', { class: 'dim', style: 'font-size:12px;margin-bottom:8px;overflow-wrap:anywhere' }, r.uri),
      textarea),
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, done, copy),
    onEscape: () => close(),
  });
  done.addEventListener('click', () => close());
  textarea.focus();
}
