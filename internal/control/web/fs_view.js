// The render page's decisions with no DOM (ui-plan G8): how a refused
// share (a 409 from POST /share with a code) is read into the phrase the
// page shows, and whether it may be retried with force.

// shareRefusal turns an ApiError into {key, force} or null when the error
// is not a share refusal. The daemon's body is JSON with a code.
export function shareRefusal(err) {
  if (!err || err.status !== 409) return null;
  let body = null;
  try { body = JSON.parse(err.message); } catch (_) { return null; }
  const codes = { not_synced: 'fs.share.not_synced', not_cached: 'fs.share.not_cached', unsupported: 'fs.share.unsupported', credentials: 'fs.share.credentials' };
  if (!body || !codes[body.code]) return null;
  return { key: codes[body.code], force: body.code === 'credentials' };
}
