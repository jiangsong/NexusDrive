import { api } from '/ui/api.js';
import { el, iconEl, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// Diagnostics: the same checks `cloudfs doctor` prints, and the fixes it can
// apply. Each check says what it found and what to do; only auto-fixable ones
// get a button.
export function renderDiagnostics(host) {
  const list = el('div', { class: 'panel', style: 'overflow:hidden' });
  const counts = el('div', { style: 'display:flex;gap:18px' });

  async function run() {
    try {
      const r = await api.post('/doctor/run', {});
      counts.replaceChildren(
        el('span', { style: 'color:var(--ok)' }, el('span', { class: 'dot ok' }), ' 正常 ' + (r.ok || 0)),
        el('span', { style: 'color:var(--warn-text)' }, el('span', { class: 'dot warn' }), ' 提醒 ' + (r.warn || 0)),
        el('span', { style: 'color:var(--danger-text)' }, el('span', { class: 'dot bad' }), ' 需处理 ' + (r.fail || 0)));
      list.replaceChildren(...(r.checks || []).map(checkRow));
    } catch (e) { toast(e.message, 'bad'); }
  }
  function checkRow(c) {
    const level = { ok: 'ok', warn: 'warn', fail: 'bad' }[c.level] || '';
    const bg = c.level === 'fail' ? 'var(--danger-bg)' : c.level === 'warn' ? '#16120b' : 'transparent';
    return el('div', { style: `display:flex;align-items:start;gap:13px;padding:15px 18px;border-bottom:1px solid var(--hairline);background:${bg}` },
      el('span', { class: 'dot ' + level, style: 'margin-top:6px' }),
      el('div', { style: 'flex-grow:1;min-width:0' },
        el('div', { style: 'font-weight:600' }, c.name),
        c.detail ? el('div', { class: 'detail', style: 'font-size:12.5px;margin-top:4px' }, c.detail) : null,
        c.fix ? el('div', { class: 'dim', style: 'font-size:12px;margin-top:5px;font-family:ui-monospace,monospace' }, c.fix) : null),
      c.fixable ? el('button', { class: 'primary', style: 'flex-shrink:0', onclick: fix }, t('diag.fix')) : null);
  }
  async function fix() {
    const ok = await confirmDelete({ title: '运行修复', body: 'doctor --fix 会跑日志恢复、清理旧完成上传、回收缓存。', confirmToken: 'fix', confirmLabel: t('diag.fix'), danger: false });
    if (!ok) return;
    try { const r = await api.post('/doctor/fix', { confirm: true }); toast((r.done || []).join('；') || '无需修复'); run(); }
    catch (e) { toast(e.message, 'bad'); }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
      el('div', {}, el('div', { class: 'eyebrow' }, '运维'), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('diag.title'))),
      el('button', { onclick: run }, iconEl('refresh'), t('diag.recheck'))),
    el('div', { style: 'padding:0 20px 12px' }, counts),
    el('div', { style: 'padding:0 20px 20px' }, list));

  run();
  return () => {};
}
