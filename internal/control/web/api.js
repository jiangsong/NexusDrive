// The one HTTP client. Every mutation carries X-CloudFS-Control: 1, which a
// cross-site form cannot set; a non-2xx becomes an ApiError whose message is
// the server's own text. Errors bubble to the toast host unless a caller
// catches them for inline display.
import { locale } from '/ui/i18n.js';

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

// withLang appends the chosen language to a request. fetch and EventSource
// both refuse to set Accept-Language, and the parameter is what lets the
// switch in the title bar beat the browser's own header on the daemon side.
function withLang(path) {
  return path + (path.includes('?') ? '&' : '?') + 'lang=' + encodeURIComponent(locale());
}

async function request(method, path, body) {
  const headers = {};
  let payload;
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  if (method !== 'GET') headers['X-CloudFS-Control'] = '1';
  const resp = await fetch(withLang(path), { method, headers, body: payload, cache: 'no-store' });
  if (!resp.ok) {
    const text = (await resp.text()).trim();
    throw new ApiError(resp.status, text || `HTTP ${resp.status}`);
  }
  const type = resp.headers.get('Content-Type') || '';
  if (type.includes('application/json')) return resp.json();
  return resp.text();
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b),
  patch: (p, b) => request('PATCH', p, b),
  put: (p, b) => request('PUT', p, b),
  del: (p, b) => request('DELETE', p, b),
};

// events subscribes to the /events SSE stream, with exponential backoff
// reconnect and a polling fallback for a viewer whose WebView drops SSE. The
// caller gets change, status, export, audit, session, index and trigger
// events; it does not have to know which transport delivered them. Audit,
// session, index and trigger events have no polling fallback: the tabs that
// show them reload on demand, and the index screen reads its progress from
// the status document.
export function events({ onStatus, onChange, onExport, onAudit, onSession, onIndex, onTrigger }) {
  let es, timer, backoff = 1000, stopped = false, pollTimer;
  const startPoll = () => {
    if (pollTimer) return;
    const tick = async () => {
      try { onStatus && onStatus(await api.get('/status')); } catch (_) {}
      // Export polling is only the degraded transport while EventSource is
      // unavailable. The normal path remains the daemon's one-second SSE.
      try { onExport && onExport(await api.get('/exports?limit=100')); } catch (_) {}
    };
    tick();
    pollTimer = setInterval(tick, 5000);
  };
  const stopPoll = () => { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } };
  const connect = () => {
    if (stopped) return;
    try { es = new EventSource(withLang('/events')); }
    catch (_) { startPoll(); return; }
    es.addEventListener('open', () => { backoff = 1000; stopPoll(); });
    es.addEventListener('status', (e) => { try { onStatus && onStatus(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('change', (e) => { try { onChange && onChange(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('export', (e) => { try { onExport && onExport(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('audit', (e) => { try { onAudit && onAudit(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('session', (e) => { try { onSession && onSession(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('index', (e) => { try { onIndex && onIndex(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('trigger', (e) => { try { onTrigger && onTrigger(JSON.parse(e.data)); } catch (_) {} });
    es.addEventListener('error', () => {
      es.close();
      startPoll();
      if (!stopped) timer = setTimeout(connect, Math.min(backoff *= 2, 30000));
    });
  };
  connect();
  return () => { stopped = true; if (es) es.close(); if (timer) clearTimeout(timer); stopPoll(); };
}
