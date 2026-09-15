import { api as defaultAPI } from '/ui/api.js';
import { el, fill, copyBtn } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { bannerFor, healthOf, progressOf, estimateOf, AUTH_COMMAND } from '/ui/embedding_view.js';

// The embedding endpoint panel of the index screen (docs/ui-plan.md F6,
// TODO.md T-39): what GET /index/embedding says about the endpoint chunk
// text is sent to, and the one button that spends a request on it.
//
// Three things about this panel are deliberate absences. There is no way
// to close the "content is sent to <host>" banner: it is a privacy
// statement, and a reader who dismissed it once would be the one person
// it was for. There is no input for the key: the key is set with
// `cloudfs index auth` in a terminal, and this page shows that command
// and nothing more. And there is no editing of provider or model: either
// change re-embeds the whole corpus, so the footer says to change them
// in the configuration file, where the cost is in front of the person
// making the change. The panel never carries the key or the URL; the
// daemon serves the host and a boolean.
//
// The "test endpoint" button is the only request besides the status
// load, and it is made on the click and never on mount, after a line
// that says the click makes one request.

function row(label, value) {
  return el('div', { class: 'row', style: 'gap:12px;align-items:baseline' },
    el('span', { class: 'muted', style: 'min-width:96px;font-size:12.5px' }, label),
    el('span', { style: 'font-size:13px;overflow-wrap:anywhere' }, value));
}

// remoteBanner is the yellow line that says content leaves this machine.
// It is a plain alert with the host in it and nothing else.
function remoteBanner(host) {
  return el('div', { class: 'banner warn', role: 'alert', 'data-remote-banner': '', style: 'margin-bottom:14px' },
    t('embedding.banner', host));
}

function when(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString(locale());
}

// checkEndpoint is the click: one POST /index/embedding/check, its outcome
// written next to the button. A failure is a normal answer of that route
// (ok:false with the error), so it lands in the same place; a transport
// error does too, because a toast would fade before it was read.
async function checkEndpoint(api, result, button) {
  button.disabled = true;
  fill(result, t('embedding.check.running'));
  try {
    const r = await api.post('/index/embedding/check', {});
    fill(result, r.ok
      ? el('span', {}, el('span', { class: 'dot ok' }), ' ' + t('embedding.check.ok', String(r.dim || 0), String(r.latency_ms || 0)))
      : el('span', { style: 'color:var(--warn-text)' }, el('span', { class: 'dot bad' }), ' ' + t('embedding.check.failed', String(r.error || ''))));
  } catch (err) {
    fill(result, el('span', { style: 'color:var(--warn-text)' }, el('span', { class: 'dot bad' }), ' ' + t('embedding.check.failed', err.message)));
  } finally {
    button.disabled = false;
  }
}

// render is the panel body for one status document.
function render(st, api) {
  const parts = [el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('embedding.title'))];
  if (!st.enabled) {
    parts.push(el('div', { class: 'detail' }, t('embedding.disabled')));
    parts.push(el('div', { class: 'dim', style: 'font-size:12px;margin-top:12px' }, t('embedding.footer')));
    return parts;
  }
  const banner = bannerFor(st);
  if (banner) parts.push(remoteBanner(banner.host));

  const health = healthOf(st);
  const healthLine = el('span', { class: 'row', style: 'gap:8px;flex-wrap:wrap', 'data-health': health.dot },
    el('span', { class: 'dot ' + health.dot }),
    el('span', {}, t(health.dot === 'ok' ? 'embedding.healthy' : 'embedding.unhealthy')),
    health.error ? el('span', { class: 'detail' }, '· ' + t('embedding.lasterror', health.error)) : null,
    health.openUntil ? el('span', { class: 'detail' }, '· ' + t('embedding.breaker', when(health.openUntil))) : null);

  const p = progressOf(st);
  const progressLine = el('span', { class: 'tnums' },
    t('embedding.progress', String(p.embedded), String(p.pending)),
    ' · ',
    t('embedding.vectors', String(p.vectors), String(p.max)),
    p.capped ? el('span', { class: 'chip', style: 'margin-left:8px', title: t('embedding.capped.title') }, t('embedding.capped')) : null);

  const e = estimateOf(st);
  const estimateLine = el('span', {},
    el('span', { class: 'tnums' }, t('embedding.chars', String(e.chars), String(e.tokens))),
    el('div', { class: 'detail tnums', style: 'margin-top:2px' }, t('embedding.estimate', String(e.corpusTokens), e.formula)),
    el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:2px' }, t('embedding.estimate.note')));

  parts.push(el('div', { style: 'display:grid;gap:8px' },
    row(t('embedding.provider'), st.provider || ''),
    row(t('embedding.model'), st.model || ''),
    row(t('embedding.dim'), st.dim ? String(st.dim) : t('embedding.dim.unknown')),
    row(t('embedding.host'), st.base_host || t('embedding.host.local')),
    row(t('embedding.health'), healthLine),
    row(t('embedding.progress.label'), progressLine),
    row(t('embedding.usage'), estimateLine)));

  // The check: the explanation, then the button, then where the result goes.
  const explain = el('div', { class: 'detail' }, t('embedding.check.explain'));
  const result = el('span', { class: 'detail', role: 'status', 'data-check-result': '' });
  const button = el('button', { style: 'padding:5px 12px', onclick: () => checkEndpoint(api, result, button) }, t('embedding.check'));
  parts.push(el('div', { style: 'margin-top:14px;display:grid;gap:8px' },
    explain, el('div', { class: 'row', style: 'gap:12px;flex-wrap:wrap' }, button, result)));

  if (st.api_key_configured === false) {
    parts.push(el('div', { style: 'margin-top:14px' },
      el('div', { class: 'detail', style: 'margin-bottom:8px' }, t('embedding.nokey')),
      el('div', { style: 'display:flex;gap:9px;align-items:flex-start' },
        el('code', { style: 'flex-grow:1;font-family:ui-monospace,monospace;font-size:12px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:9px;word-break:break-all' }, AUTH_COMMAND),
        copyBtn(AUTH_COMMAND))));
  }
  parts.push(el('div', { class: 'dim', style: 'font-size:12px;margin-top:14px' }, t('embedding.footer')));
  return parts;
}

// mountEmbeddingPanel fills root from GET /index/embedding and returns
// {refresh, dispose}. The api is injectable so a test can count requests;
// the screen passes nothing and gets the shared client.
export function mountEmbeddingPanel(root, { api = defaultAPI } = {}) {
  let disposed = false;
  const load = async () => {
    let st;
    try { st = await api.get('/index/embedding'); } catch (err) {
      if (!disposed) fill(root, el('div', { class: 'detail' }, t('embedding.load.failed', err.message)));
      return;
    }
    if (!disposed) fill(root, ...render(st, api));
  };
  load();
  return { refresh: load, dispose() { disposed = true; } };
}
