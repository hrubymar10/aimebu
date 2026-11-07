// The Macros page's pure rules (web/js/pages/macros/model.js), run in a vm. The macro-import.js rules the hook feeds it run in their own vm
// here, so the summaries are checked against what those rules really find.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.join(__dirname, '..', 'web', 'js');
const load = (file, names) => {
  const source = fs.readFileSync(path.join(web, file), 'utf8');
  const context = { TextEncoder };
  vm.createContext(context);
  vm.runInContext(`${source.replace(/^export /gm, '')}\nglobalThis.api = { ${names} };`, context);
  return { source, api: context.api };
};
const model = load('pages/macros/model.js', 'label, blankForm, formDirty, saveChange, removeChange, importSummary, EMPTY_IMPORT');
assert.ok(!/^import /m.test(model.source), 'model.js stays import-free so it can run here');
const m = model.api;
const rules = load('macro-import.js', 'parseMacroImport, describeMerge, isMacroKey, MACRO_BODY_MAX').api;
const plain = value => JSON.parse(JSON.stringify(value));
const macros = { greet: 'Hello', bye: 'Goodbye' };

assert.equal(m.label('greet'), '<greet>');

// Unsaved: any text in a new macro; a change from the stored key or body while editing.
assert.equal(m.formDirty(m.blankForm(), null, macros), false);
assert.equal(m.formDirty({ key: 'x', body: '' }, null, macros), true);
assert.equal(m.formDirty({ key: 'greet', body: 'Hello' }, 'greet', macros), false);
assert.equal(m.formDirty({ key: 'greet', body: 'Hi' }, 'greet', macros), true);

// Save, with the checks the hook makes through macro-import.js: a rename drops the old key in the same map; the texts are the legacy page's.
const ok = { validKey: true, withinSize: true };
assert.deepEqual(plain(m.saveChange(macros, null, 'new', 'x', ok)), { next: { greet: 'Hello', bye: 'Goodbye', new: 'x' } });
assert.deepEqual(plain(m.saveChange(macros, 'greet', 'hi', 'Hello', ok)), { next: { bye: 'Goodbye', hi: 'Hello' } });
assert.deepEqual(plain(m.saveChange(macros, 'greet', 'greet', 'Hey', ok)), { next: { greet: 'Hey', bye: 'Goodbye' } });
assert.match(m.saveChange(macros, null, '9x', 'x', { ...ok, validKey: false }).error, /start with a letter/);
assert.match(m.saveChange(macros, null, 'big', 'x', { ...ok, withinSize: false }).error, /exceeds 16 KiB/);
assert.match(m.saveChange(macros, null, 'bye', 'x', ok).error, /already exists/);
assert.match(m.saveChange(macros, 'greet', 'bye', 'x', ok).error, /already exists/, 'a rename onto another key is refused');
assert.equal(rules.isMacroKey('9x'), false);
assert.equal(new TextEncoder().encode('é'.repeat(8 * 1024 + 1)).length > rules.MACRO_BODY_MAX, true, 'the limit is in UTF-8 bytes');
assert.deepEqual(plain(m.removeChange(macros, 'greet')), { bye: 'Goodbye' });
assert.deepEqual(plain(macros), { greet: 'Hello', bye: 'Goodbye' }, 'the shown map is never changed in place');

// Preview: the legacy texts; only a preview with something to add or change leaves a pending import.
const summary = (current, text) => { const parsed = rules.parseMacroImport(text); return plain(m.importSummary(parsed, rules.describeMerge(current, parsed.macros))); };
assert.deepEqual(plain(m.EMPTY_IMPORT), { text: 'Paste macro JSON first.', error: true, pending: null });
const mixed = summary(macros, JSON.stringify({ greet: 'Changed', bye: 'Goodbye', alpha: 'A', '9bad': 'x' }));
assert.deepEqual(mixed.pending, { greet: 'Changed', bye: 'Goodbye', alpha: 'A' });
assert.match(mixed.text, /^Will add 1 new and overwrite 1 existing \(matched by key\); 1 already match\. Skipped 1: 9bad \(/);
assert.deepEqual(summary(macros, '{"greet":"Hello"}'), { text: 'Nothing to change: every macro in the import already exists unchanged.', error: false, pending: null });
assert.deepEqual(summary(macros, '{"9":"x"}'), { text: `Nothing to change: no valid macros in the import. Skipped 1: 9 (${rules.parseMacroImport('{"9":"x"}').invalid[0].reason}).`, error: false, pending: null });
const many = Object.fromEntries(Array.from({ length: 255 }, (_, index) => [`k${index}`, 'x']));
assert.deepEqual(summary(many, JSON.stringify({ a: 'x', b: 'y' })), { text: 'Importing would leave 257 macros; the limit is 256.', error: true, pending: null });
assert.match(summary({}, JSON.stringify(Object.fromEntries(Array.from({ length: 6 }, (_, index) => [`${index}x`, 'x'])))).text, /Skipped 6: .*; …\.$/, 'at most five skipped keys are named');

console.log('macros model tests passed');
