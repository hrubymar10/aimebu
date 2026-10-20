// Reading and merging pasted macro JSON is pure, so this runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'macro-import.js'), 'utf8');
const context = { TextEncoder };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { parseMacroImport, describeMerge, mergeMacros, filterMacroKeys, MACRO_COUNT_MAX };', context);
const { parseMacroImport, describeMerge, mergeMacros, filterMacroKeys, MACRO_COUNT_MAX } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

// A plain object, keys lower-cased and trimmed.
assert.deepEqual(plain(parseMacroImport('{"Greet": "Hello", " bye ": "Goodbye"}')), { macros: { greet: 'Hello', bye: 'Goodbye' }, invalid: [], wrapped: false });
// An object that carries a "macros" member is read from there.
assert.deepEqual(plain(parseMacroImport('{"version":1,"macros":{"a":"x"}}')), { macros: { a: 'x' }, invalid: [], wrapped: true });
// Bad rows are reported, the good ones survive.
const mixed = plain(parseMacroImport(JSON.stringify({ ok: 'fine', '9bad': 'x', empty: '  ', num: 5, big: 'x'.repeat(16 * 1024 + 1) })));
assert.deepEqual(plain(mixed.macros), { ok: 'fine' });
assert.deepEqual(mixed.invalid.map(row => row.key).sort(), ['9bad', 'big', 'empty', 'num']);
assert.match(mixed.invalid.find(row => row.key === 'big').reason, /16 KiB/);
// A body of exactly 16 KiB is allowed.
assert.deepEqual(plain(Object.keys(parseMacroImport(JSON.stringify({ edge: 'x'.repeat(16 * 1024) })).macros)), ['edge']);
// Not JSON, not an object.
assert.throws(() => parseMacroImport('nope'), /not valid JSON/);
assert.throws(() => parseMacroImport('[1,2]'), /JSON object/);
assert.throws(() => parseMacroImport('"text"'), /JSON object/);
assert.throws(() => parseMacroImport('null'), /JSON object/);

// Merge: add, overwrite, leave identical alone, count the limit.
const current = { a: '1', b: '2' };
assert.deepEqual(plain(describeMerge(current, { a: '1', b: 'changed', c: '3' })), { added: 1, updated: 1, unchanged: 1, total: 3, tooMany: false });
assert.deepEqual(plain(mergeMacros(current, { b: 'changed', c: '3' })), { a: '1', b: 'changed', c: '3' });
const many = Object.fromEntries(Array.from({ length: MACRO_COUNT_MAX }, (_, i) => [`m${i}`, 'x']));
assert.equal(describeMerge(many, { extra: 'x' }).tooMany, true);
assert.equal(describeMerge(many, { m1: 'y' }).tooMany, false);

// Filter: key or body, case-insensitive, sorted.
const list = { zeta: 'Review this', alpha: 'hello', beta: 'REVIEW again' };
assert.deepEqual(plain(filterMacroKeys(list, '')), ['alpha', 'beta', 'zeta']);
assert.deepEqual(plain(filterMacroKeys(list, 'review')), ['beta', 'zeta']);
assert.deepEqual(plain(filterMacroKeys(list, 'ALP')), ['alpha']);
assert.deepEqual(plain(filterMacroKeys(list, 'nothing')), []);
console.log('macro import tests passed');
