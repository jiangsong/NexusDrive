// What the "add rule" form on the index screen turns a choice into.
//
// The form offers three named sets of file types instead of a glob editor:
// a person picking "documents" should not have to know how the indexer
// spells a pattern. The daemon's own defaults are the source of the lists
// (internal/index rule matching), and the size parser accepts what the
// configuration file accepts, so a rule typed here reads the same as one
// written in YAML. No imports: node tests this module directly.
export const PRESETS = Object.freeze({
  docs: Object.freeze(['**/*.md', '**/*.txt', '**/*.pdf', '**/*.docx', '**/*.xlsx', '**/*.pptx']),
  code: Object.freeze(['**/*.go', '**/*.py', '**/*.ts', '**/*.js', '**/*.rs', '**/*.java', '**/*.c', '**/*.h', '**/*.sh', '**/*.sql']),
  text: Object.freeze(['**/*.md', '**/*.txt', '**/*.rst', '**/*.csv', '**/*.json', '**/*.yaml', '**/*.yml', '**/*.toml', '**/*.html']),
});

// PRESET_NAMES is the order the form offers them in.
export const PRESET_NAMES = Object.freeze(Object.keys(PRESETS));

// includeFor is the include list of a preset, as a fresh array: the form
// may edit what it gets back, and the preset must not change under it. An
// unknown name — including anything Object.prototype answers to — is an
// empty list, which the daemon reads as "its defaults".
export function includeFor(preset) {
  return Object.prototype.hasOwnProperty.call(PRESETS, preset) ? [...PRESETS[preset]] : [];
}

const UNITS = { '': 1, b: 1, kib: 1024, mib: 1024 ** 2, gib: 1024 ** 3, kb: 1000, mb: 1000 ** 2, gb: 1000 ** 3 };

// parseSize reads a size the way the configuration file does: a number
// with an optional binary (KiB, MiB, GiB) or decimal (KB, MB, GB) unit,
// spaces allowed in between. An empty field is 0, which the daemon treats
// as its default; anything it cannot read is NaN so the form can refuse
// it rather than send a limit the person did not mean.
export function parseSize(text) {
  const s = String(text || '').trim();
  if (!s) return 0;
  const m = /^(\d+(?:\.\d+)?)\s*([a-zA-Z]*)$/.exec(s);
  if (!m || !Object.prototype.hasOwnProperty.call(UNITS, m[2].toLowerCase())) return NaN;
  return Math.round(Number(m[1]) * UNITS[m[2].toLowerCase()]);
}
