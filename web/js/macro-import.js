// Pure helpers for the Macros pane: reading pasted macro JSON, saying what applying it would do, and filtering the list.
// The limits are the server's (internal/store/macros.go); checking them here lets the owner see a clear reason before saving.
export const MACRO_COUNT_MAX = 256;
export const MACRO_BODY_MAX = 16 * 1024;
const KEY = /^[a-z][a-z0-9_-]*$/;

// Accepts a plain {key: body} object, or an object with a "macros" member (a copy of a larger export).
// Entries that cannot be saved are counted, not fatal, so one bad row does not lose the rest.
export function parseMacroImport(text) {
  let data;
  try { data = JSON.parse(text); } catch { throw new Error('That is not valid JSON.'); }
  const wrapped = Boolean(data && typeof data === 'object' && !Array.isArray(data) && data.macros && typeof data.macros === 'object' && !Array.isArray(data.macros));
  const source = wrapped ? data.macros : data;
  if (!source || typeof source !== 'object' || Array.isArray(source)) throw new Error('Expected a JSON object of macro keys and bodies, like {"greeting": "Hello"}.');
  const macros = {}, invalid = [];
  for (const [rawKey, body] of Object.entries(source)) {
    const key = rawKey.trim().toLowerCase();
    if (!KEY.test(key)) invalid.push({ key: rawKey, reason: 'the key must start with a letter and use lowercase letters, digits, _ or -' });
    else if (typeof body !== 'string') invalid.push({ key: rawKey, reason: 'the body must be text' });
    else if (!body.trim()) invalid.push({ key: rawKey, reason: 'the body is empty' });
    else if (new TextEncoder().encode(body).length > MACRO_BODY_MAX) invalid.push({ key: rawKey, reason: 'the body is over 16 KiB' });
    else macros[key] = body;
  }
  return { macros, invalid, wrapped };
}

// What applying the import to the current macros would do: a matching key is overwritten, an identical one is left alone.
export function describeMerge(current, incoming) {
  let added = 0, updated = 0, unchanged = 0;
  for (const [key, body] of Object.entries(incoming)) {
    if (!Object.hasOwn(current, key)) added++;
    else if (current[key] !== body) updated++;
    else unchanged++;
  }
  const total = Object.keys(current).length + added;
  return { added, updated, unchanged, total, tooMany: total > MACRO_COUNT_MAX };
}

export function mergeMacros(current, incoming) { return { ...current, ...incoming }; }

// A case-insensitive match on the key or the body; blank shows everything.
export function filterMacroKeys(macros, query) {
  const needle = String(query || '').trim().toLowerCase();
  return Object.keys(macros).sort().filter(key => !needle || key.includes(needle) || macros[key].toLowerCase().includes(needle));
}
