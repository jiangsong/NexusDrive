// workspace_view.js decides what the agent workspace looks like from the
// file browser: which directory is the workspace itself, which are session
// directories, and which session a file belongs to. It imports nothing so
// its tests run under node without a DOM.
//
// The daemon lays sessions out as direct children of the workspace
// (<workspace>/<client>-<YYYYMMDD>-<sid[:8]>), so "session directory" is a
// question of depth, not of name: nothing here parses the directory name.

// clean canonicalises a mount path the way the daemon does: empty and dot
// segments vanish, a parent segment pops, and the result is absolute. Both
// sides of every comparison go through it so a trailing slash on the
// configured workspace cannot hide a mark.
function clean(p) {
  const parts = [];
  for (const s of String(p || '').split('/')) {
    if (!s || s === '.') continue;
    if (s === '..') parts.pop();
    else parts.push(s);
  }
  return '/' + parts.join('/');
}

// workspaceMark is 'root' for the workspace directory, 'session' for a
// direct child of it, and '' for everything else. A direct child that is a
// file also reads as 'session'; the file browser only draws the mark on
// directories.
export function workspaceMark(path, workspace) {
  if (!workspace) return '';
  const p = clean(path);
  const w = clean(workspace);
  if (p === w) return 'root';
  if (!p.startsWith(w + '/')) return '';
  return p.slice(w.length + 1).includes('/') ? '' : 'session';
}

// sessionDirOf is the session directory a path lies in, or '' when it is
// outside the workspace or is the workspace itself.
export function sessionDirOf(path, workspace) {
  if (!workspace) return '';
  const p = clean(path);
  const w = clean(workspace);
  if (!p.startsWith(w + '/')) return '';
  return w + '/' + p.slice(w.length + 1).split('/')[0];
}
