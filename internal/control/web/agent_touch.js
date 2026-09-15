import { el, fill, iconEl } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';

// The inspector's "modified by an agent" line: whether some MCP session
// wrote the selected file within the preimage retention window, and which.
// main.js hands this module an empty host under the entry's other rows;
// the request, the wording and the click live here. The daemon answers
// GET /sessions?path=&since= from the session_ops index (docs/agent-roadmap.md
// §4.3), newest session first, so one row is the one to name.
//
// The client name and the time are agent and daemon data and go in as
// text nodes.

// RETENTION is how far back the marker looks: the default of
// mcp.session.retain, after which preimages are gone and a rollback has
// nothing to restore, so an older touch is history rather than an offer.
const RETENTION = 7 * 24 * 60 * 60 * 1000;

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

// mountAgentTouch asks which session last wrote path and, when one did,
// fills container with the marker; a path no session touched, or a daemon
// that cannot answer (no agent.db), leaves it empty. openSession receives
// the session id when the marker is clicked.
export async function mountAgentTouch(container, path, { api, openSession }) {
  const since = new Date(Date.now() - RETENTION).toISOString();
  let r;
  try {
    r = await api.get('/sessions?path=' + encodeURIComponent(path) + '&since=' + encodeURIComponent(since) + '&limit=1');
  } catch (_) {
    fill(container);
    return;
  }
  const s = (r.sessions || [])[0];
  if (!s) { fill(container); return; }
  const at = s.finished_at || s.last_seen_at || s.started_at;
  fill(container,
    el('div', { style: 'margin-bottom:10px' },
      el('button', { 'data-agent-touch': s.id, style: 'font-size:12px', onclick: () => openSession(s.id) },
        iconEl('bot'), t('inspector.agenttouch'), ' · ', s.client || s.id, ' · ', when(at))));
}
