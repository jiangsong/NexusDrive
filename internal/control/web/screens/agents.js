import { el, fill } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { renderSessionsTab } from '/ui/screens/agents_sessions.js';
import { renderAuditTab } from '/ui/screens/agents_audit.js';

// The agents screen: what MCP clients are doing inside the mount. It is a
// tab container and nothing more — each tab is its own module with its own
// loads and its own event subscription, mounted when chosen and disposed
// when left. The chosen tab lives in the URL hash (#/agents?tab=audit) so a
// deep link and a reload land on the same table; switching tabs rewrites
// the hash in place rather than navigating, so the shell is not remounted
// for a change that only concerns this screen.
const TABS = ['sessions', 'audit', 'tokens', 'memory'];

// Tokens and memory arrive with later tasks; until then they are a short
// note, so the tab strip already has its final shape.
function renderPlaceholder(host) {
  fill(host, el('div', { class: 'pad dim' }, t('agents.tab.placeholder')));
  return () => {};
}

const RENDER = {
  sessions: renderSessionsTab,
  audit: renderAuditTab,
  tokens: renderPlaceholder,
  memory: renderPlaceholder,
};

// hashParams reads the query part of the hash, which is where this screen
// keeps its tab and any deep link (?session=<id>).
export function hashParams() {
  return new URLSearchParams((location.hash.split('?')[1]) || '');
}

function tabFromHash() {
  const wanted = hashParams().get('tab');
  return TABS.includes(wanted) ? wanted : 'sessions';
}

export function renderAgents(host) {
  let tab = tabFromHash();
  let dispose = null;
  const body = el('div', {});
  const bar = el('div', { class: 'tabs', role: 'tablist' });

  function show(name, params) {
    tab = name;
    if (dispose) { dispose(); dispose = null; }
    fill(bar, ...TABS.map((n) => el('button', {
      role: 'tab', 'aria-selected': String(n === tab), 'data-tab': n,
      onclick: () => {
        if (n === tab) return;
        // Only the tab survives a switch; a session deep link belongs to the
        // tab it opened on.
        history.replaceState(null, '', '#/agents?tab=' + n);
        show(n, new URLSearchParams());
      },
    }, t('agents.tab.' + n))));
    fill(body);
    dispose = (RENDER[name] || renderSessionsTab)(body, params || new URLSearchParams());
  }

  fill(host,
    el('div', { class: 'pad', style: 'padding-bottom:12px' },
      el('div', { class: 'eyebrow' }, t('agents.eyebrow')),
      el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('agents.title'))),
    bar, body);
  show(tab, hashParams());
  return () => { if (dispose) dispose(); };
}
