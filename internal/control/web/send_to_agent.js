import { api } from '/ui/api.js';
import { el, openPanel, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The "send to agent" panel: a prompt for one path, ready to paste into an
// MCP client that has this daemon registered. The daemon writes the prompt
// (/agent/prompt) so it names the file the way the MCP tools do and only
// mentions tools this daemon offers; the person edits it here and copies
// it. The copy path is the whole panel on a daemon without agents[]: the
// only requests are the GET for the prompt and the GET for the agent
// names, nothing runs, nothing is written.
//
// When /agent/endpoints names at least one agent, a select and a run
// button appear under the copy button. Running hands the edited prompt and
// the path to that agent's local command, so it is confirmed by typing the
// agent's name — the same sheet a deletion uses — and posts confirm: true.
// The answer is a delivery id the triggers screen can show.
//
// The prompt goes into the textarea as its value and the agent names into
// option text: both come from the daemon and never touch innerHTML.

// openSendToAgent opens the panel for path. heading, from a content-search
// hit, points the prompt at the passage the search found.
export async function openSendToAgent({ path, heading }) {
  let r;
  try {
    r = await api.get('/agent/prompt?path=' + encodeURIComponent(path)
      + (heading ? '&heading=' + encodeURIComponent(heading) : ''));
  } catch (err) { toast(err.message, 'bad'); return; }
  // A daemon without an engine answers an empty list; a daemon that cannot
  // answer at all still gets the copy panel.
  let agents;
  try { agents = (await api.get('/agent/endpoints')).agents || []; } catch (_) { agents = []; }

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
      textarea,
      agents.length ? runRow(agents, path, textarea) : null),
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, done, copy),
    onEscape: () => close(),
  });
  done.addEventListener('click', () => close());
  textarea.focus();
}

// runRow is the agent select and the run button, built only when the
// daemon named at least one agent.
function runRow(agents, path, textarea) {
  const select = el('select', { 'aria-label': t('agent.run.agent') },
    ...agents.map((a) => el('option', { value: a.name }, a.name)));
  const run = el('button', { onclick: () => runAgent(select.value, path, textarea, run) }, t('agent.run.button'));
  return el('div', { class: 'row', style: 'margin-top:12px;align-items:center;gap:8px' },
    el('span', { class: 'dim', style: 'font-size:12px' }, t('agent.run.label')), select, run);
}

// runAgent confirms, posts, and tells the person where the delivery is.
async function runAgent(agent, path, textarea, button) {
  if (!agent) return;
  const sure = await confirmDelete({
    title: t('agent.run.confirm.title'),
    body: t('agent.run.confirm.body', agent, path),
    confirmToken: agent,
    confirmLabel: t('agent.run.button'),
  });
  if (!sure) return;
  button.disabled = true;
  try {
    const r = await api.post('/agent/invoke', { agent, paths: [path], prompt: textarea.value, confirm: true });
    toast(el('span', {}, t('agent.run.submitted'), ' ',
      el('a', { href: '#/triggers?delivery=' + r.id }, t('agent.run.view'))));
  } catch (e) {
    if (e.status === 409) toast(t('agent.run.queued'), 'bad');
    else toast(e.message, 'bad');
  } finally {
    button.disabled = false;
  }
}
