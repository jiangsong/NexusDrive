// Every string the page shows goes through t(). The tables in i18n_zh.js and
// i18n_en.js are the whole translation story for the UI: one file to copy when
// a language is added, and a key that renders as itself when an entry is
// missing, so a gap is visible rather than blank. This file is only the
// loader: which language, and how a key becomes a sentence.
//
// The daemon has its own catalog for the sentences it writes (diagnostics,
// backend prompts, status warnings). The page tells it which language to use
// with the lang query parameter every request carries — fetch cannot set
// Accept-Language, and the parameter is also what makes a chosen language beat
// the browser's own header.
//
// The imports are relative rather than /ui/-absolute like the rest of the
// app: the catalog tests import this module under node, where there is no
// /ui/ to resolve against, and the browser resolves both spellings the same.
import { zh } from './i18n_zh.js';
import { en } from './i18n_en.js';

// Exported so the catalogs can be compared against each other: identical key
// sets, and the same number of %s per key. Nothing outside a test should read
// them — t() is the way in.
export const tables = { zh, en };

// supported answers "is this one of our two languages" the only way that is
// safe on a plain object: `tables[code]` is truthy for anything
// Object.prototype provides, so ?lang=constructor used to pass and end up in
// document.documentElement.lang and on every request's query string.
function supported(code) {
  return typeof code === 'string' && Object.prototype.hasOwnProperty.call(tables, code);
}

// LOCALES is what the language switch renders, in the order it offers them.
export const LOCALES = [
  { code: 'zh', label: '中文' },
  { code: 'en', label: 'English' },
];

const STORAGE_KEY = 'cloudfs.lang';

// detect prefers an explicit URL choice, then what the person chose here
// before, then what the browser asks for, and falls back to Chinese. Keeping
// the URL choice first also makes switching work when storage is unavailable.
function detect() {
  const explicit = new URLSearchParams(location.search).get('lang');
  if (supported(explicit)) return explicit;
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (supported(saved)) return saved;
  } catch (e) { /* private windows deny storage; the browser's language still works */ }
  const wanted = (navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language || '']);
  for (const tag of wanted) {
    const base = String(tag).toLowerCase().split('-')[0];
    if (supported(base)) return base;
  }
  return 'zh';
}

let current = detect();
if (document.documentElement) document.documentElement.lang = current === 'zh' ? 'zh-CN' : current;

// locale is the language every t() and every API request currently uses.
export function locale() { return current; }

// setLocale switches language and reloads. A reload rather than a re-render is
// deliberate: the daemon renders its own strings per request, so the page has
// to ask for them again anyway, and a reload cannot leave half the screen in
// the previous language.
export function setLocale(code) {
  if (!supported(code) || code === current) return;
  try { localStorage.setItem(STORAGE_KEY, code); } catch (e) { /* the URL below persists it across reload */ }
  current = code;
  const next = new URL(location.href);
  next.searchParams.set('lang', code);
  location.replace(next.toString());
}

export function t(key, ...args) {
  const table = tables[current] || zh;
  let s = table[key];
  if (s === undefined) s = zh[key];
  if (s === undefined) s = key;
  // One pass, with a function replacement. A string replacement is read for
  // $&, $`, $' and $1, and the arguments here are provider error text, remote
  // names and file paths — none of it written by us — so a path containing $'
  // used to eat the rest of the sentence it was being placed into. Replacing
  // once per argument had the same shape of problem one level up: an argument
  // that itself contained %s swallowed the argument after it.
  let i = 0;
  return s.replace(/%s/g, () => (i < args.length ? String(args[i++]) : '%s'));
}
