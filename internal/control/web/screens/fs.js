import { api } from '/ui/api.js';
import { el, fill, toast, copyBtn, confirmDelete, bytes, iconEl } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { fsPathFromHash } from '/ui/router.js';
import { mountProvenance } from '/ui/provenance.js';
import { openHistoryPanel } from '/ui/history_panel.js';
import { openSessionPanel } from '/ui/session_panel.js';
import { shareRefusal } from '/ui/fs_view.js';

// The render page (ui-plan G8-1, docs/agent-first-design.md §8.1): what
// http://<console>/#/fs/<path> opens — the link an agent's finish_session
// hands on and a hook's full context describes. It shows the file the
// way GET /fs/render describes it: Markdown as the daemon's own rendered
// HTML (renderMarkdown lets no markup from the source through, so the
// fragment goes in as HTML), text and code escaped in a <pre>, an image
// or a PDF from /fs/raw, anything else as a download. Above it the
// provenance rows the inspector shows, and the two actions of the
// sharing surface: copy the console link, create a public link (typed
// confirmation, then POST /share — the one irreversible action here).

export function renderFs(host) {
  let disposed = false;
  const path = fsPathFromHash(location.hash);
  const head = el('div', { class: 'pad' });
  const prov = el('div', { style: 'padding:0 20px' });
  const body = el('div', { style: 'padding:0 20px 20px' }, el('p', { class: 'dim' }, t('fs.loading')));
  fill(host, head, prov, body);
  if (!path || path === '/') {
    fill(head, el('div', { class: 'eyebrow' }, t('fs.eyebrow')), el('h2', {}, t('fs.none')));
    fill(body);
    return () => { disposed = true; };
  }

  async function share(r) {
    const name = path.split('/').pop();
    const ok = await confirmDelete({
      title: t('fs.share.title'), body: t('fs.share.body', path), confirmToken: name, confirmLabel: t('fs.share.create'),
    });
    if (!ok) return;
    let res;
    try {
      res = await api.post('/share', { path, confirm: true });
    } catch (err) {
      const why = shareRefusal(err);
      toast(why ? t(why.key) : err.message, 'bad');
      if (why && why.key === 'fs.share.credentials') {
        const force = await confirmDelete({ title: t('fs.share.force.title'), body: t('fs.share.force.body'), confirmToken: name, confirmLabel: t('fs.share.force') });
        if (!force) return;
        try { res = await api.post('/share', { path, confirm: true, force: true }); } catch (e2) { toast(e2.message, 'bad'); return; }
      } else {
        return;
      }
    }
    fill(shareOut,
      el('div', { class: 'banner', 'data-share-result': '', style: 'margin-top:10px' },
        el('div', { class: 'eyebrow' }, t('fs.share.done')),
        el('code', { class: 'detail', style: 'word-break:break-all' }, res.url),
        res.code ? el('div', { class: 'dim' }, t('fs.share.code', res.code)) : null,
        res.expires_at ? el('div', { class: 'dim' }, t('fs.share.expires', new Date(res.expires_at).toLocaleString())) : null,
        el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(res.url))));
  }

  async function lanLink() {
    let r;
    try { r = await api.post('/share/render-link', { path }); } catch (err) { toast(err.message, 'bad'); return; }
    fill(shareOut,
      el('div', { class: 'banner', 'data-render-link': '', style: 'margin-top:10px' },
        el('div', { class: 'eyebrow' }, t('fs.lan.done')),
        el('code', { class: 'detail', style: 'word-break:break-all' }, r.url),
        el('div', { class: 'dim' }, t('fs.lan.once', new Date(r.expires_at).toLocaleString())),
        el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(r.url))));
  }

  const shareOut = el('div', {});

  function content(r) {
    switch (r.kind) {
      case 'markdown': {
        // The fragment is the daemon's renderMarkdown output: every
        // character of the source was escaped there, so this is the one
        // place the console sets HTML it did not build itself.
        const doc = el('div', { class: 'rendered', 'data-rendered-markdown': '' });
        doc.innerHTML = r.html || '';
        return doc;
      }
      case 'text':
      case 'code':
        return el('pre', { class: 'detail snippet', 'data-rendered-text': r.kind, style: 'white-space:pre-wrap' }, r.text || '');
      case 'image':
        return el('img', { src: r.raw, alt: path.split('/').pop(), style: 'max-width:100%' });
      case 'pdf':
        return el('iframe', { src: r.raw, title: path.split('/').pop(), style: 'width:100%;height:75vh;border:1px solid var(--border);border-radius:6px', sandbox: '' });
      default:
        return el('p', { class: 'detail' }, t('fs.other', r.mime || ''));
    }
  }

  async function load() {
    let r;
    try {
      r = await api.get('/fs/render?path=' + encodeURIComponent(path));
    } catch (err) {
      if (disposed) return;
      fill(head, el('div', { class: 'eyebrow' }, t('fs.eyebrow')), el('h2', {}, path));
      fill(body, el('p', { class: 'detail' }, err.message));
      return;
    }
    if (disposed) return;
    fill(head,
      el('div', { class: 'eyebrow' }, t('fs.eyebrow')),
      el('h2', { style: 'overflow-wrap:anywhere' }, path.split('/').pop()),
      el('div', { class: 'dim', style: 'font-size:12px' }, path, ' · ', bytes(r.size || 0), r.local_only ? ' · ' + t('state.pending') : (r.cached < 1 ? ' · ' + t('fs.uncached', Math.round((r.cached || 0) * 100)) : '')),
      el('div', { class: 'row', style: 'margin-top:10px;gap:8px;flex-wrap:wrap' },
        r.console_url ? el('span', { class: 'row', style: 'gap:6px;align-items:center' }, el('span', { class: 'dim', style: 'font-size:12px' }, t('fs.link')), copyBtn(r.console_url)) : null,
        el('button', { 'data-share': '', disabled: !r.share, title: r.share ? '' : t('fs.share.unsupported'), onclick: () => share(r) }, iconEl('globe'), t('fs.share.create')),
        el('button', { 'data-lan-link': '', onclick: lanLink }, iconEl('cloud'), t('fs.lan.create')),
        el('button', { onclick: () => openHistoryPanel(path, { openSession: openSessionPanel }) }, iconEl('undo'), t('inspector.history')),
        el('a', { href: '#/connections?path=' + encodeURIComponent(path), style: 'font-size:13px' }, t('fs.infiles'))),
      shareOut);
    mountProvenance(prov, path, { api, openSession: openSessionPanel, openHistory: (p) => openHistoryPanel(p, { openSession: openSessionPanel }) });
    fill(body, r.truncated ? el('p', { class: 'dim', style: 'font-size:12px' }, t('fs.truncated')) : null, content(r));
  }
  load();
  return () => { disposed = true; };
}
