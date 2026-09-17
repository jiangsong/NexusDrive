// The read-heat scatter (ui-plan G6-1): reads over staleness, one point per
// path, drawn as an SVG string from plain data. No DOM and no imports, so
// the geometry runs under node in _tests/heat_plot.test.mjs and the screen
// only sets innerHTML to what this returns — with every text escaped here,
// since a path is data the daemon relayed, not markup.
//
// x is how long since the file changed (days, log scale so a week and a
// year both fit), y is how often it was read (log scale too). The quadrant
// boundaries are the medians of the points shown, so "hot" and "stale"
// mean "more than the others on this screen", which is what a worklist
// needs. The radius grows with the log of the reads.

// quadrantOf names the quadrant of a point against the boundaries.
export function quadrantOf(p, bounds) {
  const hot = p.y >= bounds.y;
  const stale = p.x >= bounds.x;
  return (hot ? 'hot' : 'warm') + '_' + (stale ? 'stale' : 'fresh');
}

// median is the middle value of a list of numbers; 0 for none.
export function median(values) {
  const v = values.filter((n) => Number.isFinite(n)).sort((a, b) => a - b);
  if (!v.length) return 0;
  const mid = Math.floor(v.length / 2);
  return v.length % 2 ? v[mid] : (v[mid - 1] + v[mid]) / 2;
}

// bounds are the medians of x and y; a single point sits on both.
export function bounds(points) {
  return { x: median(points.map((p) => p.x)), y: median(points.map((p) => p.y)) };
}

// radius grows with the log of the reads: one read is the smallest dot,
// a thousand about four times as wide.
export function radius(reads) {
  const n = Math.max(1, Number(reads) || 0);
  return 3 + Math.log10(n) * 3;
}

// scale maps a value onto a pixel range on a log axis with a floor of 1
// (a file changed today, read once).
function scale(value, max, px0, px1) {
  const lo = 0;
  const hi = Math.log10(Math.max(2, max + 1));
  const v = Math.log10(Math.max(1, value) + 0);
  const f = hi === lo ? 0 : (v - lo) / (hi - lo);
  return px0 + (px1 - px0) * Math.max(0, Math.min(1, f));
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// KIND_CLASS is the CSS class of a point by its dominant kind of reader.
const KIND_CLASS = { agent: 'heat-agent', kernel: 'heat-kernel', console: 'heat-console', webdav: 'heat-webdav' };

// plot renders points ([{x, y, kind, path}], x in days, y in reads) into an
// SVG string of width × height. Each point is a <circle> with data-path
// and a <title> for hover; the quadrant lines and their labels come from
// the medians. Labels are the caller's words (already translated), given
// as {hot, stale}.
export function plot(points, { width = 640, height = 320, labels = {} } = {}) {
  const pts = (points || []).filter((p) => p && Number.isFinite(p.x) && Number.isFinite(p.y));
  const pad = { l: 36, r: 12, t: 12, b: 28 };
  const b = bounds(pts);
  const maxX = Math.max(1, ...pts.map((p) => p.x));
  const maxY = Math.max(1, ...pts.map((p) => p.y));
  const sx = (x) => scale(x, maxX, pad.l, width - pad.r);
  const sy = (y) => height - pad.b - (scale(y, maxY, 0, height - pad.t - pad.b));
  const out = [];
  out.push(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" class="heat-plot">`);
  if (!pts.length) {
    out.push(`<text x="${width / 2}" y="${height / 2}" text-anchor="middle" class="heat-empty">${esc(labels.empty || '')}</text></svg>`);
    return out.join('');
  }
  const bx = sx(b.x);
  const by = sy(b.y);
  out.push(`<line x1="${bx.toFixed(1)}" y1="${pad.t}" x2="${bx.toFixed(1)}" y2="${height - pad.b}" class="heat-bound"/>`);
  out.push(`<line x1="${pad.l}" y1="${by.toFixed(1)}" x2="${width - pad.r}" y2="${by.toFixed(1)}" class="heat-bound"/>`);
  out.push(`<text x="${width - pad.r}" y="${pad.t + 12}" text-anchor="end" class="heat-label">${esc(labels.hot || '')}</text>`);
  out.push(`<text x="${width - pad.r}" y="${height - pad.b - 6}" text-anchor="end" class="heat-label">${esc(labels.stale || '')}</text>`);
  for (const p of pts) {
    const q = quadrantOf(p, b);
    out.push(`<circle cx="${sx(p.x).toFixed(1)}" cy="${sy(p.y).toFixed(1)}" r="${radius(p.y).toFixed(1)}" class="heat-point ${KIND_CLASS[p.kind] || 'heat-other'}" data-path="${esc(p.path)}" data-quadrant="${q}"><title>${esc(p.path)} · ${esc(p.y)} · ${esc(Math.round(p.x))}d</title></circle>`);
  }
  out.push('</svg>');
  return out.join('');
}

// pointsOf turns GET /agent/heat entries into plot points at now: x is
// days since mtime (0 when meta has no mtime), y is reads, kind is the
// reader with the most reads.
export function pointsOf(entries, now) {
  const t0 = typeof now === 'number' ? now : Date.now();
  return (entries || []).map((e) => {
    const mt = e.mtime ? Date.parse(e.mtime) : NaN;
    const x = Number.isFinite(mt) ? Math.max(0, (t0 - mt) / 86400e3) : 0;
    let kind = '';
    let best = -1;
    for (const [k, n] of Object.entries(e.by_kind || {})) if (n > best) { best = n; kind = k; }
    return { x, y: Number(e.reads) || 0, kind, path: e.path };
  });
}
