// The decisions behind the embedding panel and the semantic search mode,
// with no DOM and no imports so node can run them (web/_tests/
// embedding_view.test.mjs). The input is the document GET /index/embedding
// serves (internal/control/embedding.go EmbeddingResponse): everything the
// daemon knows about the endpoint, minus the key, the reference to it, the
// base URL and the header it travels in. embedding_panel.js renders what
// these return; nothing here decides how it looks.

// bannerFor says whether the "content is sent to <host>" banner is shown.
// It is shown when the daemon says remote is true — a boolean, not a
// truthy value, because the banner is a privacy statement and a proxy or
// a typo must not switch it on or off. There is no way to close it.
export function bannerFor(st) {
  const remote = !!st && st.remote === true;
  if (!remote) return null;
  return { host: String(st.base_host || '') };
}

// healthOf is the health line: a dot (ok, warn while the last request
// failed, bad while the breaker holds requests back, off when there is no
// endpoint), the last error the daemon kept and the time the breaker
// opens again. Go serializes a zero time as year one; that is "not open".
export function healthOf(st) {
  if (!st || !st.enabled) return { dot: 'off', error: '', openUntil: '' };
  const raw = st.breaker_open_until ? String(st.breaker_open_until) : '';
  const openUntil = raw && !raw.startsWith('0001') ? raw : '';
  let dot = 'ok';
  if (!st.healthy) dot = openUntil ? 'bad' : 'warn';
  return { dot, error: String(st.last_error || ''), openUntil };
}

// progressOf is the embedded / pending / vectors of max line. capped is
// the daemon's word that max_chunks stopped the worker.
export function progressOf(st) {
  st = st || {};
  return {
    embedded: st.embedded || 0, pending: st.pending || 0,
    vectors: st.vectors || 0, max: st.max_chunks || 0, capped: st.capped === true,
  };
}

// estimateOf is the cost side. The daemon counts characters and chunks;
// it does not know the endpoint's tokenizer or price list, and neither
// does this page, so there is no price here — only the counts, the
// approximate tokens and the formula the reader applies to their own
// price. The panel says next to it that this is an estimate.
export function estimateOf(st) {
  st = st || {};
  const e = st.estimate || {};
  const chars = e.chars != null ? e.chars : (st.chars_this_month || 0);
  const tokens = e.tokens_approx != null ? e.tokens_approx : Math.floor(chars / 4);
  return { chars, tokens, corpusTokens: e.corpus_tokens_approx || 0, formula: String(e.formula || '') };
}

// AUTH_COMMAND is what the panel shows when no key is configured. The key
// itself is read by that command from a hidden prompt, a pipe or a file,
// and never by this page.
export const AUTH_COMMAND = 'cloudfs index auth';

// searchModeParam maps a search-box mode to the mode= value /index/search
// accepts: the keyword segment asks for keyword, the semantic one for
// hybrid (BM25 and cosine fused; the daemon runs keyword and says so in
// degraded when it has no embedder). The name search has no index mode.
export function searchModeParam(mode) {
  if (mode === 'semantic') return 'hybrid';
  if (mode === 'content') return 'keyword';
  return '';
}

// degradedReason is the daemon's explanation of why a semantic search ran
// as a keyword one, as a string for a text node; empty when it did not.
export function degradedReason(r) {
  if (!r || r.degraded == null || r.degraded === '') return '';
  return String(r.degraded);
}
