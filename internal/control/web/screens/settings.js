import { api } from '/ui/api.js';
import { el, fill, bytes, copyBtn } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { sections } from '/ui/settings_view.js';

// The settings screen (ui-plan G3, read-only by the 2026-09-17 decision):
// the agent-facing configuration as it is in force — MCP limits and
// transport, session retention, hooks, read heat, memory root, index —
// each section as rows with the YAML that would set it, to copy into the
// configuration file. Nothing here writes: the browser must not define
// what runs on this machine, and a setting takes effect on restart, so a
// form would promise more than it could keep. GET /settings is a
// whitelist and names no remote and no credential.

function valueText(r) {
  if (r.bytes) return bytes(r.value || 0);
  if (typeof r.value === 'boolean') return t(r.value ? 'settings.yes' : 'settings.no');
  return String(r.value ?? '');
}

function row(r) {
  return el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:8px', 'data-setting': r.key },
    el('span', { class: 'muted' }, t(r.key), r.builtin ? el('span', { class: 'dim' }, ' · ', t('settings.builtin')) : null),
    el('span', { class: 'detail', style: 'text-align:right;overflow-wrap:anywhere' }, valueText(r),
      r.hint ? el('div', { class: 'dim', style: 'font-size:12px' }, r.hint.arg !== undefined ? t(r.hint.key, r.hint.arg) : t(r.hint.key)) : null));
}

function section(s) {
  return el('div', { class: 'panel pad', 'data-section': s.id, style: 'margin-bottom:14px' },
    el('div', { class: 'eyebrow', style: 'margin-bottom:10px' }, t('settings.section.' + s.id)),
    ...s.rows.map(row),
    s.yaml ? el('div', { style: 'margin-top:10px' },
      el('pre', { class: 'detail snippet', style: 'white-space:pre' }, s.yaml),
      el('div', { class: 'row', style: 'margin-top:6px' }, copyBtn(s.yaml))) : null);
}

export function renderSettings(host) {
  let disposed = false;
  const body = el('div', { style: 'padding:0 20px 20px;max-width:760px' }, el('p', { class: 'dim' }, t('settings.loading')));
  fill(host,
    el('div', { class: 'pad' },
      el('div', { class: 'eyebrow' }, t('settings.eyebrow')),
      el('h2', {}, t('settings.title')),
      el('p', { class: 'detail', style: 'margin:6px 0 0' }, t('settings.readonly_note'))),
    body);
  api.get('/settings').then((v) => {
    if (disposed) return;
    fill(body, ...sections(v).map(section));
  }).catch((e) => {
    if (disposed) return;
    fill(body, el('p', { class: 'detail' }, e.message));
  });
  return () => { disposed = true; };
}
