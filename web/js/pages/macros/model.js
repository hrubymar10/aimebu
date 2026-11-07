// Import-free: scripts/test-macros-model.cjs runs this module in a vm. The rules themselves stay in macro-import.js; the hook passes in
// what they found.

export const label = key => `<${key}>`;
export const blankForm = () => ({ key: '', body: '' });
export const formDirty = (form, editing, macros) => (editing ? form.key !== editing || form.body !== macros[editing] : Boolean(form.key || form.body));

// The map a Save would send, or why it cannot be sent. A rename drops the old key in the same write.
export function saveChange(macros, editing, key, body, { validKey, withinSize }) {
  if (!validKey) return { error: 'Keys must start with a letter and contain lowercase letters, digits, _ or -.' };
  if (!withinSize) return { error: 'Macro body exceeds 16 KiB.' };
  if (Object.hasOwn(macros, key) && key !== editing) return { error: 'A macro with that key already exists.' };
  const next = { ...macros };
  if (editing && editing !== key) delete next[editing];
  next[key] = body;
  return { next };
}

export function removeChange(macros, key) {
  const next = { ...macros };
  delete next[key];
  return next;
}

export const EMPTY_IMPORT = { text: 'Paste macro JSON first.', error: true, pending: null };

// What applying a parsed import would do, from parseMacroImport and describeMerge: { text, error, pending }, pending only when Apply has
// something to send.
export function importSummary(parsed, merge) {
  const skipped = parsed.invalid.length ? ` Skipped ${parsed.invalid.length}: ${parsed.invalid.slice(0, 5).map(row => `${row.key} (${row.reason})`).join('; ')}${parsed.invalid.length > 5 ? '; …' : ''}.` : '';
  if (merge.tooMany) return { text: `Importing would leave ${merge.total} macros; the limit is 256.${skipped}`, error: true, pending: null };
  if (!merge.added && !merge.updated) return { text: `Nothing to change: ${merge.unchanged ? 'every macro in the import already exists unchanged' : 'no valid macros in the import'}.${skipped}`, error: false, pending: null };
  return { text: `Will add ${merge.added} new and overwrite ${merge.updated} existing (matched by key); ${merge.unchanged} already match.${skipped}`, error: false, pending: parsed.macros };
}
