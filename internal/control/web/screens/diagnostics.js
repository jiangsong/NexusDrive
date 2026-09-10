import { api } from '/ui/api.js';
import { el, fill, iconEl, toast, confirmDelete } from '/ui/ui.js';
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
      fill(counts,
        el('span', { style: 'color:var(--ok)' }, el('span', { class: 'dot ok' }), ' ' + t('diag.count.ok') + ' ' + (r.ok || 0)),
        el('span', { style: 'color:var(--warn-text)' }, el('span', { class: 'dot warn' }), ' ' + t('diag.count.warn') + ' ' + (r.warn || 0)),
        el('span', { style: 'color:var(--danger-text)' }, el('span', { class: 'dot bad' }), ' ' + t('diag.count.fail') + ' ' + (r.fail || 0)));
      fill(list, ...(r.checks || []).map(checkRow));
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
    const ok = await confirmDelete({ title: t('diag.fix.title'), body: t('diag.fix.body'), confirmToken: 'fix', confirmLabel: t('diag.fix'), danger: false });
    if (!ok) return;
    try { const r = await api.post('/doctor/fix', { confirm: true }); toast((r.done || []).join('; ') || t('diag.fix.none')); run(); }
    catch (e) { toast(e.message, 'bad'); }
  }

  // The self-start service and the restart control. Both change the machine
  // beyond this session, so uninstall and restart go through the typed sheet.
  const svcPanel = el('div', { class: 'panel pad' });
  async function loadService() {
    let st;
    try { st = await api.get('/service/status'); }
    catch (e) { fill(svcPanel, el('div', { class: 'detail' }, e.message)); return; }
    const rows = [el('div', { class: 'eyebrow' }, t('diag.service'))];
    if (!st.supported) {
      rows.push(el('div', { class: 'detail', style: 'margin-top:6px' }, st.reason || t('diag.service.unsupported')));
      fill(svcPanel, ...rows); return;
    }
    const state = el('div', { style: 'display:flex;align-items:center;gap:9px;margin-top:8px' },
      el('span', { class: 'dot ' + (st.installed ? 'ok' : '') }),
      el('span', {}, st.installed ? t('diag.service.installed') : t('diag.service.absent')));
    const action = st.installed
      ? el('button', { class: 'danger', onclick: uninstallService }, t('diag.service.uninstall'))
      : el('button', { class: 'primary', onclick: installService }, t('diag.service.install'));
    rows.push(el('div', { style: 'display:flex;align-items:center;justify-content:space-between;gap:16px' }, state, action));
    rows.push(el('div', { class: 'dim', style: 'font-size:12px;margin-top:8px' }, t('diag.service.hint')));
    fill(svcPanel, ...rows);
  }
  async function installService() {
    const ok = await confirmDelete({ title: t('diag.service.install'), body: t('diag.service.hint'), confirmToken: 'install', confirmLabel: t('diag.service.install'), danger: false });
    if (!ok) return;
    try { await api.post('/service/install', {}); toast(t('diag.service.installed')); loadService(); }
    catch (e) { toast(e.message, 'bad'); }
  }
  async function uninstallService() {
    const ok = await confirmDelete({ title: t('diag.service.uninstall'), body: t('diag.service.hint'), confirmToken: 'uninstall', confirmLabel: t('diag.service.uninstall') });
    if (!ok) return;
    try { await api.post('/service/uninstall?confirm=true', {}); toast(t('diag.service.absent')); loadService(); }
    catch (e) { toast(e.message, 'bad'); }
  }

  const restartPanel = el('div', { class: 'panel pad' },
    el('div', { class: 'eyebrow' }, t('diag.daemon')),
    el('div', { style: 'display:flex;align-items:center;justify-content:space-between;gap:16px;margin-top:8px' },
      el('div', { class: 'detail', style: 'max-width:60ch' }, t('diag.restart.confirm')),
      el('button', { class: 'danger', style: 'flex-shrink:0', onclick: restartDaemon }, t('diag.restart'))));
  async function restartDaemon() {
    const ok = await confirmDelete({ title: t('diag.restart'), body: t('diag.restart.confirm'), confirmToken: 'restart', confirmLabel: t('diag.restart') });
    if (!ok) return;
    try {
      await api.post('/daemon/restart?confirm=true', {});
      toast(t('diag.restart.progress'));
    } catch (e) {
      // A dropped connection mid-restart is expected: the daemon closes its
      // listener as it goes down. Only a real refusal is worth a red toast.
      if (e.status) toast(e.message, 'bad');
      else toast(t('diag.restart.progress'));
    }
  }

  host.append(
    el('div', { class: 'pad', style: 'display:flex;align-items:end;justify-content:space-between' },
      el('div', {}, el('div', { class: 'eyebrow' }, t('diag.eyebrow')), el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('diag.title'))),
      el('button', { onclick: run }, iconEl('refresh'), t('diag.recheck'))),
    el('div', { style: 'padding:0 20px 12px' }, counts),
    el('div', { style: 'padding:0 20px 20px' }, list),
    el('div', { style: 'padding:0 20px 20px;display:grid;gap:16px' }, svcPanel, restartPanel));

  run();
  loadService();
  return () => {};
}
