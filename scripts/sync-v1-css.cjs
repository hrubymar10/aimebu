// web/v1.css is v1's stylesheet (~/xcode/aimebu/frontend/style.css) as it is, minus the rules of the two v1 features v2 does not have (fleets and the
// leaderboard). This script writes it, or with --check says whether the committed file is what it would write.
//   node scripts/sync-v1-css.cjs [path/to/v1/style.css]          write web/v1.css
//   node scripts/sync-v1-css.cjs --check [path/to/v1/style.css]  exit 1 when web/v1.css differs
// --final is the end state: no cascade layer, and v1's reset and html/body rules included.
// A rule is dropped when every selector of it names a removed feature; in a selector list only the removed selectors go. Nothing else is edited.
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const args = process.argv.slice(2);
const check = args.includes('--check');
const source = args.find(arg => !arg.startsWith('--')) || path.join(os.homedir(), 'xcode', 'aimebu', 'frontend', 'style.css');
const target = path.join(__dirname, '..', 'web', 'v1.css');
const REMOVED = /fleet|leaderboard/;
// While v2 still has areas that rely on the browser's default margins and its own page layout, v1's universal reset and its html/body rules (a flex
// column page, no scrolling) are left out; they come in with --final, when the last area has moved over.
// Classes v2's own markup already uses, whose v1 rules would change unmigrated areas where v2 sets nothing (v1 has the same name for another job): held
// back, per class, in scripts/v1-css-hold.json until their area moves over.
const HOLD_FILE = JSON.parse(fs.readFileSync(path.join(__dirname, 'v1-css-hold.json'), 'utf8'));
const HOLD = new Set(HOLD_FILE.classes);
// A generic state class (`active`) stays held for the areas that have not moved over, but a selector that also names a class of a moved area
// (`.settings-nav-item.active`) is that area's own rule: `released` maps the held class to the moved classes that release it.
const RELEASED = new Map(Object.entries(HOLD_FILE.released || {}).map(([name, owners]) => [name, new Set(owners)]));
const held = selector => {
  const names = [...selector.matchAll(/\.([A-Za-z_][\w-]*)/g)].map(match => match[1]);
  return names.some(name => HOLD.has(name) && !(RELEASED.has(name) && names.some(other => RELEASED.get(name).has(other))));
};
const TRANSITIONAL = new Set(['*', '*::before', '*::after', 'html', 'body']);
const final = args.includes('--final');

// Splits css into top-level items: { kind: 'text'|'rule'|'at', head, body }. text is whitespace and comments between rules.
function split(css) {
  const out = [];
  let i = 0, start = 0;
  const skipString = quote => { for (i++; i < css.length && css[i] !== quote; i++) if (css[i] === '\\') i++; };
  const skipComment = () => { const end = css.indexOf('*/', i + 2); i = end < 0 ? css.length : end + 1; };
  while (i < css.length) {
    // between rules: whitespace and comments
    const textStart = i;
    while (i < css.length) {
      if (/\s/.test(css[i])) i++;
      else if (css.startsWith('/*', i)) { skipComment(); i++; }
      else break;
    }
    if (i > textStart) out.push({ kind: 'text', text: css.slice(textStart, i) });
    if (i >= css.length) break;
    start = i;
    let head = '';
    for (; i < css.length && css[i] !== '{' && css[i] !== ';'; i++) {
      if (css[i] === '"' || css[i] === "'") skipString(css[i]);
      else if (css.startsWith('/*', i)) skipComment();
    }
    head = css.slice(start, i);
    if (css[i] === ';') { out.push({ kind: 'text', text: css.slice(start, i + 1) }); i++; continue; }
    const bodyStart = ++i;
    for (let depth = 1; i < css.length && depth > 0; i++) {
      if (css[i] === '"' || css[i] === "'") skipString(css[i]);
      else if (css.startsWith('/*', i)) skipComment();
      else if (css[i] === '{') depth++;
      else if (css[i] === '}') { depth--; if (depth === 0) break; }
    }
    out.push({ kind: head.trim().startsWith('@') ? 'at' : 'rule', head, body: css.slice(bodyStart, i) });
    i++;
  }
  return out;
}

function filter(css) {
  let out = '';
  for (const item of split(css)) {
    if (item.kind === 'text') { out += item.text; continue; }
    if (item.kind === 'at') {
      if (/^@(media|supports|layer)\b/.test(item.head.trim())) {
        const inner = filter(item.body);
        if (inner.trim()) out += `${item.head}{${inner}}`;
      } else out += `${item.head}{${item.body}}`;
      continue;
    }
    const selectors = item.head.split(',').map(selector => selector.trim()).filter(Boolean);
    const kept = selectors.filter(selector => !REMOVED.test(selector) && (final || (!TRANSITIONAL.has(selector) && !held(selector))));
    if (!kept.length) continue;
    out += kept.length === selectors.length ? `${item.head}{${item.body}}` : `${kept.join(',\n')} {${item.body}}`;
  }
  return out;
}

const original = fs.readFileSync(source, 'utf8');
let filtered = filter(original).replace(/\n{3,}/g, '\n\n');
// A section comment left with nothing under it goes too.
filtered = filtered.replace(/\/\* ── (?:Fleets|Leaderboards?)[^*]*\*\/\s*/g, '');
const header = `/* v1's stylesheet, as it is. Source: aimebu/frontend/style.css, without the fleets and leaderboard rules v2 does not have${final ? '' : ',\n   and for now without its universal reset, its html/body rules and the rules of classes v2 still styles itself (TRANSITIONAL and v1-css-hold.json)'}.
   Written by scripts/sync-v1-css.cjs; do not edit it by hand: a fix belongs in v1, then \`node scripts/sync-v1-css.cjs\` brings it in. */\n`;
// In a cascade layer, so that every unlayered v2 rule still wins over it: v1 applies only where v2 sets nothing, and an area moves over when its
// v2 rules are deleted. The layer comes off when the last v2 rule that overrode v1 is gone (--final).
const layered = !final;
// The layer order is declared first: v2's base styles (v2-base: legacy style.css and the generic element and button look), then the v1-reset (v1's
// `* { margin: 0; padding: 0; box-sizing: border-box }`, applied in theme-v1.css only inside the areas that have moved over), then v1's own rules.
// A later layer beats an earlier one whatever the selectors, so v1's class rules always win over v2's base, and every unlayered v2 rule wins over all.
const order = '@layer v2-base, v1-reset, v1;\n\n';
const next = header + (layered ? `${order}@layer v1 {\n${filtered}\n}\n` : filtered);
if (check) {
  const have = fs.existsSync(target) ? fs.readFileSync(target, 'utf8') : '';
  if (have !== next) { console.error('web/v1.css is not what scripts/sync-v1-css.cjs writes from v1 (run it without --check)'); process.exit(1); }
  console.log('v1.css matches v1');
} else {
  fs.writeFileSync(target, next);
  console.log(`web/v1.css: ${original.split('\n').length} -> ${next.split('\n').length} lines`);
}
