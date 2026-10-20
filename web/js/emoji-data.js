// The gemoji database (web/vendor/gemoji) and what the composer and the picker do with it: lookup by `:shortcode:`,
// search over names, aliases and tags, skin tones, and the remembered skin tone.
export const CATEGORIES = [
  { key: 'people', label: 'People', icon: '😀', sources: ['Smileys & Emotion', 'People & Body'] },
  { key: 'nature', label: 'Nature', icon: '🌿', sources: ['Animals & Nature'] },
  { key: 'food', label: 'Food', icon: '🍔', sources: ['Food & Drink'] },
  { key: 'travel', label: 'Travel', icon: '✈️', sources: ['Travel & Places'] },
  { key: 'activities', label: 'Activities', icon: '⚽', sources: ['Activities'] },
  { key: 'objects', label: 'Objects', icon: '💡', sources: ['Objects'] },
  { key: 'symbols', label: 'Symbols', icon: '🔣', sources: ['Symbols'] },
  { key: 'flags', label: 'Flags', icon: '🏁', sources: ['Flags'] },
];
export const SKIN_TONES = [
  { key: '', label: 'Default', swatch: '👋' }, { key: '\u{1F3FB}', label: 'Light', swatch: '👋🏻' }, { key: '\u{1F3FC}', label: 'Medium-light', swatch: '👋🏼' },
  { key: '\u{1F3FD}', label: 'Medium', swatch: '👋🏽' }, { key: '\u{1F3FE}', label: 'Medium-dark', swatch: '👋🏾' }, { key: '\u{1F3FF}', label: 'Dark', swatch: '👋🏿' },
];
const TONE_STORAGE = 'aimebu-emoji-skin-tone';
const VS16 = '️';

export function savedTone() {
  try { const tone = localStorage.getItem(TONE_STORAGE) || ''; return SKIN_TONES.some(entry => entry.key === tone) ? tone : ''; } catch { return ''; }
}
export function saveTone(tone) {
  try { if (tone) localStorage.setItem(TONE_STORAGE, tone); else localStorage.removeItem(TONE_STORAGE); } catch { /* Private browsing may deny storage. */ }
}
// Every person in the sequence takes the modifier right after their base code point (🧑🏽‍🤝‍🧑🏽, 👩🏽‍❤️‍💋‍👨🏽, 💏🏽), and a
// variation selector after a toned base is dropped (⛹🏽‍♂️). Code points that are not modifier bases (♂, ❤, the ZWJ) are untouched,
// and so is a base between two joiners: it is a thing the people share, not a person (the 🤝 in 🧑‍🤝‍🧑).
const MODIFIER_BASE = /\p{Emoji_Modifier_Base}/u;
const ZWJ = '\u200D';
export function withTone(entry, tone) {
  if (!tone || !entry.skin_tones) return entry.emoji;
  const points = [...entry.emoji];
  let out = '';
  for (let index = 0; index < points.length; index++) {
    out += points[index];
    if (MODIFIER_BASE.test(points[index]) && !(points[index - 1] === ZWJ && points[index + 1] === ZWJ)) { out += tone; if (points[index + 1] === VS16) index++; }
  }
  return out;
}

let loaded = null;
let database = null;
// Loads and indexes the database once. A failed load is not cached, so the next open retries.
export function loadEmoji() {
  loaded ||= fetch('/vendor/gemoji/emoji.json').then(response => { if (!response.ok) throw new Error(`emoji data: HTTP ${response.status}`); return response.json(); })
    .then(list => { database = buildDatabase(list); return database; })
    .catch(error => { loaded = null; throw error; });
  return loaded;
}
export function emojiLoaded() { return database; }

export function buildDatabase(list) {
  const byAlias = new Map();
  const categoryOf = new Map(CATEGORIES.flatMap(category => category.sources.map(source => [source, category.key])));
  const entries = list.map(item => ({ ...item, group: categoryOf.get(item.category) || 'symbols', name: String(item.description || item.aliases[0] || '') }));
  for (const entry of entries) for (const alias of entry.aliases) if (!byAlias.has(alias)) byAlias.set(alias, entry);
  return { entries, byAlias };
}

const norm = text => String(text).toLowerCase().replace(/[_\s-]+/g, ' ').trim();
// Ranks an entry for a query: an alias that starts with it first, then any alias containing it, then name words, then
// tags. Returns -1 for no match. Lower is better; the caller keeps the database order within a rank.
function rank(entry, query) {
  let best = -1;
  const take = value => { if (best < 0 || value < best) best = value; };
  for (const alias of entry.aliases) {
    const text = norm(alias);
    if (text === query) return 0;
    if (text.startsWith(query)) take(1); else if (text.includes(query)) take(3);
  }
  const name = norm(entry.name);
  if (name.startsWith(query)) take(2); else if (name.split(' ').some(word => word.startsWith(query))) take(4); else if (name.includes(query)) take(5);
  for (const tag of entry.tags) { const text = norm(tag); if (text.startsWith(query)) take(4); else if (text.includes(query)) take(6); }
  return best;
}
export function searchEmoji(db, query, limit = Infinity) {
  const text = norm(query);
  if (!text) return [];
  const ranked = [];
  db.entries.forEach((entry, index) => { const score = rank(entry, text); if (score >= 0) ranked.push({ entry, score, index }); });
  ranked.sort((a, b) => a.score - b.score || a.index - b.index);
  return ranked.slice(0, limit).map(item => item.entry);
}
// The alias to show for a search hit: the one that matched, else the first.
export function matchedAlias(entry, query) {
  const text = norm(query);
  return entry.aliases.find(alias => norm(alias).startsWith(text)) || entry.aliases.find(alias => norm(alias).includes(text)) || entry.aliases[0];
}

// True when `position` sits in an inline code span or a fenced block of `text`; `:` is left alone there.
export function insideCode(text, position) {
  const before = text.slice(0, position);
  const fences = before.split('```').length - 1;
  if (fences % 2) return true;
  const line = before.slice(before.lastIndexOf('\n') + 1).replaceAll('```', '');
  return (line.split('`').length - 1) % 2 === 1;
}

// Message text: a known `:shortcode:` (the ones the composer's ":" list offers) is shown as its emoji, except in code and links. The
// database loads on the first message that has something shortcode-shaped in it; roots seen before it arrives are done when it does.
const SHORTCODE = /(?<![A-Za-z0-9]):([a-z0-9_+-]+):(?![A-Za-z0-9])/g;
const waiting = new Set();
function replaceIn(root, db) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, { acceptNode: node => (node.parentElement?.closest('code, pre, a, button, textarea, .md-codeblock') ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT) });
  const nodes = []; for (let node = walker.nextNode(); node; node = walker.nextNode()) nodes.push(node);
  for (const node of nodes) {
    const text = node.nodeValue; SHORTCODE.lastIndex = 0;
    if (!SHORTCODE.test(text)) continue;
    const replaced = text.replace(SHORTCODE, (whole, alias) => { const entry = db.byAlias.get(alias); return entry ? entry.emoji : whole; });
    if (replaced !== text) node.nodeValue = replaced;
  }
}
export function expandShortcodes(root) {
  if (!/:[a-z0-9_+-]+:/.test(root.textContent)) return;
  if (database) { replaceIn(root, database); return; }
  waiting.add(root);
  loadEmoji().then(db => { for (const pending of waiting) if (pending.isConnected) replaceIn(pending, db); waiting.clear(); }).catch(() => { waiting.clear(); });
}
