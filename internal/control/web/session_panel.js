import { api } from '/ui/api.js';
import { el, openPanel, toast } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { scopeParts } from '/ui/scope_view.js';

// The session panel: one MCP session in detail — who it is, what it may
// touch, and the last fifty things it did — with the one action a person
// can take on it, finishing it. GET /sessions/<id> answers all of that in
// one round trip, so the panel opens on a single request.
//
// Everything shown here was written by the agent or its client (client
// name, tool names, paths, error text). It all goes in as text nodes.
const TAIL_COLUMNS = 4;

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

function auditTailRow(r) {
  const paths = r.paths || [];
  return el('tr', { class: r.result === 'denied' ? 'denied' : '', 'data-result': r.result },
    el('td', { class: 'dim tnums', style: 'white-space:nowrap' }, when(r.ts)),
    el('td', {}, r.tool),
    el('td', { class: 'detail', style: 'word-break:break-all' }, paths.join(', ')),
    el('td', {}, t('audit.result.' + r.result)));
}

// openSessionPanel fetches the session and opens it. onFinished, when
// given, runs after a successful finish so the list behind the panel can
// reload without waiting for the session event.
export async function openSessionPanel(id, onFinished) {
  let d;
  try {
    d = await api.get('/sessions/' + encodeURIComponent(id));
  } catch (err) {
    toast(err.message, 'bad');
    return null;
  }
  const s = d.session || {};
  const scope = scopeParts(s.scope).map((p) => t(p.key, ...p.args)).join(' · ');
  let close = null;

  async function finish() {
    try {
      await api.post('/sessions/' + encodeURIComponent(id) + '/finish', {});
      toast(t('session.finished.toast'));
      if (close) close();
      if (onFinished) onFinished();
    } catch (err) {
      toast(err.message, 'bad');
    }
  }

  const finishBtn = s.state === 'active' ? el('button', { class: 'danger', onclick: finish }, t('session.finish')) : null;
  const done = el('button', { class: 'primary' }, t('conn.close'));
  done.addEventListener('click', () => { if (close) close(); });

  const facts = el('div', { style: 'display:grid;grid-template-columns:max-content 1fr;gap:6px 14px;font-size:13px' },
    el('div', { class: 'muted' }, t('session.col.client')),
    el('div', { class: 'detail' }, (s.client || s.id || '') + (s.client_version ? ' ' + s.client_version : '')),
    el('div', { class: 'muted' }, t('session.transport')),
    el('div', { class: 'detail' }, s.transport || ''),
    el('div', { class: 'muted' }, t('session.col.scope')),
    el('div', { class: 'detail' }, scope),
    el('div', { class: 'muted' }, t('session.col.state')),
    el('div', { class: 'detail' }, t('session.state.' + s.state)),
    el('div', { class: 'muted' }, t('session.col.started')),
    el('div', { class: 'detail tnums' }, when(s.started_at)),
    s.finished_at ? el('div', { class: 'muted' }, t('session.finished_at')) : null,
    s.finished_at ? el('div', { class: 'detail tnums' }, when(s.finished_at)) : null,
    el('div', { class: 'muted' }, t('session.col.writes')),
    el('div', { class: 'detail tnums' }, String(s.writes || 0)),
    s.workspace ? el('div', { class: 'muted' }, t('session.workspace')) : null,
    s.workspace ? el('div', { class: 'detail' }, s.workspace) : null,
    s.summary ? el('div', { class: 'muted' }, t('session.summary')) : null,
    s.summary ? el('div', { class: 'detail' }, s.summary) : null);

  const tail = d.audit || [];
  const content = el('div', {},
    el('div', { class: 'dim', style: 'font-size:12px;margin-bottom:10px;word-break:break-all' }, s.id || id),
    facts,
    el('div', { class: 'eyebrow', style: 'margin:16px 0 6px' }, t('session.audit.tail')),
    el('div', { class: 'panel', style: 'overflow:auto;max-height:45vh' },
      el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, t('audit.col.time')), el('th', {}, t('audit.filter.tool')),
          el('th', {}, t('audit.col.path')), el('th', {}, t('audit.filter.result')))),
        el('tbody', {},
          tail.length ? tail.map(auditTailRow)
            : el('tr', {}, el('td', { colspan: String(TAIL_COLUMNS), class: 'dim' }, t('audit.empty')))))));

  close = openPanel({
    title: t('session.panel.title', s.client || id),
    content,
    width: 760,
    footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, finishBtn, done),
    onEscape: () => close(),
  });
  return close;
}
