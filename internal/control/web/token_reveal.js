import { el, openPanel, copyBtn } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The only place a plain access token is ever shown. The daemon hands it out
// once, in the reply to POST /mcp/tokens, and the tokens tab passes that
// reply straight here. The token is a parameter of this call and nothing
// else keeps it: this module imports no store and touches no Web Storage,
// and closing the panel blanks the nodes that held it before dropping the
// last reference. Whoever closes without copying gets to create another
// token, not to see this one again.
//
// snippets and addCommands come from the same reply: the registration
// snippets for Claude Code and Codex with the token already substituted, so
// a person can paste one straight into their client. Each block has its own
// copy button. Everything is inserted as text.

function block(label, text) {
  return el('div', { style: 'margin-top:14px' },
    el('div', { class: 'eyebrow', style: 'margin-bottom:6px' }, label),
    el('pre', { class: 'detail snippet' }, text),
    el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(text)));
}

// openTokenReveal opens the panel. It resolves when the panel is closed; by
// then the text nodes are empty.
export function openTokenReveal({ token, snippets = {}, addCommands = {} }) {
  return new Promise((resolve) => {
    const plain = el('code', { class: 'plain-token' }, token);
    const blocks = el('div', {},
      addCommands.claude ? block(t('tokens.reveal.claude.add'), addCommands.claude) : null,
      snippets.claude ? block(t('tokens.reveal.claude'), snippets.claude) : null,
      snippets.codex ? block(t('tokens.reveal.codex'), snippets.codex) : null);
    const content = el('div', {},
      el('p', { class: 'warn-text', role: 'alert', style: 'margin:0 0 12px' }, t('tokens.reveal.once')),
      el('div', { class: 'row', style: 'align-items:center;gap:10px' }, plain, copyBtn(token)),
      blocks);
    const done = el('button', { class: 'primary' }, t('tokens.reveal.close'));
    let close = null;
    // discard is the one way out. Emptying the nodes first means a panel
    // that lingers in a detached subtree, or a devtools reference to it,
    // holds nothing worth reading.
    const discard = () => {
      plain.textContent = '';
      blocks.textContent = '';
      if (close) close();
      resolve();
    };
    close = openPanel({
      title: t('tokens.reveal.title'), width: 620, content,
      footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, done),
      onEscape: discard,
      // The token is shown once. A click that misses the copy button must
      // not be the click that throws it away.
      backdrop: false,
    });
    done.addEventListener('click', discard);
  });
}
