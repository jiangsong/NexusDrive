import { api } from '/ui/api.js';
import { el, fill, toast, openForm, confirmDelete } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { parsePrefixes, validateTokenScope } from '/ui/scope_view.js';
import { openTokenReveal } from '/ui/token_reveal.js';

// The tokens tab: every access token issued for the HTTP transport, by
// name and fingerprint, with what it may read and write and whether it is
// still live. GET /mcp/tokens never carries a token, only the four
// characters after the prefix, and that is all a row ever shows.
//
// Creating one is the one moment the plain token exists in the page: the
// reply to POST /mcp/tokens goes straight into the reveal panel and nothing
// here keeps it. Revoking one is typed-confirmed, because every agent
// holding that token loses access the moment it lands.
const COLUMNS = 8;
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;
const TTL = [['86400', 'tokens.ttl.1d'], ['604800', 'tokens.ttl.7d'], ['2592000', 'tokens.ttl.30d'], ['0', 'tokens.ttl.never']];

function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString(locale());
}

function stateCell(tok) {
  const dot = tok.state === 'active' ? 'ok' : tok.state === 'expired' ? 'warn' : 'bad';
  return el('span', { style: 'display:inline-flex;align-items:center;gap:7px' },
    el('span', { class: 'dot ' + dot }), t('tokens.state.' + tok.state));
}

// pathsCell lists prefixes one per line; an empty read list is the whole
// mount and a read-only token has nothing to write.
function pathsCell(list, emptyKey) {
  const paths = Array.isArray(list) ? list.filter((p) => typeof p === 'string' && p) : [];
  if (!paths.length) return el('td', { class: 'dim' }, t(emptyKey));
  return el('td', { class: 'detail', style: 'word-break:break-all' }, ...paths.map((p, i) => [i ? el('br') : null, p]));
}

export function renderTokensTab(host) {
  const rows = el('tbody');
  let disposed = false;
  let generation = 0;

  function tokenRow(tok) {
    const revoke = tok.state === 'active'
      ? el('button', { class: 'danger', onclick: () => revokeToken(tok) }, t('tokens.revoke'))
      : null;
    return el('tr', { 'data-token': tok.id },
      el('td', {}, tok.name),
      el('td', { class: 'detail tnums' }, el('code', {}, tok.fingerprint || '')),
      pathsCell(tok.read, 'scope.all'),
      tok.read_only ? el('td', { class: 'dim' }, t('scope.readonly')) : pathsCell(tok.write, 'tokens.write.none'),
      el('td', { class: 'detail tnums' }, tok.expires_at ? when(tok.expires_at) : t('tokens.expires.never')),
      el('td', { class: 'detail tnums' }, tok.last_used_at ? when(tok.last_used_at) : t('tokens.lastused.never')),
      el('td', {}, stateCell(tok)),
      el('td', { class: 'num' }, revoke));
  }

  async function load() {
    const mine = ++generation;
    try {
      const r = await api.get('/mcp/tokens');
      if (disposed || mine !== generation) return;
      fill(rows, ...(r.tokens || []).map(tokenRow));
      if (!rows.children.length) fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS), class: 'dim' }, t('tokens.empty'))));
    } catch (e) {
      if (disposed || mine !== generation) return;
      fill(rows, el('tr', {}, el('td', { colspan: String(COLUMNS) }, e.message)));
    }
  }

  async function createToken() {
    const name = el('input', { type: 'text', placeholder: 'codex', autocomplete: 'off', spellcheck: 'false' });
    const read = el('textarea', { rows: '3', placeholder: '/work', spellcheck: 'false' });
    const write = el('textarea', { rows: '3', placeholder: '/work/.agent', spellcheck: 'false' });
    const ttl = el('select', {}, ...TTL.map(([v, key]) => el('option', { value: v, selected: v === '604800' }, t(key))));
    const readOnly = el('input', { type: 'checkbox' });
    const ok = await openForm({
      title: t('tokens.new'),
      rows: [
        [t('tokens.form.name'), name], [t('tokens.form.read'), read], [t('tokens.form.write'), write],
        [t('tokens.form.ttl'), ttl], [t('tokens.form.readonly'), readOnly],
      ],
      note: el('div', { class: 'dim', style: 'font-size:11.5px' }, t('tokens.form.note')),
      confirmLabel: t('tokens.create'),
      validate: () => {
        if (!NAME_RE.test(name.value.trim())) { name.focus(); return t('tokens.err.name'); }
        // The daemon refuses the same shapes; checking here keeps the
        // mistake next to the field instead of in a toast.
        const key = validateTokenScope({ read: parsePrefixes(read.value), write: parsePrefixes(write.value) });
        if (key) (key === 'tokens.err.relative' ? read : write).focus();
        return key ? t(key) : '';
      },
    });
    if (!ok) return;
    const readList = parsePrefixes(read.value);
    // An empty write box means "the same as read"; the daemon reads null
    // that way, and an empty list as "nothing".
    const writeList = write.value.trim() ? parsePrefixes(write.value) : null;
    try {
      const r = await api.post('/mcp/tokens', {
        name: name.value.trim(), read: readList, write: writeList,
        read_only: readOnly.checked, ttl_seconds: Number(ttl.value),
      });
      toast(t('tokens.created', r.principal && r.principal.name ? r.principal.name : name.value.trim()));
      load();
      await openTokenReveal({ token: r.token, snippets: r.snippets || {}, addCommands: r.add_commands || {} });
    } catch (e) {
      toast(e.message, 'bad');
    }
  }

  async function revokeToken(tok) {
    const ok = await confirmDelete({
      title: t('tokens.revoke.title', tok.name), body: t('tokens.revoke.body'),
      confirmToken: tok.name, confirmLabel: t('tokens.revoke'),
    });
    if (!ok) return;
    try {
      await api.post('/mcp/tokens/' + encodeURIComponent(tok.id) + '/revoke', { confirm: true });
      toast(t('tokens.revoked', tok.name));
      load();
    } catch (e) {
      toast(e.message, 'bad');
    }
  }

  fill(host,
    el('div', { class: 'pad', style: 'display:flex;justify-content:space-between;align-items:center;gap:12px;padding-top:0;padding-bottom:12px' },
      el('div', { class: 'detail', style: 'font-size:13px' }, t('tokens.note')),
      el('div', { class: 'row', style: 'gap:8px;flex-shrink:0' },
        el('button', { onclick: () => load() }, t('action.refresh')),
        el('button', { class: 'primary', onclick: createToken }, t('tokens.new')))),
    el('div', { style: 'padding:0 20px 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {},
        el('th', {}, t('tokens.col.name')), el('th', {}, t('tokens.col.fingerprint')),
        el('th', {}, t('tokens.col.read')), el('th', {}, t('tokens.col.write')),
        el('th', {}, t('tokens.col.expires')), el('th', {}, t('tokens.col.lastused')),
        el('th', {}, t('tokens.col.state')), el('th', { class: 'num' }, ''))), rows))));

  load();
  return () => { disposed = true; };
}
