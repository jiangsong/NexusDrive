import { api } from '/ui/api.js';
import { el, fill, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The proxy page: outbounds, groups and the rule list. A change is saved and,
// when the daemon can apply it live, takes effect at once; otherwise the page
// is told a restart is needed.
export function renderProxy(host) {
  const outRows = el('tbody');
  const rulesList = el('div', { class: 'panel', style: 'overflow:auto' });
  const groupsBox = el('div', { class: 'stack' });

  async function load() {
    try {
      const cfg = await api.get('/proxy/config');
      const health = {};
      try { for (const h of await api.post('/proxy/check', {})) health[h.name] = h; } catch (_) {}
      fill(outRows, ...(cfg.outbounds || []).map((o) => {
        const h = health[o.name] || {};
        const color = h.healthy ? 'var(--ok)' : h.error ? 'var(--bad)' : 'var(--warn)';
        return el('tr', {},
          el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:9px' }, el('span', { class: 'dot', style: 'background:' + color }), o.name)),
          el('td', { class: 'muted' }, o.type),
          el('td', { class: 'dim', style: 'font-family:ui-monospace,monospace;font-size:12.5px' }, o.addr || '本机出口'),
          el('td', { class: 'num', style: 'color:' + color }, h.latency_ms != null ? h.latency_ms + ' ms' : '—'),
          el('td', { class: 'detail', style: 'padding-left:16px' }, h.error || (h.healthy ? '可用' : '未探测')));
      }));
      fill(groupsBox, ...(cfg.groups || []).map((g) => el('div', { class: 'panel pad' },
        el('div', { style: 'display:flex;align-items:center;gap:10px' }, el('span', { style: 'font-weight:620' }, g.name),
          el('span', { style: 'padding:2px 9px;border-radius:999px;background:#1d3350;color:var(--accent-text);font-size:11px' }, g.type)),
        el('div', { class: 'dim', style: 'font-size:12px;margin-top:8px' }, '成员：' + (g.members || []).join(' · ')))));
      if (!(cfg.groups || []).length) fill(groupsBox, el('div', { class: 'dim', style: 'padding:12px' }, t('empty')));
      fill(rulesList, ...(cfg.rules || []).map((r) => el('div', { style: 'display:flex;gap:11px;padding:10px 16px;border-bottom:1px solid var(--hairline);font-family:ui-monospace,monospace;font-size:12.5px' },
        el('span', { class: 'detail' }, r))),
        el('div', { class: 'dim', style: 'padding:10px 16px;font-size:12px' }, '默认出口：' + (cfg.default || 'direct')));
      if (!(cfg.rules || []).length) fill(rulesList, el('div', { class: 'dim', style: 'padding:12px' }, t('empty')));
    } catch (e) { toast(e.message, 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
      el('div', {}, el('div', { class: 'eyebrow' }, '网络'), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('proxy.title'))),
      el('button', { onclick: load }, t('proxy.recheck'))),
    el('div', { style: 'padding:0 20px' }, el('div', { class: 'panel', style: 'overflow:auto' },
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, t('proxy.outbounds')), el('th', {}, '类型'), el('th', {}, '地址'), el('th', { class: 'num' }, '延迟'), el('th', { style: 'padding-left:16px' }, '状态'))), outRows))),
    el('div', { style: 'display:grid;grid-template-columns:420px 1fr;gap:20px;padding:20px' },
      el('div', {}, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('proxy.groups')), groupsBox),
      el('div', {}, el('div', { class: 'eyebrow', style: 'margin-bottom:12px' }, t('proxy.rules')), rulesList)));

  load();
  return () => {};
}
